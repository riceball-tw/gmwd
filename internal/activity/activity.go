// Package activity decides whether the user is idle, so gmwd only works
// while they aren't using Claude (or, in away mode, the computer).
package activity

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	"gmwd/internal/config"
	"gmwd/internal/usage"
)

// State is the outcome of one check.
type State struct {
	Idle   bool   `json:"idle"`
	Reason string `json:"reason"`
	// LastClaude is the newest interactive Claude Code activity seen, if
	// any within the quiet period.
	LastClaude time.Time `json:"lastClaude,omitzero"`
}

// Detector checks for user activity.
type Detector struct {
	// LogRoot is ~/.claude/projects.
	LogRoot string
	// HIDIdle returns time since the last keyboard/mouse input; nil uses
	// ioreg. Only consulted in away mode.
	HIDIdle func(ctx context.Context) (time.Duration, error)
	Now     func() time.Time
}

func (d *Detector) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Check reports whether the user is idle under the given settings.
func (d *Detector) Check(ctx context.Context, s config.Settings) (State, error) {
	if s.QuietMinutes == 0 {
		return State{Idle: true, Reason: "idle detection off"}, nil
	}
	now := d.now()
	quiet := time.Duration(s.QuietMinutes) * time.Minute
	last, err := usage.LastInteractive(d.LogRoot, now.Add(-quiet))
	if err != nil {
		return State{Reason: "can't read Claude Code logs"}, err
	}
	if !last.IsZero() && now.Sub(last) < quiet {
		return State{LastClaude: last, Reason: fmt.Sprintf("user active: Claude Code used %s ago", ago(now.Sub(last)))}, nil
	}
	st := State{Idle: true, Reason: fmt.Sprintf("idle: no Claude Code use for %dm", s.QuietMinutes)}
	if s.IdleMode != config.IdleAway {
		return st, nil
	}
	hid := d.HIDIdle
	if hid == nil {
		hid = SystemHIDIdle
	}
	idle, err := hid(ctx)
	if err != nil {
		// Can't tell: assume the user is there.
		return State{Reason: "can't read keyboard idle time"}, err
	}
	if idle < quiet {
		return State{Reason: fmt.Sprintf("user active: input %s ago", ago(idle))}, nil
	}
	st.Reason = fmt.Sprintf("idle: away for %s", ago(idle))
	return st, nil
}

func ago(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

var hidRe = regexp.MustCompile(`"HIDIdleTime" = (\d+)`)

// SystemHIDIdle reads macOS's time since last input from ioreg.
func SystemHIDIdle(ctx context.Context) (time.Duration, error) {
	out, err := exec.CommandContext(ctx, "ioreg", "-c", "IOHIDSystem", "-d", "4").Output()
	if err != nil {
		return 0, fmt.Errorf("ioreg: %w", err)
	}
	return parseHIDIdle(out)
}

func parseHIDIdle(out []byte) (time.Duration, error) {
	m := hidRe.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("ioreg: no HIDIdleTime in %d bytes", len(bytes.TrimSpace(out)))
	}
	ns, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(ns), nil
}
