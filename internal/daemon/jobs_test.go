package daemon

import (
	"context"
	"os"
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

type idle struct{}

func (idle) Check(context.Context, config.Settings) (activity.State, error) {
	return activity.State{Idle: true, Reason: "idle"}, nil
}

// startWithJobs runs a daemon whose scheduler uses run as the runner.
func startWithJobs(t *testing.T, home string, run func(context.Context, topic.Topic, runner.Options) (runner.Summary, error)) running {
	t.Helper()
	d := &Daemon{Home: home, Prober: &fakeProber{snaps: []float64{0.10}}, PollInterval: time.Hour,
		Runner: run, Activity: idle{}, TickInterval: time.Hour, Logf: t.Logf}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, err := rpc.Dial(filepath.Join(home, "gmwd.sock")); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon didn't start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var once sync.Once
	stop := func() error {
		var err error
		once.Do(func() { cancel(); err = <-done })
		return err
	}
	t.Cleanup(func() { stop() })
	return running{stop: stop}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAddedTopicRunsAutomatically(t *testing.T) {
	home := shortHome(t)
	var mu sync.Mutex
	var got []runner.Options
	startWithJobs(t, home, func(ctx context.Context, tp topic.Topic, o runner.Options) (runner.Summary, error) {
		mu.Lock()
		got = append(got, o)
		mu.Unlock()
		return runner.Summary{TopicID: tp.ID, Iterations: 1, StopReason: runner.StopDone, TopicStatus: topic.Done}, nil
	})
	c := client(t, home)
	events, _ := c.Subscribe(ctx)
	if err := c.Call(ctx, rpc.MethodTopicAdd, rpc.TopicParams{Topic: topic.Topic{ID: "auto", Kind: topic.Research, Prompt: "p"}}, nil); err != nil {
		t.Fatal(err)
	}
	// Adding wakes the scheduler; no need to wait for the hourly tick.
	nextEvent(t, events, rpc.EventJob)
	eventually(t, "topic done", func() bool {
		var tp topic.Topic
		c.Call(ctx, rpc.MethodTopicGet, rpc.IDParams{ID: "auto"}, &tp)
		return tp.Status == topic.Done
	})
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Budget.WindowPct <= 0 || got[0].Budget.ReservePct != 0.15 {
		t.Errorf("runner options = %+v", got)
	}
	var st rpc.StatusResult
	c.Call(ctx, rpc.MethodStatus, nil, &st)
	if st.Job != nil || st.Reason != "idle: no queued topics" {
		t.Errorf("status after = job %+v reason %q", st.Job, st.Reason)
	}
}

func TestStatusShowsJobAndPauseStopsIt(t *testing.T) {
	home := shortHome(t)
	started := make(chan struct{})
	startWithJobs(t, home, func(ctx context.Context, tp topic.Topic, o runner.Options) (runner.Summary, error) {
		close(started)
		for {
			if ctx.Err() != nil {
				return runner.Summary{TopicID: tp.ID, StopReason: runner.StopCancelled, TopicStatus: topic.Queued}, nil
			}
			if r := o.ShouldStop(); r != "" {
				return runner.Summary{TopicID: tp.ID, StopReason: r, TopicStatus: topic.Queued}, nil
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	c := client(t, home)
	c.Call(ctx, rpc.MethodTopicAdd, rpc.TopicParams{Topic: topic.Topic{ID: "long", Kind: topic.Research, Prompt: "p"}}, nil)
	<-started
	var st rpc.StatusResult
	c.Call(ctx, rpc.MethodStatus, nil, &st)
	if st.Job == nil || st.Job.TopicID != "long" || !strings.HasPrefix(st.Reason, "running long:") || st.Topics[topic.Running] != 1 {
		t.Errorf("status during job: job %+v reason %q topics %v", st.Job, st.Reason, st.Topics)
	}
	paused := true
	c.Call(ctx, rpc.MethodSettingsUpdate, rpc.SettingsPatch{Paused: &paused}, nil)
	eventually(t, "job to stop", func() bool {
		st = rpc.StatusResult{} // decoding into a used value keeps stale fields
		c.Call(ctx, rpc.MethodStatus, nil, &st)
		return st.Job == nil && st.Topics[topic.Queued] == 1
	})
	if st.Reason != "paused" {
		t.Errorf("reason = %q", st.Reason)
	}
	var runs rpc.RunsResult
	c.Call(ctx, rpc.MethodRunsList, rpc.RunsParams{TopicID: "long"}, &runs)
	if len(runs.Runs) != 1 || runs.Runs[0].StopReason != "paused" {
		t.Errorf("runs = %+v", runs.Runs)
	}
}

func TestRecoversAbandonedTopicsAtStart(t *testing.T) {
	home := shortHome(t)
	os.MkdirAll(home, 0o700)
	s, err := store.Open(filepath.Join(home, "gmwd.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.AddTopic(ctx, topic.Topic{ID: "orphan", Kind: topic.Research, Prompt: "p", Priority: 3})
	s.ClaimTopic(ctx, "orphan")
	s.StartRun(ctx, store.Run{TopicID: "orphan", StartedAt: time.Now(), PID: 1 << 30}) // no such process
	s.Close()

	startDaemon(t, home, nil, 0)
	var tp topic.Topic
	if err := client(t, home).Call(ctx, rpc.MethodTopicGet, rpc.IDParams{ID: "orphan"}, &tp); err != nil {
		t.Fatal(err)
	}
	if tp.Status != topic.Queued {
		t.Errorf("status = %s", tp.Status)
	}
}

func TestPartialUsageFromJobIsMerged(t *testing.T) {
	home := shortHome(t)
	startWithJobs(t, home, func(ctx context.Context, tp topic.Topic, o runner.Options) (runner.Summary, error) {
		// A rejected event that names only the 5h window.
		info := usage.RateLimitInfo{Status: "rejected", ResetsAt: time.Now().Add(time.Hour).Unix(), RateLimitType: "five_hour"}
		o.OnUsage(info.Snapshot(time.Now()))
		return runner.Summary{TopicID: tp.ID, StopReason: runner.StopRateLimited, TopicStatus: topic.Queued}, nil
	})
	c := client(t, home)
	eventually(t, "first usage", func() bool {
		var st rpc.StatusResult
		c.Call(ctx, rpc.MethodStatus, nil, &st)
		return st.Usage != nil
	})
	c.Call(ctx, rpc.MethodTopicAdd, rpc.TopicParams{Topic: topic.Topic{ID: "hit", Kind: topic.Research, Prompt: "p"}}, nil)
	var st rpc.StatusResult
	eventually(t, "rejected usage", func() bool {
		c.Call(ctx, rpc.MethodStatus, nil, &st)
		return st.Usage != nil && st.Usage.Status == "rejected"
	})
	if st.Usage.FiveHour.Utilization != 1 || st.Usage.SevenDay.Utilization != 0.3 || st.Usage.SevenDay.ResetsAt.IsZero() {
		t.Errorf("usage = %+v", st.Usage)
	}
	// And the scheduler now waits for the reset instead of retrying.
	eventually(t, "rate-limit reason", func() bool {
		st = rpc.StatusResult{}
		c.Call(ctx, rpc.MethodStatus, nil, &st)
		return strings.HasPrefix(st.Reason, "rate limited until")
	})
}

// A topic added before the first usage reading must start as soon as the
// reading arrives, not at the next (hourly here) tick.
func TestUsageReadingWakesScheduler(t *testing.T) {
	home := shortHome(t)
	release := make(chan struct{})
	slow := &gatedProber{gate: release, inner: &fakeProber{snaps: []float64{0.10}}}
	ran := make(chan struct{}, 1)
	d := &Daemon{Home: home, Prober: slow, PollInterval: time.Hour, Activity: idle{}, TickInterval: time.Hour, Logf: t.Logf,
		Runner: func(ctx context.Context, tp topic.Topic, o runner.Options) (runner.Summary, error) {
			ran <- struct{}{}
			return runner.Summary{TopicID: tp.ID, StopReason: runner.StopDone, TopicStatus: topic.Done}, nil
		}}
	rctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(rctx) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, "socket", func() bool {
		c, err := rpc.Dial(filepath.Join(home, "gmwd.sock"))
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	c := client(t, home)
	c.Call(ctx, rpc.MethodTopicAdd, rpc.TopicParams{Topic: topic.Topic{ID: "early", Kind: topic.Research, Prompt: "p"}}, nil)
	var st rpc.StatusResult
	eventually(t, "waiting for usage", func() bool {
		st = rpc.StatusResult{}
		c.Call(ctx, rpc.MethodStatus, nil, &st)
		return st.Reason == "waiting: usage not known yet"
	})
	close(release)
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("topic didn't start when usage arrived")
	}
}

type gatedProber struct {
	gate  chan struct{}
	inner Prober
}

func (g *gatedProber) Probe(ctx context.Context) (usage.Snapshot, error) {
	select {
	case <-g.gate:
	case <-ctx.Done():
		return usage.Snapshot{}, ctx.Err()
	}
	return g.inner.Probe(ctx)
}
