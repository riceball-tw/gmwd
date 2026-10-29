package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"gmwd/internal/config"
	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

var ctx = context.Background()

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gmwd.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func research(id string, prio int) topic.Topic {
	return topic.Topic{ID: id, Title: "T " + id, Kind: topic.Research, Prompt: "p", Priority: prio}
}

func TestTopicCRUD(t *testing.T) {
	s, _ := open(t)
	added, err := s.AddTopic(ctx, research("a", 3))
	if err != nil {
		t.Fatal(err)
	}
	if added.Status != topic.Queued || added.CreatedAt.IsZero() {
		t.Errorf("defaults not filled: %+v", added)
	}
	if _, err := s.AddTopic(ctx, research("a", 1)); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate add: %v", err)
	}
	if _, err := s.AddTopic(ctx, topic.Topic{ID: "bad", Kind: topic.Solve, Prompt: "p"}); err == nil {
		t.Error("invalid topic accepted")
	}

	got, err := s.GetTopic(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "T a" || got.Kind != topic.Research || got.CreatedAt.UnixMilli() != added.CreatedAt.UnixMilli() {
		t.Errorf("got %+v", got)
	}

	got.Prompt, got.Model, got.MaxTokens, got.DoneWhen, got.Repo = "new", "opus", 99, "when", "/r"
	if err := s.UpdateTopic(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, _ := s.GetTopic(ctx, "a")
	if again.Prompt != "new" || again.Model != "opus" || again.MaxTokens != 99 || again.DoneWhen != "when" || again.Repo != "/r" {
		t.Errorf("update lost fields: %+v", again)
	}

	if err := s.SetTopicStatus(ctx, "a", topic.Paused); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.GetTopic(ctx, "a"); again.Status != topic.Paused {
		t.Errorf("status = %q", again.Status)
	}
	if err := s.DeleteTopic(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"get":    func() error { _, err := s.GetTopic(ctx, "a"); return err }(),
		"delete": s.DeleteTopic(ctx, "a"),
		"status": s.SetTopicStatus(ctx, "a", topic.Done),
		"update": s.UpdateTopic(ctx, research("a", 1)),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s on missing topic: %v", name, err)
		}
	}
}

func TestListTopicsOrder(t *testing.T) {
	s, _ := open(t)
	base := time.Now()
	for i, tp := range []topic.Topic{research("low", 1), research("high-old", 5), research("mid", 3), research("high-new", 5)} {
		tp.CreatedAt = base.Add(time.Duration(i) * time.Second)
		if _, err := s.AddTopic(ctx, tp); err != nil {
			t.Fatal(err)
		}
	}
	ts, err := s.ListTopics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, tp := range ts {
		ids = append(ids, tp.ID)
	}
	want := []string{"high-old", "high-new", "mid", "low"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v", ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
}

func TestClaimTopicOnlyOnce(t *testing.T) {
	s, path := open(t)
	if _, err := s.AddTopic(ctx, research("job", 3)); err != nil {
		t.Fatal(err)
	}
	// Two processes racing: model them as two handles on one file.
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := s
			if i%2 == 1 {
				h = other
			}
			errs[i] = h.ClaimTopic(ctx, "job")
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("%d claims succeeded, want 1: %v", wins, errs)
	}
	if err := s.ClaimTopic(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("claim missing: %v", err)
	}
}

