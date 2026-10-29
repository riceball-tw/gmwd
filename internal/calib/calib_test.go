package calib

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"gmwd/internal/store"
	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

func TestEstimate(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "gmwd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()

	// Empty: the default ratio, no model rates.
	e, err := Estimate(ctx, st, now)
	if err != nil || e.Ratio.FromData || e.Ratio.Ratio != 0.15 || len(e.Models) != 0 {
		t.Fatalf("empty = %+v, %v", e, err)
	}

	r5, r7 := now.Add(time.Hour), now.Add(48*time.Hour)
	sample := func(at time.Time, u5, u7 float64, status string) {
		st.AddUsage(ctx, usage.Snapshot{Status: status,
			FiveHour: usage.Window{Kind: usage.FiveHour, Utilization: u5, ResetsAt: r5, ObservedAt: at},
			SevenDay: usage.Window{Kind: usage.SevenDay, Utilization: u7, ResetsAt: r7, ObservedAt: at}})
	}
	sample(now.Add(-3*time.Hour), 0.10, 0.30, "allowed")
	sample(now.Add(-2*time.Hour), 0.40, 0.36, "allowed")
	sample(now.Add(-time.Hour), 1.00, 0.36, "rejected")
	sample(now.Add(-8*24*time.Hour), 0.0, 0.0, "rejected") // too old to count

	st.AddTopic(ctx, topic.Topic{ID: "t", Kind: topic.Research, Prompt: "p", Priority: 3})
	run := func(model string, started time.Time, tokens int64, spent float64, ended bool) {
		id, _ := st.StartRun(ctx, store.Run{TopicID: "t", StartedAt: started, Model: model})
		r := store.Run{ID: id, TopicID: "t", WindowSpent: spent, Tokens: usage.TokenCounts{Output: tokens, CacheRead: 10 * tokens}}
		if ended {
			r.EndedAt = started.Add(time.Minute)
		}
		st.FinishRun(ctx, r)
	}
	run("sonnet", now.Add(-time.Hour), 40_000, 0.02, true)
	run("sonnet", now.Add(-2*time.Hour), 80_000, 0.04, true)       // sonnet: 120k over 6 points → 20k/point
	run("opus", now.Add(-time.Hour), 10_000, 0.01, true)           // too little data
	run("haiku", now.Add(-20*24*time.Hour), 1_000_000, 0.10, true) // too old
	run("sonnet", now.Add(-10*time.Minute), 1_000_000, 0.0, false) // still running

	e, err = Estimate(ctx, st, now)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Ratio.FromData || math.Abs(e.Ratio.Ratio-0.2) > 1e-9 {
		t.Errorf("ratio = %+v (want 0.06/0.30, rejected reading skipped)", e.Ratio)
	}
	if e.LimitHits != 1 {
		t.Errorf("limit hits = %d", e.LimitHits)
	}
	if len(e.Models) != 1 {
		t.Fatalf("models = %+v", e.Models)
	}
	s, ok := e.Rate("sonnet")
	if !ok || math.Abs(s.TokensPerPoint-20_000) > 1e-6 || s.Runs != 2 || math.Abs(s.Points-0.06) > 1e-9 {
		t.Errorf("sonnet = %+v", s)
	}
	if _, ok := e.Rate("opus"); ok {
		t.Error("opus rate from too little data")
	}
}
