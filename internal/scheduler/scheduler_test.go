package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gmwd/internal/activity"
	"gmwd/internal/config"
	"gmwd/internal/rpc"
	"gmwd/internal/runner"
	"gmwd/internal/store"
	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

var ctx = context.Background()

type fakeActivity struct {
	mu   sync.Mutex
	idle bool
}

func (f *fakeActivity) set(idle bool) { f.mu.Lock(); f.idle = idle; f.mu.Unlock() }

func (f *fakeActivity) Check(context.Context, config.Settings) (activity.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idle {
		return activity.State{Idle: true, Reason: "idle: no Claude Code use for 10m"}, nil
	}
	return activity.State{Reason: "user active: Claude Code used 1m ago"}, nil
}

type env struct {
	t     *testing.T
	st    *store.Store
	act   *fakeActivity
	s     *Scheduler
	mu    sync.Mutex
	runs  []runner.Options
	logs  []string
	usage []usage.Snapshot
	// run is what the fake runner does; default: one done iteration.
	run RunFunc
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "gmwd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, act: &fakeActivity{idle: true}}
	e.run = func(ctx context.Context, tp topic.Topic, o runner.Options) (runner.Summary, error) {
		return runner.Summary{TopicID: tp.ID, Iterations: 1, Succeeded: 1, StopReason: runner.StopDone, TopicStatus: topic.Done, WindowSpent: 0.03}, nil
	}
	e.s = &Scheduler{
		Store:    st,
		Activity: e.act,
		Run: func(ctx context.Context, tp topic.Topic, o runner.Options) (runner.Summary, error) {
			e.mu.Lock()
			e.runs = append(e.runs, o)
			run := e.run
			e.mu.Unlock()
			return run(ctx, tp, o)
		},
		OnUsage: func(u usage.Snapshot) { e.mu.Lock(); e.usage = append(e.usage, u); e.mu.Unlock() },
		Logf: func(format string, args ...any) {
			e.mu.Lock()
			e.logs = append(e.logs, strings.TrimSpace(fmt.Sprintf(format, args...)))
			e.mu.Unlock()
		},
	}
	return e
}

// freshUsage stores a reading: 5h at u5 resetting in 4h, week 30% resetting
// in 50h. With default settings the pacer grants 40 points.
func (e *env) freshUsage(u5 float64) {
	now := time.Now()
	e.st.AddUsage(ctx, usage.Snapshot{
		Status:   "allowed",
		FiveHour: usage.Window{Kind: usage.FiveHour, Utilization: u5, ResetsAt: now.Add(4 * time.Hour), ObservedAt: now, Source: usage.SourceUsageCommand},
		SevenDay: usage.Window{Kind: usage.SevenDay, Utilization: 0.30, ResetsAt: now.Add(50 * time.Hour), ObservedAt: now, Source: usage.SourceUsageCommand},
	})
}

