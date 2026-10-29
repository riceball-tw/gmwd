package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gmwd/internal/config"
	"gmwd/internal/rpc"
	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

type statusReport struct {
	// Daemon is nil when the daemon isn't running.
	Daemon     *rpc.StatusResult `json:"daemon,omitempty"`
	Snapshot   *usage.Snapshot   `json:"snapshot,omitempty"`
	ProbeError string            `json:"probeError,omitempty"`
	Window     usage.Tally       `json:"window"`
	Week       usage.Tally       `json:"week"`
}

func cmdStatus(args []string, out io.Writer) error {
	fs := newFlags("status")
	asJSON := fs.Bool("json", false, "print JSON")
	refresh := fs.Bool("refresh", false, "ask Claude Code for fresh usage instead of the daemon's last reading")
	noProbe := fs.Bool("no-probe", false, "don't run `claude /usage`; daemon state and local logs only")
	if err := fs.Parse(args); err != nil {
		return err
	}

	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var rep statusReport
	if c, err := dialDaemon(); err == nil {
		defer c.Close()
		if *refresh && !*noProbe {
			if err := c.Call(ctx, rpc.MethodUsageRefresh, nil, nil); err != nil {
				rep.ProbeError = err.Error()
			}
		}
		var st rpc.StatusResult
		if err := c.Call(ctx, rpc.MethodStatus, nil, &st); err != nil {
			return err
		}
		// Just started and not polled yet: read usage now (it's free).
		if st.Usage == nil && st.UsageError == "" && !*refresh && !*noProbe {
			if err := c.Call(ctx, rpc.MethodUsageRefresh, nil, nil); err != nil {
				rep.ProbeError = err.Error()
			}
			if err := c.Call(ctx, rpc.MethodStatus, nil, &st); err != nil {
				return err
			}
		}
		rep.Daemon, rep.Snapshot = &st, st.Usage
		if rep.ProbeError == "" {
			rep.ProbeError = st.UsageError
		}
	} else if !*noProbe {
		home, err := config.Home()
		if err != nil {
			return err
		}
		snap, err := usage.Prober{Claude: claudeBin, Dir: filepath.Join(home, "probe")}.Probe(ctx)
		if err != nil {
			rep.ProbeError = err.Error()
		} else {
			rep.Snapshot = &snap
		}
	}

	// Without usage, fall back to rolling windows.
	windowStart, weekStart := now.Add(-usage.FiveHourDuration), now.Add(-7*24*time.Hour)
	if s := rep.Snapshot; s != nil {
		if t := s.FiveHour.WindowStart(); !t.IsZero() {
			windowStart = t
		}
		if t := s.SevenDay.WindowStart(); !t.IsZero() {
			weekStart = t
		}
	}
	root, err := usage.DefaultLogRoot()
	if err != nil {
		return err
	}
	if rep.Week, err = usage.ScanLogs(root, weekStart); err != nil {
		return err
	}
	if rep.Window, err = usage.ScanLogs(root, windowStart); err != nil {
		return err
	}

	if *asJSON {
		return writeJSON(out, rep)
	}
	printStatus(out, rep, now)
	return nil
}

