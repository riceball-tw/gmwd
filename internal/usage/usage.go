// Package usage reports how much of the Claude subscription limits has been
// used and when each window resets.
//
// Sources, most trusted first (see docs/spike.md):
//   - rate_limit_event from `claude -p --output-format stream-json`
//     (2-decimal utilization, exact reset times; rides on real calls),
//   - `claude -p /usage` (free; integer percent, minute-precision resets),
//   - the local session logs (token counts only; misses other devices).
package usage

import "time"

// WindowKind names a rate-limit window.
type WindowKind string

const (
	FiveHour WindowKind = "five_hour"
	SevenDay WindowKind = "seven_day"
)

// Source says where a Window observation came from.
type Source string

const (
	SourceRateLimitEvent Source = "rate_limit_event"
	SourceUsageCommand   Source = "usage_command"
)

// Window is one observation of a rate-limit window.
type Window struct {
	Kind WindowKind `json:"kind"`
	// Utilization is the used share of the cap, 0..1.
	Utilization float64 `json:"utilization"`
	// ResetsAt is zero when unknown (e.g. no 5h window is active).
	ResetsAt   time.Time `json:"resetsAt"`
	Source     Source    `json:"source"`
	ObservedAt time.Time `json:"observedAt"`
}

// Remaining is the unused share of the cap, 0..1.
func (w Window) Remaining() float64 {
	r := 1 - w.Utilization
	if r < 0 {
		return 0
	}
	return r
}

// Snapshot is the state of both windows at one moment.
type Snapshot struct {
	FiveHour Window `json:"fiveHour"`
	SevenDay Window `json:"sevenDay"`
	// Status is "allowed" or "rejected" when known from a rate_limit_event.
	Status string `json:"status,omitempty"`
	// Extra holds additional weekly caps (e.g. a per-model week) keyed by
	// their label, when Claude Code reports them. SevenDay is the binding one.
	Extra map[string]Window `json:"extra,omitempty"`
}

// FiveHourDuration is the length of the short window.
const FiveHourDuration = 5 * time.Hour

// WindowStart returns when the window began, derived from its reset time.
func (w Window) WindowStart() time.Time {
	if w.ResetsAt.IsZero() {
		return time.Time{}
	}
	switch w.Kind {
	case FiveHour:
		return w.ResetsAt.Add(-FiveHourDuration)
	case SevenDay:
		return w.ResetsAt.Add(-7 * 24 * time.Hour)
	}
	return time.Time{}
}