func (e *env) add(id string, prio int) {
	if _, err := e.st.AddTopic(ctx, topic.Topic{ID: id, Kind: topic.Research, Prompt: "p", Priority: prio}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) status(id string) topic.Status {
	tp, err := e.st.GetTopic(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return tp.Status
}

func (e *env) runCount() int { e.mu.Lock(); defer e.mu.Unlock(); return len(e.runs) }

func (e *env) settings(f func(*config.Settings)) {
	s, _ := e.st.Settings(ctx)
	f(&s)
	if err := e.st.SaveSettings(ctx, s); err != nil {
		e.t.Fatal(err)
	}
}

func TestRunsTopicWhenIdleAndWithinBudget(t *testing.T) {
	e := newEnv(t)
	e.freshUsage(0.10)
	e.add("t1", 3)
	reason := e.s.Tick(ctx)
	if !strings.HasPrefix(reason, "running t1: budget 40% of the 5h window") {
		t.Fatalf("reason = %q", reason)
	}
	e.s.WaitJob()
	if e.runCount() != 1 {
		t.Fatalf("runs = %d", e.runCount())
	}
	o := e.runs[0]
	if math.Abs(o.Budget.WindowPct-0.40) > 1e-9 || o.Budget.ReservePct != 0.15 || o.Budget.Iterations != runner.DefaultIterations {
		t.Errorf("budget = %+v", o.Budget)
	}
	if o.Start == nil || o.Start.FiveHour.Utilization != 0.10 {
		t.Errorf("start usage = %+v", o.Start)
	}
	if e.status("t1") != topic.Done {
		t.Errorf("status = %s", e.status("t1"))
	}
	runs, _ := e.st.ListRuns(ctx, "t1", 5)
	if len(runs) != 1 || runs[0].StopReason != "done" || runs[0].EndedAt.IsZero() || runs[0].WindowSpent != 0.03 {
		t.Errorf("runs = %+v", runs)
	}
	if job, _ := e.s.Status(); job != nil {
		t.Errorf("job still set: %+v", job)
	}
}

func TestDoesNotRun(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(e *env)
		reason string
	}{
		{"user active", func(e *env) { e.freshUsage(0.1); e.add("t", 3); e.act.set(false) }, "waiting: user active: Claude Code used 1m ago"},
		{"paused", func(e *env) {
			e.freshUsage(0.1)
			e.add("t", 3)
			e.settings(func(s *config.Settings) { s.Paused = true })
		}, "paused"},
		{"no topics", func(e *env) { e.freshUsage(0.1) }, "idle: no queued topics"},
		{"only paused or done topics", func(e *env) {
			e.freshUsage(0.1)
			e.add("a", 3)
			e.add("b", 3)
			e.st.SetTopicStatus(ctx, "a", topic.Paused)
			e.st.SetTopicStatus(ctx, "b", topic.Done)
		}, "idle: no queued topics"},
		{"no usage yet", func(e *env) { e.add("t", 3) }, "waiting: usage not known yet"},
		{"reserve reached", func(e *env) { e.freshUsage(0.86); e.add("t", 3) }, "5h window at 86%, keeping 15% for you"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			c.setup(e)
			if got := e.s.Tick(ctx); !strings.HasPrefix(got, c.reason) {
				t.Errorf("reason = %q, want %q…", got, c.reason)
			}
			if e.runCount() != 0 {
				t.Error("a job ran")
			}
			if _, r := e.s.Status(); !strings.HasPrefix(r, c.reason) {
				t.Errorf("status reason = %q", r)
			}
		})
	}
}

func TestPicksHighestPriorityQueued(t *testing.T) {
	e := newEnv(t)
	e.freshUsage(0.1)
	e.add("low", 2)
	e.add("top-paused", 5)
	e.st.SetTopicStatus(ctx, "top-paused", topic.Paused)
	e.add("high", 4)
	if r := e.s.Tick(ctx); !strings.HasPrefix(r, "running high:") {
		t.Errorf("reason = %q", r)
	}
	e.s.WaitJob()
}

