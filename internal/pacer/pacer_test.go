package pacer

import (
	"math"
	"strings"
	"testing"
	"time"

	"gmwd/internal/config"
	"gmwd/internal/usage"
)

var now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.Local)

// base: week 30% used, resets in 50h (10 windows left); 5h window 10% used,
// resets in 4h. Target 90% → 60 points of headroom → 6 per window → 40
// window points at ratio 0.15. Reserve leaves 75 points of window room.
func base() Input {
	return Input{
		Now:      now,
		Settings: config.DefaultSettings(),
		Usage: &usage.Snapshot{
			Status:   "allowed",
			FiveHour: usage.Window{Kind: usage.FiveHour, Utilization: 0.10, ResetsAt: now.Add(4 * time.Hour), ObservedAt: now.Add(-time.Minute)},
			SevenDay: usage.Window{Kind: usage.SevenDay, Utilization: 0.30, ResetsAt: now.Add(50 * time.Hour), ObservedAt: now.Add(-time.Minute)},
		},
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestDecide(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(*Input)
		run    bool
		budget float64
		reason string
	}{
		{"even share", func(*Input) {}, true, 0.40, "budget 40% of the 5h window (≈6% of the week) until"},
		{"paused", func(in *Input) { in.Settings.Paused = true }, false, 0, "paused"},
		{"no usage", func(in *Input) { in.Usage = nil }, false, 0, "waiting: usage not known yet"},
		{"stale usage", func(in *Input) { in.Usage.FiveHour.ObservedAt = now.Add(-31 * time.Minute) }, false, 0, "waiting: last usage reading is 31m0s old"},
		{"rate limited", func(in *Input) { in.Usage.Status = "rejected"; in.Usage.FiveHour.Utilization = 1 }, false, 0, "rate limited until"},
		{"rate limit over", func(in *Input) {
			in.Usage.Status = "rejected"
			in.Usage.FiveHour.Utilization = 1
			in.Usage.FiveHour.ResetsAt = now.Add(-time.Minute)
		}, true, 0.40, "budget 40%"},
		{"week at target", func(in *Input) { in.Usage.SevenDay.Utilization = 0.90 }, false, 0, "week at 90%, target 90%: done until"},
		{"slider lowered below usage", func(in *Input) { in.Settings.MaxBurnPct = 20 }, false, 0, "week at 30%, target 20%"},
		{"reserve caps budget", func(in *Input) { in.Usage.FiveHour.Utilization = 0.80 }, true, 0.05, "budget 5%"},
		{"reserve reached", func(in *Input) { in.Usage.FiveHour.Utilization = 0.845 }, false, 0, "5h window at 84%, keeping 15% for you"},
		{"bigger reserve", func(in *Input) { in.Settings.ReservePct = 60 }, true, 0.30, "budget 30%"},
		{"window share spent", func(in *Input) { in.SpentThisWindow = 0.395 }, false, 0, "this window's share is used (40% of 40%)"},
		{"window share partly spent", func(in *Input) { in.SpentThisWindow = 0.25 }, true, 0.15, "budget 15%"},
		// 30 min left: 90% elapsed → share × 1.75 = 10.5 week points → 70.
		{"late in window", func(in *Input) { in.Usage.FiveHour.ResetsAt = now.Add(30 * time.Minute) }, true, 0.70, "budget 70%"},
		// 1 min left: share ×~2 = 79.6 points, capped by window room 75.
		{"window about to reset", func(in *Input) { in.Usage.FiveHour.ResetsAt = now.Add(time.Minute) }, true, 0.75, "budget 75%"},
		// Last window of the week: all 60 headroom points, capped by window room.
		{"week about to reset", func(in *Input) { in.Usage.SevenDay.ResetsAt = now.Add(3 * time.Hour) }, true, 0.75, "budget 75%"},
		// The 5h window is over: utilization counts from zero, no boost.
		{"5h window reset", func(in *Input) {
			in.Usage.FiveHour.Utilization = 0.95
			in.Usage.FiveHour.ResetsAt = now.Add(-10 * time.Minute)
		}, true, 0.40, "budget 40%"},
		// No active window: a job opens one; no boost.
		{"no 5h window", func(in *Input) { in.Usage.FiveHour.ResetsAt = time.Time{} }, true, 0.40, "budget 40%"},
		// Week reset passed: 90 points over ceil(168/5) = 34 windows.
		{"week reset passed", func(in *Input) {
			in.Usage.SevenDay.Utilization = 0.95
			in.Usage.SevenDay.ResetsAt = now.Add(-time.Hour)
		}, true, 0.90 / 34 / 0.15, "budget 18%"},
		// 2 points of headroom over 10 windows is 0.2 week points each:
		// 0.4 window points at ratio 0.5, under the 1-point minimum.
		{"tiny headroom", func(in *Input) { in.Settings.MaxBurnPct = 32; in.Ratio = 0.5 }, false, 0,
			"week at 30% of a 32% target: 2% left over 10 windows is too little to start a job"},
		{"higher ratio", func(in *Input) { in.Ratio = 0.30 }, true, 0.20, "(≈6% of the week)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := base()
			c.edit(&in)
			d := Decide(in)
			if d.CanRun != c.run || !near(d.BudgetPct, c.budget) || !strings.Contains(d.Reason, c.reason) {
				t.Errorf("got run=%v budget=%.4f %q\nwant run=%v budget=%.4f %q", d.CanRun, d.BudgetPct, d.Reason, c.run, c.budget, c.reason)
			}
			if d.CanRun && (d.BudgetPct < MinBudget || d.BudgetPct > 1-in.Settings.ReservePct/100) {
				t.Errorf("budget %.4f outside [min, 1-reserve]", d.BudgetPct)
			}
		})
	}
}

