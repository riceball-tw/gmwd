package usage

import (
	"encoding/json"
	"fmt"
	"time"
)

// RateLimitInfo mirrors rate_limit_info in a stream-json rate_limit_event and
// quotaLimits / apiErrorParams.rate_limit_info in the session logs.
type RateLimitInfo struct {
	Status         string                   `json:"status"`
	ResetsAt       int64                    `json:"resetsAt"`
	RateLimitType  string                   `json:"rateLimitType"`
	OverageStatus  string                   `json:"overageStatus"`
	IsUsingOverage bool                     `json:"isUsingOverage"`
	UnifiedWindows map[string]unifiedWindow `json:"unifiedWindows"`
}

type unifiedWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resetsAt"`
}

// ParseRateLimitEvent decodes one stream-json line of type rate_limit_event.
func ParseRateLimitEvent(line []byte, observedAt time.Time) (Snapshot, error) {
	var ev struct {
		Type          string        `json:"type"`
		RateLimitInfo RateLimitInfo `json:"rate_limit_info"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return Snapshot{}, err
	}
	if ev.Type != "rate_limit_event" {
		return Snapshot{}, fmt.Errorf("not a rate_limit_event: %q", ev.Type)
	}
	return ev.RateLimitInfo.Snapshot(observedAt), nil
}

// Snapshot converts the info into windows. Without unifiedWindows (as in
// rejected log records) only the window named by RateLimitType is filled, and
// a rejection means its utilization is 1.
func (r RateLimitInfo) Snapshot(observedAt time.Time) Snapshot {
	s := Snapshot{Status: r.Status}
	set := func(kind WindowKind, util float64, resets int64) {
		w := Window{Kind: kind, Utilization: util, Source: SourceRateLimitEvent, ObservedAt: observedAt}
		if resets > 0 {
			w.ResetsAt = time.Unix(resets, 0)
		}
		switch kind {
		case FiveHour:
			s.FiveHour = w
		case SevenDay:
			s.SevenDay = w
		}
	}
	for name, uw := range r.UnifiedWindows {
		set(WindowKind(name), uw.Utilization, uw.ResetsAt)
	}
	if len(r.UnifiedWindows) == 0 && r.Status == "rejected" {
		set(WindowKind(r.RateLimitType), 1, r.ResetsAt)
	}
	return s
}