// blockingRun simulates iterations, asking ShouldStop before each one.
func blockingRun(iterations chan<- int) RunFunc {
	return func(ctx context.Context, tp topic.Topic, o runner.Options) (runner.Summary, error) {
		sum := runner.Summary{TopicID: tp.ID, TopicStatus: topic.Queued}
		for i := 1; ; i++ {
			if ctx.Err() != nil {
				sum.StopReason = runner.StopCancelled
				return sum, nil
			}
			if r := o.ShouldStop(); r != "" {
				sum.StopReason = r
				return sum, nil
			}
			sum.Iterations = i
			o.OnIteration(sum)
			iterations <- i
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func TestJobStopsWhenUserReturns(t *testing.T) {
	e := newEnv(t)
	e.freshUsage(0.1)
	e.add("t", 3)
	iters := make(chan int, 100)
	e.run = blockingRun(iters)
	e.s.Tick(ctx)
	<-iters
	job, _ := e.s.Status()
	if job == nil || job.TopicID != "t" || job.Iteration < 1 {
		t.Errorf("job = %+v", job)
	}
	// While a job runs, ticks don't start another.
	e.add("other", 5)
	e.s.Tick(ctx)
	e.act.set(false)
	e.s.WaitJob()
	if e.runCount() != 1 {
		t.Errorf("runs = %d", e.runCount())
	}
	runs, _ := e.st.ListRuns(ctx, "t", 1)
	if runs[0].StopReason != string(runner.StopUserActive) || e.status("t") != topic.Queued {
		t.Errorf("stop %q status %s", runs[0].StopReason, e.status("t"))
	}
}

func TestPauseStopsJobAtBoundary(t *testing.T) {
	e := newEnv(t)
	e.freshUsage(0.1)
	e.add("t", 3)
	iters := make(chan int, 100)
	e.run = blockingRun(iters)
	e.s.Tick(ctx)
	<-iters
	e.settings(func(s *config.Settings) { s.Paused = true })
	e.s.WaitJob()
	runs, _ := e.st.ListRuns(ctx, "t", 1)
	if runs[0].StopReason != string(runner.StopPaused) {
		t.Errorf("stop %q", runs[0].StopReason)
	}
}

func TestSetupErrorFailsTopicInsteadOfRetrying(t *testing.T) {
	e := newEnv(t)
	e.freshUsage(0.1)
	e.add("broken", 3)
	e.run = func(context.Context, topic.Topic, runner.Options) (runner.Summary, error) {
		return runner.Summary{}, errors.New("/repo: not a git repository")
	}
	e.s.Tick(ctx)
	e.s.WaitJob()
	if e.status("broken") != topic.Failed {
		t.Errorf("status = %s", e.status("broken"))
	}
	if r := e.s.Tick(ctx); r != "idle: no queued topics" || e.runCount() != 1 {
		t.Errorf("retried: %q, runs %d", r, e.runCount())
	}
	runs, _ := e.st.ListRuns(ctx, "broken", 1)
	if !strings.Contains(runs[0].StopReason, "not a git repository") {
		t.Errorf("stop reason %q", runs[0].StopReason)
	}
}

func TestWindowShareLimitsTheNextJob(t *testing.T) {
	e := newEnv(t)
	e.freshUsage(0.1)
	e.add("a", 3)
	e.add("b", 3)
	e.run = func(ctx context.Context, tp topic.Topic, o runner.Options) (runner.Summary, error) {
		// Used its whole budget.
		return runner.Summary{TopicID: tp.ID, StopReason: runner.StopWindow, TopicStatus: topic.Queued, WindowSpent: o.Budget.WindowPct}, nil
	}
	e.s.Tick(ctx)
	e.s.WaitJob()
	if r := e.s.Tick(ctx); !strings.HasPrefix(r, "this window's share is used") || e.runCount() != 1 {
		t.Errorf("reason %q, runs %d", r, e.runCount())
	}
}

func TestUsageFromJobsIsForwarded(t *testing.T) {
	e := newEnv(t)
	e.freshUsage(0.1)
	e.add("t", 3)
	e.run = func(ctx context.Context, tp topic.Topic, o runner.Options) (runner.Summary, error) {
		o.OnUsage(usage.Snapshot{FiveHour: usage.Window{Utilization: 0.2}})
		return runner.Summary{TopicStatus: topic.Done, StopReason: runner.StopDone}, nil
	}
	e.s.Tick(ctx)
	e.s.WaitJob()
	if len(e.usage) != 1 || e.usage[0].FiveHour.Utilization != 0.2 {
		t.Errorf("forwarded %+v", e.usage)
	}
}

func TestEventsDuringJob(t *testing.T) {
	e := newEnv(t)
	e.freshUsage(0.1)
	e.add("t", 3)
	var mu sync.Mutex
	var evs []rpc.Event
	e.s.Notify = func(ev rpc.Event) { mu.Lock(); evs = append(evs, ev); mu.Unlock() }
	e.s.Tick(ctx)
	e.s.WaitJob()
	mu.Lock()
	defer mu.Unlock()
	var kinds []string
	for _, ev := range evs {
		k := string(ev.Type)
		if ev.Type == rpc.EventJob && ev.Job == nil {
			k += ":end"
		}
		if ev.Topic != nil {
			k += ":" + string(ev.Topic.Status)
		}
		kinds = append(kinds, k)
	}
	want := "topic:running job topic:done job:end"
	if strings.Join(kinds, " ") != want {
		t.Errorf("events %v, want %s", kinds, want)
	}
}

func TestLoopShutdownStopsJobAndReleasesTopic(t *testing.T) {
	e := newEnv(t)
	e.freshUsage(0.1)
	e.add("t", 3)
	iters := make(chan int, 100)
	e.run = blockingRun(iters)
	e.s.Interval = time.Hour
	lctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { e.s.Loop(lctx); close(done) }()
	<-iters
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Loop didn't return")
	}
	if e.status("t") != topic.Queued {
		t.Errorf("status = %s", e.status("t"))
	}
	runs, _ := e.st.ListRuns(ctx, "t", 1)
	if runs[0].StopReason != string(runner.StopCancelled) || runs[0].EndedAt.IsZero() {
		t.Errorf("run = %+v", runs[0])
	}
}

func TestWakeTriggersDecision(t *testing.T) {
	e := newEnv(t)
	e.freshUsage(0.1)
	e.s.Interval = time.Hour
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go e.s.Loop(lctx)
	waitFor(t, func() bool { _, r := e.s.Status(); return r == "idle: no queued topics" })
	e.add("t", 3)
	e.s.Wake()
	waitFor(t, func() bool { return e.runCount() == 1 })
}

func TestReasonsDifferingOnlyInNumbersAreLoggedOnce(t *testing.T) {
	e := newEnv(t)
	for _, r := range []string{"waiting: user active: Claude Code used 6s ago", "waiting: user active: Claude Code used 16s ago", "paused", "waiting: user active: Claude Code used 2m ago"} {
		e.s.setReason(r)
	}
	if len(e.logs) != 3 {
		t.Errorf("logged %q", e.logs)
	}
	if _, r := e.s.Status(); r != "waiting: user active: Claude Code used 2m ago" {
		t.Errorf("status shows %q, want the latest wording", r)
	}
}

func TestReasonsAreLoggedOnChange(t *testing.T) {
	e := newEnv(t)
	e.freshUsage(0.1)
	e.s.Tick(ctx)
	e.s.Tick(ctx)
	e.act.set(false)
	e.s.Tick(ctx)
	var pacer []string
	for _, l := range e.logs {
		if strings.HasPrefix(l, "pacer: ") {
			pacer = append(pacer, l)
		}
	}
	want := []string{"pacer: idle: no queued topics", "pacer: waiting: user active: Claude Code used 1m ago"}
	if strings.Join(pacer, "|") != strings.Join(want, "|") {
		t.Errorf("logged %q", pacer)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNoStaleRunningReasonAfterJob(t *testing.T) {
	e := newEnv(t)
	e.freshUsage(0.1)
	e.add("t", 3)
	release := make(chan struct{})
	e.s.OnFinished = func(topic.Topic, store.Run) { <-release } // slow follow-up work
	e.s.Tick(ctx)
	waitFor(t, func() bool { job, _ := e.s.Status(); return job == nil })
	job, reason := e.s.Status()
	if job == nil && strings.HasPrefix(reason, "running") {
		t.Errorf("no job but reason %q", reason)
	}
	close(release)
	e.s.WaitJob()
}