func printStatus(out io.Writer, rep statusReport, now time.Time) {
	if d := rep.Daemon; d != nil {
		state := "active"
		if d.Settings.Paused {
			state = "PAUSED"
		}
		fmt.Fprintf(out, "Daemon: running (pid %d, up %s), %s — %s\n", d.PID, round(now.Sub(d.StartedAt)), state, d.Reason)
		fmt.Fprintf(out, "  max burn %.0f%% of the week, reserve %.0f%% of each 5h window\n", d.Settings.MaxBurnPct, d.Settings.ReservePct)
		fmt.Fprintf(out, "  topics: %s\n", topicCounts(d.Topics))
		if j := d.Job; j != nil {
			fmt.Fprintf(out, "  job: %s, iteration %d, %s tokens, running %s\n", j.TopicID, j.Iteration,
				tokens(j.Tokens.Input+j.Tokens.Output+j.Tokens.CacheCreate), round(now.Sub(j.StartedAt)))
		}
		fmt.Fprintln(out)
	} else {
		fmt.Fprintln(out, "Daemon: not running (gmwd install, or gmwd daemon)")
		fmt.Fprintln(out)
	}

	if s := rep.Snapshot; s != nil {
		fmt.Fprintf(out, "Claude Code reports (%s, %s ago):\n", sourceName(s.FiveHour.Source), round(now.Sub(s.FiveHour.ObservedAt)))
		printWindow(out, "5h window", s.FiveHour, now)
		printWindow(out, "week", s.SevenDay, now)
		labels := make([]string, 0, len(s.Extra))
		for l := range s.Extra {
			labels = append(labels, l)
		}
		sort.Strings(labels)
		for _, l := range labels {
			printWindow(out, "week ("+l+")", s.Extra[l], now)
		}
	}
	if rep.ProbeError != "" {
		fmt.Fprintf(out, "Claude Code /usage unavailable: %s\n", rep.ProbeError)
	}
	if rep.Snapshot == nil {
		fmt.Fprintln(out, "Showing local estimates over rolling windows.")
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Local session logs (this machine only; claude.ai and other devices not included):")
	printTally(out, "5h window", rep.Window)
	printTally(out, "week", rep.Week)
	if len(rep.Week.ByModel) > 0 {
		models := make([]string, 0, len(rep.Week.ByModel))
		for m := range rep.Week.ByModel {
			models = append(models, m)
		}
		sort.Slice(models, func(i, j int) bool {
			return rep.Week.ByModel[models[i]].Total() > rep.Week.ByModel[models[j]].Total()
		})
		fmt.Fprintln(out, "  week by model:")
		for _, m := range models {
			c := rep.Week.ByModel[m]
			fmt.Fprintf(out, "    %-20s %8s  (in %s, out %s, cache write %s, cache read %s)\n",
				m, tokens(c.Total()), tokens(c.Input), tokens(c.Output), tokens(c.CacheCreate), tokens(c.CacheRead))
		}
	}
	if n := len(rep.Week.RateLimitHits); n > 0 {
		last := rep.Week.RateLimitHits[n-1]
		fmt.Fprintf(out, "  rate-limit hits this week: %d (last %s, %s)\n", n, last.At.Local().Format("Mon 15:04"), last.Info.RateLimitType)
	}
	if !rep.Week.LastInteractive.IsZero() {
		fmt.Fprintf(out, "  last interactive Claude Code activity: %s ago\n", round(now.Sub(rep.Week.LastInteractive)))
	}
}

func sourceName(s usage.Source) string {
	switch s {
	case usage.SourceUsageCommand:
		return "via /usage"
	case usage.SourceRateLimitEvent:
		return "from a job's rate-limit event"
	}
	return string(s)
}

func topicCounts(m map[topic.Status]int) string {
	var parts []string
	for _, st := range []topic.Status{topic.Running, topic.Queued, topic.Paused, topic.Done, topic.Failed} {
		if m[st] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", m[st], st))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func printWindow(out io.Writer, label string, w usage.Window, now time.Time) {
	reset := "reset unknown"
	if !w.ResetsAt.IsZero() {
		reset = fmt.Sprintf("resets %s (in %s)", w.ResetsAt.Local().Format("Mon Jan 2 15:04"), round(w.ResetsAt.Sub(now)))
	}
	fmt.Fprintf(out, "  %-12s %3.0f%% used  %s %s\n", label, w.Utilization*100, bar(w.Utilization, 20), reset)
}

func printTally(out io.Writer, label string, t usage.Tally) {
	c := t.Total()
	fmt.Fprintf(out, "  %-12s %8s tokens in %d requests (interactive %s, claude -p %s) since %s\n",
		label, tokens(c.Total()), c.Requests, tokens(t.Interactive.Total()), tokens(t.Headless.Total()),
		t.Since.Local().Format("Mon 15:04"))
}

func bar(frac float64, width int) string {
	n := int(frac*float64(width) + 0.5)
	n = min(max(n, 0), width)
	return strings.Repeat("■", n) + strings.Repeat("□", width-n)
}

func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func round(d time.Duration) string {
	if d < 0 {
		return "0m"
	}
	d = d.Round(time.Minute)
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	h, m := d/time.Hour, (d%time.Hour)/time.Minute
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
