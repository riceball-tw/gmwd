package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gmwd/internal/usage"
)

type statusReport struct {
	Snapshot   *usage.Snapshot `json:"snapshot,omitempty"`
	ProbeError string          `json:"probeError,omitempty"`
	Window     usage.Tally     `json:"window"`
	Week       usage.Tally     `json:"week"`
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	noProbe := fs.Bool("no-probe", false, "skip `claude /usage`; estimate from local logs only")
	fs.Parse(args)

	now := time.Now()
	var rep statusReport
	if !*noProbe {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		snap, err := usage.Prober{Dir: filepath.Join(home, ".gmwd", "probe")}.Probe(ctx)
		if err != nil {
			rep.ProbeError = err.Error()
		} else {
			rep.Snapshot = &snap
		}
	}

	// Without a probe, fall back to rolling windows.
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
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printStatus(rep, now)
	return nil
}

func printStatus(rep statusReport, now time.Time) {
	if s := rep.Snapshot; s != nil {
		fmt.Printf("Claude Code reports (via /usage, %s):\n", s.FiveHour.ObservedAt.Format("15:04"))
		printWindow("5h window", s.FiveHour, now)
		printWindow("week", s.SevenDay, now)
		labels := make([]string, 0, len(s.Extra))
		for l := range s.Extra {
			labels = append(labels, l)
		}
		sort.Strings(labels)
		for _, l := range labels {
			printWindow("week ("+l+")", s.Extra[l], now)
		}
	} else if rep.ProbeError != "" {
		fmt.Printf("Claude Code /usage unavailable: %s\n", rep.ProbeError)
		fmt.Println("Showing local estimates over rolling windows.")
	}

	fmt.Println()
	fmt.Println("Local session logs (this machine only; claude.ai and other devices not included):")
	printTally("5h window", rep.Window)
	printTally("week", rep.Week)
	if len(rep.Week.ByModel) > 0 {
		models := make([]string, 0, len(rep.Week.ByModel))
		for m := range rep.Week.ByModel {
			models = append(models, m)
		}
		sort.Slice(models, func(i, j int) bool {
			return rep.Week.ByModel[models[i]].Total() > rep.Week.ByModel[models[j]].Total()
		})
		fmt.Println("  week by model:")
		for _, m := range models {
			c := rep.Week.ByModel[m]
			fmt.Printf("    %-20s %8s  (in %s, out %s, cache write %s, cache read %s)\n",
				m, tokens(c.Total()), tokens(c.Input), tokens(c.Output), tokens(c.CacheCreate), tokens(c.CacheRead))
		}
	}
	if n := len(rep.Week.RateLimitHits); n > 0 {
		last := rep.Week.RateLimitHits[n-1]
		fmt.Printf("  rate-limit hits this week: %d (last %s, %s)\n", n, last.At.Local().Format("Mon 15:04"), last.Info.RateLimitType)
	}
	if !rep.Week.LastInteractive.IsZero() {
		fmt.Printf("  last interactive Claude Code activity: %s ago\n", round(now.Sub(rep.Week.LastInteractive)))
	}
}

func printWindow(label string, w usage.Window, now time.Time) {
	reset := "reset unknown"
	if !w.ResetsAt.IsZero() {
		reset = fmt.Sprintf("resets %s (in %s)", w.ResetsAt.Local().Format("Mon Jan 2 15:04"), round(w.ResetsAt.Sub(now)))
	}
	fmt.Printf("  %-12s %3.0f%% used  %s %s\n", label, w.Utilization*100, bar(w.Utilization, 20), reset)
}

func printTally(label string, t usage.Tally) {
	c := t.Total()
	fmt.Printf("  %-12s %8s tokens in %d requests (interactive %s, claude -p %s) since %s\n",
		label, tokens(c.Total()), c.Requests, tokens(t.Interactive.Total()), tokens(t.Headless.Total()),
		t.Since.Local().Format("Mon 15:04"))
}

func bar(frac float64, width int) string {
	n := int(frac*float64(width) + 0.5)
	n = min(max(n, 0), width)
	b := make([]rune, width)
	for i := range b {
		if i < n {
			b[i] = '■'
		} else {
			b[i] = '□'
		}
	}
	return string(b)
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
