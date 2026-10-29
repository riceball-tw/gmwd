package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Prober runs `claude -p /usage`, which costs no quota (no model call).
type Prober struct {
	// Claude is the claude binary; "claude" when empty.
	Claude string
	// Dir is the working directory for the child. It should be a gmwd-owned
	// directory so no project settings or hooks apply.
	Dir string
}

// Probe returns the current usage as Claude Code reports it.
func (p Prober) Probe(ctx context.Context) (Snapshot, error) {
	bin := p.Claude
	if bin == "" {
		bin = "claude"
	}
	if p.Dir != "" {
		if err := os.MkdirAll(p.Dir, 0o700); err != nil {
			return Snapshot{}, err
		}
	}
	cmd := exec.CommandContext(ctx, bin, "-p", "/usage",
		"--output-format", "json",
		"--no-session-persistence",
		"--setting-sources", "project,local", // skip the user's hooks and plugins
		"--strict-mcp-config",
	)
	cmd.Dir = p.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return Snapshot{}, fmt.Errorf("claude /usage: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var res struct {
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return Snapshot{}, fmt.Errorf("claude /usage: decode: %w", err)
	}
	if res.IsError {
		return Snapshot{}, fmt.Errorf("claude /usage: %s", res.Result)
	}
	return ParseUsageText(res.Result, time.Now())
}

var (
	// Current session: 6% used · resets Oct 30 at 1:20am (Asia/Taipei)
	sessionRe = regexp.MustCompile(`(?m)^Current session:\s*(\d+(?:\.\d+)?)% used(?:\s*·\s*resets\s+(.+?))?\s*$`)
	// Current week (all models): 29% used · resets Nov 1 at 6pm (Asia/Taipei)
	weekRe = regexp.MustCompile(`(?m)^Current week \(([^)]+)\):\s*(\d+(?:\.\d+)?)% used(?:\s*·\s*resets\s+(.+?))?\s*$`)
)

// ParseUsageText parses the text that `/usage` prints. The highest weekly
// line is taken as SevenDay; any others go to Extra.
func ParseUsageText(text string, now time.Time) (Snapshot, error) {
	var s Snapshot
	m := sessionRe.FindStringSubmatch(text)
	if m == nil {
		return s, fmt.Errorf("usage text: no 'Current session' line")
	}
	w, err := usageWindow(FiveHour, m[1], m[2], now)
	if err != nil {
		return s, err
	}
	s.FiveHour = w

	weeks := weekRe.FindAllStringSubmatch(text, -1)
	if len(weeks) == 0 {
		return s, fmt.Errorf("usage text: no 'Current week' line")
	}
	binding := ""
	all := map[string]Window{}
	for _, wm := range weeks {
		w, err := usageWindow(SevenDay, wm[2], wm[3], now)
		if err != nil {
			return s, err
		}
		all[wm[1]] = w
		if binding == "" || w.Utilization > all[binding].Utilization {
			binding = wm[1]
		}
	}
	s.SevenDay = all[binding]
	delete(all, binding)
	if len(all) > 0 {
		s.Extra = all
	}
	return s, nil
}

func usageWindow(kind WindowKind, pct, resets string, now time.Time) (Window, error) {
	p, err := strconv.ParseFloat(pct, 64)
	if err != nil {
		return Window{}, err
	}
	w := Window{Kind: kind, Utilization: p / 100, Source: SourceUsageCommand, ObservedAt: now}
	if resets != "" {
		t, err := parseResetTime(resets, now)
		if err != nil {
			return Window{}, err
		}
		// /usage prints a minute early (1:19am for an exact 01:20:00 in
		// rate_limit_event); every observed reset is on a 10-minute mark.
		w.ResetsAt = t.Round(10 * time.Minute)
	}
	return w, nil
}

// "Oct 30 at 1:20am (Asia/Taipei)", "Nov 1 at 6pm (Asia/Taipei)", "7:10pm (Asia/Taipei)"
var resetRe = regexp.MustCompile(`^(?:([A-Z][a-z]{2}) (\d{1,2}) at )?(\d{1,2})(?::(\d{2}))?(am|pm)(?: \(([^)]+)\))?$`)

// parseResetTime parses the reset phrase from /usage. The year is not
// printed, so it picks the one that puts the time closest to now.
func parseResetTime(s string, now time.Time) (time.Time, error) {
	m := resetRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return time.Time{}, fmt.Errorf("usage text: unrecognised reset time %q", s)
	}
	loc := now.Location()
	if m[6] != "" {
		l, err := time.LoadLocation(m[6])
		if err != nil {
			return time.Time{}, fmt.Errorf("usage text: zone %q: %w", m[6], err)
		}
		loc = l
	}
	hour, _ := strconv.Atoi(m[3])
	minute := 0
	if m[4] != "" {
		minute, _ = strconv.Atoi(m[4])
	}
	hour %= 12
	if m[5] == "pm" {
		hour += 12
	}
	local := now.In(loc)
	if m[1] == "" {
		// Time only: the next occurrence.
		t := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
		if t.Before(local.Add(-time.Minute)) {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	}
	mon, err := time.Parse("Jan", m[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("usage text: month %q", m[1])
	}
	day, _ := strconv.Atoi(m[2])
	var best time.Time
	for _, y := range []int{local.Year() - 1, local.Year(), local.Year() + 1} {
		t := time.Date(y, mon.Month(), day, hour, minute, 0, 0, loc)
		if best.IsZero() || absDur(t.Sub(now)) < absDur(best.Sub(now)) {
			best = t
		}
	}
	return best, nil
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