// Whatever the inputs, the pacer never plans past the reserve or the target.
func TestDecideNeverExceedsLimits(t *testing.T) {
	for u5 := 0.0; u5 <= 1.0; u5 += 0.05 {
		for u7 := 0.0; u7 <= 1.0; u7 += 0.05 {
			for _, left := range []time.Duration{time.Minute, time.Hour, 4 * time.Hour} {
				for _, spent := range []float64{0, 0.1, 0.5} {
					for _, weekLeft := range []time.Duration{3 * time.Hour, 50 * time.Hour} {
						in := base()
						in.Usage.SevenDay.ResetsAt = now.Add(weekLeft)
						in.Usage.FiveHour.Utilization, in.Usage.SevenDay.Utilization = u5, u7
						in.Usage.FiveHour.ResetsAt = now.Add(left)
						in.SpentThisWindow = spent
						d := Decide(in)
						if !d.CanRun {
							continue
						}
						if u5+d.BudgetPct > 1-in.Settings.ReservePct/100+1e-9 {
							t.Fatalf("u5=%.2f: budget %.3f eats the reserve", u5, d.BudgetPct)
						}
						if u7+d.BudgetPct*DefaultRatio > in.Settings.MaxBurnPct/100+1e-9 {
							t.Fatalf("u7=%.2f: budget %.3f overshoots the weekly target", u7, d.BudgetPct)
						}
					}
				}
			}
		}
	}
}

func snap(at time.Time, u5, u7 float64, r5, r7 time.Time) usage.Snapshot {
	return usage.Snapshot{
		FiveHour: usage.Window{Utilization: u5, ResetsAt: r5, ObservedAt: at},
		SevenDay: usage.Window{Utilization: u7, ResetsAt: r7, ObservedAt: at},
	}
}

func TestEstimateRatio(t *testing.T) {
	r5a, r5b, r7 := now.Add(time.Hour), now.Add(6*time.Hour), now.Add(48*time.Hour)
	if got := EstimateRatio(nil); got != DefaultRatio {
		t.Errorf("no samples: %v", got)
	}
	samples := []usage.Snapshot{
		// Out of order on purpose.
		snap(now.Add(10*time.Minute), 0.30, 0.34, r5a, r7),
		snap(now, 0.10, 0.30, r5a, r7),
		// Window reset: this pair doesn't count.
		snap(now.Add(20*time.Minute), 0.02, 0.35, r5b, r7),
		snap(now.Add(30*time.Minute), 0.12, 0.37, r5b, r7),
		// Utilization dropped (rounding/noise): ignored.
		snap(now.Add(40*time.Minute), 0.11, 0.37, r5b, r7),
	}
	// Counted: +0.20/+0.04 and +0.10/+0.02 → 0.06/0.30 = 0.2.
	if got := EstimateRatio(samples); !near(got, 0.2) {
		t.Errorf("ratio = %v, want 0.2", got)
	}
	// Too little movement to trust: default.
	if got := EstimateRatio(samples[:2][1:]); got != DefaultRatio {
		t.Errorf("one sample: %v", got)
	}
	small := []usage.Snapshot{snap(now, 0.10, 0.30, r5a, r7), snap(now.Add(time.Minute), 0.15, 0.32, r5a, r7)}
	if got := EstimateRatio(small); got != DefaultRatio {
		t.Errorf("5 points of movement: %v", got)
	}
	// Absurd values are clamped.
	wild := []usage.Snapshot{snap(now, 0.10, 0.10, r5a, r7), snap(now.Add(time.Minute), 0.40, 0.95, r5a, r7)}
	if got := EstimateRatio(wild); got != 1 {
		t.Errorf("clamp: %v", got)
	}
}

func TestEstimateRatioSkipsRejectedReadings(t *testing.T) {
	r5, r7 := now.Add(time.Hour), now.Add(48*time.Hour)
	rejected := snap(now.Add(20*time.Minute), 1.0, 0.35, r5, r7) // weekly copied from the last reading
	rejected.Status = "rejected"
	samples := []usage.Snapshot{
		snap(now, 0.10, 0.30, r5, r7),
		snap(now.Add(10*time.Minute), 0.35, 0.35, r5, r7),
		rejected,
	}
	got := EstimateRatioDetail(samples)
	if !got.FromData || !near(got.Ratio, 0.2) || !near(got.WindowPoints, 0.25) {
		t.Errorf("got %+v, want ratio 0.2 from 0.25 points (the rejected reading ignored)", got)
	}
}