func TestReopenKeepsDataAndSchema(t *testing.T) {
	s, path := open(t)
	if _, err := s.AddTopic(ctx, research("keep", 3)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.GetTopic(ctx, "keep"); err != nil {
		t.Error(err)
	}
	var v int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != len(migrations) {
		t.Errorf("user_version = %d, %v", v, err)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	s, path := open(t)
	if _, err := s.db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s2, err := Open(path); err == nil {
		s2.Close()
		t.Error("opened a database from a newer gmwd")
	}
}

func TestRuns(t *testing.T) {
	s, _ := open(t)
	if _, err := s.AddTopic(ctx, research("r", 3)); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Hour)
	id, err := s.StartRun(ctx, Run{TopicID: "r", RunID: "run1", RunDir: "/d", StartedAt: start, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	fin := Run{ID: id, TopicID: "r", RunID: "run1", RunDir: "/d", EndedAt: time.Now(), StopReason: "done",
		Iterations: 3, Succeeded: 2, Failed: 1, Tokens: usage.TokenCounts{Input: 1, Output: 2, CacheCreate: 3, CacheRead: 4},
		CostUSD: 0.5, WindowSpent: 0.04, Commits: []string{"abc", "def"}}
	if err := s.FinishRun(ctx, fin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartRun(ctx, Run{TopicID: "r", RunID: "run2", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListRuns(ctx, "r", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].RunID != "run2" || !runs[0].EndedAt.IsZero() {
		t.Fatalf("runs = %+v", runs)
	}
	got := runs[1]
	if got.Model != "sonnet" {
		t.Errorf("model = %q", got.Model)
	}
	if got.StopReason != "done" || got.Iterations != 3 || got.Tokens.CacheRead != 4 || got.WindowSpent != 0.04 ||
		len(got.Commits) != 2 || got.StartedAt.UnixMilli() != start.UnixMilli() {
		t.Errorf("run = %+v", got)
	}
	if _, err := s.StartRun(ctx, Run{TopicID: "nope", StartedAt: time.Now()}); err == nil {
		t.Error("run for a missing topic accepted (foreign key)")
	}
	// Deleting a topic deletes its runs.
	if err := s.DeleteTopic(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	if runs, _ := s.ListRuns(ctx, "", 10); len(runs) != 0 {
		t.Errorf("runs survived topic delete: %+v", runs)
	}
}

func TestUsageSamples(t *testing.T) {
	s, _ := open(t)
	if _, err := s.LatestUsage(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty: %v", err)
	}
	r5, r7 := time.Unix(1793294400, 0), time.Unix(1793527200, 0)
	for i, u := range []float64{0.10, 0.12} {
		at := time.Unix(1793280000+int64(i)*60, 0)
		snap := usage.Snapshot{
			Status:   "allowed",
			FiveHour: usage.Window{Kind: usage.FiveHour, Utilization: u, ResetsAt: r5, Source: usage.SourceUsageCommand, ObservedAt: at},
			SevenDay: usage.Window{Kind: usage.SevenDay, Utilization: 0.3, ResetsAt: r7, Source: usage.SourceUsageCommand, ObservedAt: at},
		}
		if err := s.AddUsage(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LatestUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.FiveHour.Utilization != 0.12 || !got.FiveHour.ResetsAt.Equal(r5) || !got.SevenDay.ResetsAt.Equal(r7) ||
		got.FiveHour.Source != usage.SourceUsageCommand || got.Status != "allowed" || got.SevenDay.Kind != usage.SevenDay {
		t.Errorf("latest = %+v", got)
	}
}

func TestSettings(t *testing.T) {
	s, _ := open(t)
	got, err := s.Settings(ctx)
	if err != nil || got != config.DefaultSettings() {
		t.Fatalf("defaults = %+v, %v", got, err)
	}
	want := config.DefaultSettings()
	want.Paused, want.MaxBurnPct, want.ReservePct, want.IdleMode, want.QuietMinutes = true, 70, 20, config.IdleAway, 30
	if err := s.SaveSettings(ctx, want); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Settings(ctx); got != want {
		t.Errorf("got %+v", got)
	}
	if err := s.SaveSettings(ctx, config.Settings{MaxBurnPct: 200}); err == nil {
		t.Error("invalid settings saved")
	}
	if got, _ := s.Settings(ctx); got != want {
		t.Errorf("invalid save changed settings: %+v", got)
	}
}

func TestAddTopicReturnsStoredValues(t *testing.T) {
	s, _ := open(t)
	tp := research("prec", 3)
	tp.CreatedAt = time.Date(2026, 10, 29, 12, 0, 0, 123456789, time.UTC)
	added, err := s.AddTopic(ctx, tp)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTopic(ctx, "prec")
	if !added.CreatedAt.Equal(got.CreatedAt) {
		t.Errorf("added %v, stored %v", added.CreatedAt, got.CreatedAt)
	}
}

func TestSettingsFromOlderVersionGetNewDefaults(t *testing.T) {
	s, _ := open(t)
	// Settings saved before idle settings existed.
	if _, err := s.db.Exec(`INSERT INTO settings (id, json) VALUES (1, '{"paused":true,"maxBurnPct":60,"reservePct":10}')`); err != nil {
		t.Fatal(err)
	}
	got, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Paused || got.MaxBurnPct != 60 || got.IdleMode != config.IdleNotUsingClaude || got.QuietMinutes != 10 {
		t.Errorf("got %+v", got)
	}
}

func TestUpgradeFromV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0] + `; PRAGMA user_version = 1;
		INSERT INTO topics (id, title, kind, prompt, status, created_at, updated_at) VALUES ('old', 'Old', 'research', 'p', 'queued', 1, 1);
		INSERT INTO runs (topic_id, run_id, started_at, ended_at, window_spent) VALUES ('old', 'r', 1, 2, 0.05);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runs, err := s.ListRuns(ctx, "old", 10)
	if err != nil || len(runs) != 1 || runs[0].WindowSpent != 0.05 {
		t.Errorf("runs after upgrade: %+v %v", runs, err)
	}
}

func TestWindowSpent(t *testing.T) {
	s, _ := open(t)
	s.AddTopic(ctx, research("w", 3))
	windowStart := time.Now().Add(-2 * time.Hour)
	for _, r := range []struct {
		at    time.Time
		spent float64
	}{{windowStart.Add(-time.Minute), 0.30}, {windowStart.Add(time.Minute), 0.05}, {windowStart.Add(time.Hour), 0.07}} {
		id, _ := s.StartRun(ctx, Run{TopicID: "w", StartedAt: r.at})
		s.FinishRun(ctx, Run{ID: id, TopicID: "w", EndedAt: r.at.Add(time.Minute), WindowSpent: r.spent})
	}
	got, err := s.WindowSpent(ctx, windowStart)
	if err != nil || math.Abs(got-0.12) > 1e-9 {
		t.Errorf("spent = %v, %v", got, err)
	}
	if got, _ := s.WindowSpent(ctx, time.Now()); got != 0 {
		t.Errorf("empty window = %v", got)
	}
}

func TestRecoverAbandoned(t *testing.T) {
	s, _ := open(t)
	for _, id := range []string{"dead", "live", "norun", "queued"} {
		s.AddTopic(ctx, research(id, 3))
	}
	for _, id := range []string{"dead", "live", "norun"} {
		if err := s.ClaimTopic(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	s.StartRun(ctx, Run{TopicID: "dead", StartedAt: time.Now(), PID: 111})
	s.StartRun(ctx, Run{TopicID: "live", StartedAt: time.Now(), PID: 222})
	alive := func(pid int) bool { return pid == 222 }
	got, err := s.RecoverAbandoned(ctx, alive)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"dead", "norun"}) {
		t.Errorf("recovered %v", got)
	}
	for id, want := range map[string]topic.Status{"dead": topic.Queued, "norun": topic.Queued, "live": topic.Running, "queued": topic.Queued} {
		if tp, _ := s.GetTopic(ctx, id); tp.Status != want {
			t.Errorf("%s: %s, want %s", id, tp.Status, want)
		}
	}
	runs, _ := s.ListRuns(ctx, "dead", 1)
	if runs[0].StopReason != "abandoned" || runs[0].EndedAt.IsZero() {
		t.Errorf("dead run = %+v", runs[0])
	}
	if runs, _ := s.ListRuns(ctx, "live", 1); !runs[0].EndedAt.IsZero() {
		t.Error("live run was closed")
	}
}

func TestStartRunRecordsOurPID(t *testing.T) {
	s, _ := open(t)
	s.AddTopic(ctx, research("p", 3))
	s.ClaimTopic(ctx, "p")
	s.StartRun(ctx, Run{TopicID: "p", StartedAt: time.Now()})
	// We're alive, so nothing is recovered.
	got, _ := s.RecoverAbandoned(ctx, func(pid int) bool { return pid == os.Getpid() })
	if len(got) != 0 {
		t.Errorf("recovered our own run: %v", got)
	}
}

func TestListUsage(t *testing.T) {
	s, _ := open(t)
	base := time.Now().Add(-time.Hour)
	for i := range 3 {
		at := base.Add(time.Duration(i) * 10 * time.Minute)
		s.AddUsage(ctx, usage.Snapshot{FiveHour: usage.Window{Utilization: float64(i) / 10, ObservedAt: at}, SevenDay: usage.Window{Utilization: 0.3, ObservedAt: at}})
	}
	got, err := s.ListUsage(ctx, base.Add(5*time.Minute))
	if err != nil || len(got) != 2 || got[0].FiveHour.Utilization != 0.1 || got[1].FiveHour.Utilization != 0.2 {
		t.Errorf("got %+v, %v", got, err)
	}
}
