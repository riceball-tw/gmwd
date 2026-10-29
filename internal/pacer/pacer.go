// Package pacer turns usage and the user's settings into a burn budget:
// whether gmwd may start a job now, and how much of the 5h window it may
// use. It is a pure function of its input.
package pacer

import (
	"fmt"
	"math"
	"sort"
	"time"

	"gmwd/internal/config"
	"gmwd/internal/usage"
)

const (
	// DefaultRatio is weekly-cap points per 5h-window point until usage
	// samples say otherwise. Observed on a Max plan: 13 window points moved
	// the week by 2.
	DefaultRatio = 0.15
	// MinBudget is the smallest budget worth starting a job for (5h points).
	MinBudget = 0.01
	// DefaultMaxUsageAge: older readings are too stale to plan with.
	DefaultMaxUsageAge = 30 * time.Minute
	// boostFrom is the share of a 5h window after which unused headroom
	// earns a bigger allowance, up to 2× the even share at the end.
	boostFrom = 0.6
	week      = 7 * 24 * time.Hour
)

// Input is everything a decision depends on.
type Input struct {
	Now      time.Time
	Usage    *usage.Snapshot // latest reading; nil if none
	Settings config.Settings
	// SpentThisWindow is how many 5h points gmwd's runs used in the current
	// 5h window (0..1).
	SpentThisWindow float64
	// Ratio is weekly points per 5h point; 0 means DefaultRatio.
	Ratio       float64
	MaxUsageAge time.Duration // 0 means DefaultMaxUsageAge
}

// Decision is the pacer's answer.
type Decision struct {
	CanRun bool `json:"canRun"`
	// BudgetPct is how many 5h-window points (0..1) the next job may use.
	BudgetPct float64 `json:"budgetPct"`
	Reason    string  `json:"reason"`
	// Until is when the situation is expected to change (a reset), if known.
	Until time.Time `json:"until,omitzero"`
}

// Decide applies the pacing rules.
func Decide(in Input) Decision {
	s := in.Settings
	if s.Paused {
		return Decision{Reason: "paused"}
	}
	if in.Usage == nil {
		return Decision{Reason: "waiting: usage not known yet"}
	}
	maxAge := in.MaxUsageAge
	if maxAge == 0 {
		maxAge = DefaultMaxUsageAge
	}
	five, seven := in.Usage.FiveHour, in.Usage.SevenDay
	now := in.Now

	// A window whose reset has passed starts from zero.
	fiveReset := !five.ResetsAt.IsZero() && !now.Before(five.ResetsAt)
	sevenReset := !seven.ResetsAt.IsZero() && !now.Before(seven.ResetsAt)
	u5, u7 := five.Utilization, seven.Utilization
	if fiveReset {
		u5 = 0
	}
	if sevenReset {
		u7 = 0
	}
	if in.Usage.Status == "rejected" && !fiveReset && !sevenReset {
		until := five.ResetsAt
		if seven.Utilization >= 1 {
			until = seven.ResetsAt
		}
		return Decision{Reason: "rate limited until " + clock(until), Until: until}
	}
	if now.Sub(five.ObservedAt) > maxAge {
		return Decision{Reason: fmt.Sprintf("waiting: last usage reading is %s old", round(now.Sub(five.ObservedAt)))}
	}

	// Weekly target and the even share for each 5h window left this week.
	target := s.MaxBurnPct / 100
	headroom := target - u7
	weekEnd := seven.ResetsAt
	if weekEnd.IsZero() || sevenReset {
		weekEnd = now.Add(week) // unknown: assume a whole week, the cautious choice
	}
	if headroom < 0.005 {
		return Decision{Reason: fmt.Sprintf("week at %s, target %s: done until %s", pct(u7), pct(target), clock(weekEnd)), Until: weekEnd}
	}
	windowsLeft := math.Max(1, math.Ceil(float64(weekEnd.Sub(now))/float64(usage.FiveHourDuration)))
	share := headroom / windowsLeft

	// Use-it-or-lose-it: late in a window, unused headroom expires anyway.
	windowEnd := now.Add(usage.FiveHourDuration) // a job now opens a window
	if !five.ResetsAt.IsZero() && !fiveReset {
		windowEnd = five.ResetsAt
		elapsed := 1 - float64(windowEnd.Sub(now))/float64(usage.FiveHourDuration)
		share *= 1 + clamp((elapsed-boostFrom)/(1-boostFrom), 0, 1)
	}
	share = math.Min(share, headroom)

	ratio := in.Ratio
	if ratio <= 0 {
		ratio = DefaultRatio
	}
	allowed := share/ratio - in.SpentThisWindow
	windowRoom := 1 - s.ReservePct/100 - u5
	budget := math.Min(allowed, windowRoom)
	if budget < MinBudget {
		if windowRoom <= allowed {
			return Decision{Reason: fmt.Sprintf("5h window at %s, keeping %s for you; resets %s", pct(u5), pct(s.ReservePct/100), clock(windowEnd)), Until: windowEnd}
		}
		if share/ratio < MinBudget {
			// Too little weekly headroom to be worth a job in any window.
			return Decision{Reason: fmt.Sprintf("week at %s of a %s target: %s left over %.0f windows is too little to start a job",
				pct(u7), pct(target), pct(headroom), windowsLeft), Until: weekEnd}
		}
		return Decision{Reason: fmt.Sprintf("this window's share is used (%s of %s); next window %s", pct(in.SpentThisWindow), pct(share/ratio), clock(windowEnd)), Until: windowEnd}
	}
	return Decision{
		CanRun:    true,
		BudgetPct: budget,
		Reason:    fmt.Sprintf("budget %s of the 5h window (≈%s of the week) until %s", pct(budget), pct(budget*ratio), clock(windowEnd)),
		Until:     windowEnd,
	}
}

// EstimateRatio estimates weekly points per 5h point from usage samples
// taken inside the same 5h and weekly windows. It returns DefaultRatio
// until at least minWindowPoints of 5h movement has been seen.
func EstimateRatio(samples []usage.Snapshot) float64 {
	const minWindowPoints = 0.2
	sorted := append([]usage.Snapshot(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].FiveHour.ObservedAt.Before(sorted[j].FiveHour.ObservedAt)
	})
	var d5, d7 float64
	for i := 1; i < len(sorted); i++ {
		a, b := sorted[i-1], sorted[i]
		if !a.FiveHour.ResetsAt.Equal(b.FiveHour.ResetsAt) || !a.SevenDay.ResetsAt.Equal(b.SevenDay.ResetsAt) {
			continue // a window reset in between
		}
		x5 := b.FiveHour.Utilization - a.FiveHour.Utilization
		x7 := b.SevenDay.Utilization - a.SevenDay.Utilization
		if x5 <= 0 || x7 < 0 {
			continue
		}
		d5 += x5
		d7 += x7
	}
	if d5 < minWindowPoints {
		return DefaultRatio
	}
	return clamp(d7/d5, 0.02, 1)
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

func clock(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format("Mon 15:04")
}

func round(d time.Duration) string { return d.Round(time.Minute).String() }
