// Package scheduler decides when the daemon runs a topic: only while the
// user is idle, gmwd isn't paused, and the pacer grants a budget. One job
// runs at a time.
package scheduler

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	"gmwd/internal/activity"
	"gmwd/internal/config"
	"gmwd/internal/pacer"
	"gmwd/internal/rpc"
	"gmwd/internal/runner"
	"gmwd/internal/store"
	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

// DefaultInterval is how often the scheduler reconsiders without a nudge.
const DefaultInterval = time.Minute

// RunFunc runs one topic; (*runner.Runner).Run in production.
type RunFunc func(ctx context.Context, t topic.Topic, opts runner.Options) (runner.Summary, error)

// ActivityChecker reports whether the user is idle.
type ActivityChecker interface {
	Check(ctx context.Context, s config.Settings) (activity.State, error)
}

type Scheduler struct {
	Store    *store.Store
	Run      RunFunc
	Activity ActivityChecker
	// OnUsage receives rate_limit_events from running jobs (the daemon
	// stores them and tells clients).
	OnUsage func(usage.Snapshot)
	Notify  func(rpc.Event)
	// OnFinished is called after a job's outcome is recorded.
	OnFinished func(t topic.Topic, r store.Run)
	Logf       func(format string, args ...any)
	Now        func() time.Time
	Interval   time.Duration

	wake chan struct{}
	mu   sync.Mutex
	job  *rpc.JobInfo
	// jobDone is closed when the current job has been cleaned up.
	jobDone    chan struct{}
	cancelJob  context.CancelFunc
	reason     string
	lastLogged string
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Scheduler) notify(ev rpc.Event) {
	if s.Notify != nil {
		s.Notify(ev)
	}
}

// Wake asks for a decision now (settings changed, topic added, ...).
func (s *Scheduler) Wake() {
	s.mu.Lock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	w := s.wake
	s.mu.Unlock()
	select {
	case w <- struct{}{}:
	default:
	}
}

// Status returns the running job (nil if none) and why gmwd is or isn't
// working.
func (s *Scheduler) Status() (*rpc.JobInfo, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return nil, s.reason
	}
	j := *s.job
	return &j, s.reason
}

// Loop ticks until ctx is done, then stops any running job and waits for
// it to clean up.
func (s *Scheduler) Loop(ctx context.Context) {
	s.mu.Lock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	wake := s.wake
	s.mu.Unlock()
	every := s.Interval
	if every <= 0 {
		every = DefaultInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	defer s.stopJob()
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-wake:
		}
	}
}

func (s *Scheduler) stopJob() {
	s.mu.Lock()
	cancel, done := s.cancelJob, s.jobDone
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// WaitJob blocks until the current job (if any) has finished.
func (s *Scheduler) WaitJob() {
	s.mu.Lock()
	done := s.jobDone
	s.mu.Unlock()
	if done != nil {
		<-done
	}
}

// digits makes reasons comparable across ticks: "used 6s ago" and "used
// 16s ago" are the same situation and shouldn't each be logged.
var digits = regexp.MustCompile(`[0-9]+`)

func (s *Scheduler) setReason(r string) {
	key := digits.ReplaceAllString(r, "#")
	s.mu.Lock()
	s.reason = r
	changed := key != s.lastLogged
	s.lastLogged = key
	s.mu.Unlock()
	if changed {
		s.logf("pacer: %s", r)
	}
}

// Tick makes one decision and starts a job if everything allows it. It
// returns the reason it reached.
func (s *Scheduler) Tick(ctx context.Context) string {
	s.mu.Lock()
	busy := s.job != nil
	s.mu.Unlock()
	if busy {
		return s.reasonNow()
	}
	reason, start := s.decide(ctx)
	if start != nil {
		s.start(ctx, *start)
	}
	s.setReason(reason)
	return reason
}

func (s *Scheduler) reasonNow() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

type plan struct {
	topic    topic.Topic
	settings config.Settings
	decision pacer.Decision
	usage    usage.Snapshot
}

func (s *Scheduler) decide(ctx context.Context) (string, *plan) {
	settings, err := s.Store.Settings(ctx)
	if err != nil {
		return "error: " + err.Error(), nil
	}
	if settings.Paused {
		return "paused", nil
	}
	if s.Activity != nil {
		st, err := s.Activity.Check(ctx, settings)
		if err != nil {
			s.logf("activity: %v", err)
		}
		if !st.Idle {
			return "waiting: " + st.Reason, nil
		}
	}
	ts, err := s.Store.ListTopics(ctx)
	if err != nil {
		return "error: " + err.Error(), nil
	}
	var next *topic.Topic
	for i := range ts {
		if ts[i].Status == topic.Queued {
			next = &ts[i] // highest priority, then oldest
			break
		}
	}
	if next == nil {
		return "idle: no queued topics", nil
	}

	now := s.now()
	in := pacer.Input{Now: now, Settings: settings}
	snap, err := s.Store.LatestUsage(ctx)
	if err == nil {
		in.Usage = &snap
		if snap.FiveHour.ResetsAt.After(now) {
			if in.SpentThisWindow, err = s.Store.WindowSpent(ctx, snap.FiveHour.WindowStart()); err != nil {
				return "error: " + err.Error(), nil
			}
		}
		if hist, err := s.Store.ListUsage(ctx, now.Add(-7*24*time.Hour)); err == nil {
			in.Ratio = pacer.EstimateRatio(hist)
		}
	}
	d := pacer.Decide(in)
	if !d.CanRun {
		return d.Reason, nil
	}
	return fmt.Sprintf("running %s: %s", next.ID, d.Reason), &plan{topic: *next, settings: settings, decision: d, usage: snap}
}

func (s *Scheduler) start(ctx context.Context, p plan) {
	if err := s.Store.ClaimTopic(ctx, p.topic.ID); err != nil {
		s.logf("claim %s: %v", p.topic.ID, err) // e.g. a foreground gmwd run took it
		return
	}
	startedAt := s.now()
	runRow, err := s.Store.StartRun(ctx, store.Run{TopicID: p.topic.ID, StartedAt: startedAt, Model: p.topic.ModelOrDefault()})
	if err != nil {
		s.logf("record run: %v", err)
		s.Store.SetTopicStatus(ctx, p.topic.ID, topic.Queued)
		return
	}
	jctx, cancel := context.WithCancel(ctx)
	job := &rpc.JobInfo{TopicID: p.topic.ID, StartedAt: startedAt}
	done := make(chan struct{})
	s.mu.Lock()
	s.job, s.cancelJob, s.jobDone = job, cancel, done
	s.mu.Unlock()
	running := p.topic
	running.Status = topic.Running
	s.notify(rpc.Event{Type: rpc.EventTopic, Topic: &running})
	s.notify(rpc.Event{Type: rpc.EventJob, Job: job})
	s.logf("starting %s (%s) with %.0f%% of the 5h window", p.topic.ID, p.topic.Kind, p.decision.BudgetPct*100)

	go func() {
		defer close(done)
		defer cancel()
		s.runJob(jctx, p, runRow)
	}()
}

func (s *Scheduler) runJob(ctx context.Context, p plan, runRow int64) {
	opts := runner.Options{
		Budget: runner.Budget{
			Iterations: runner.DefaultIterations,
			WindowPct:  p.decision.BudgetPct,
			ReservePct: p.settings.ReservePct / 100,
		},
		OnUsage: func(snap usage.Snapshot) {
			if s.OnUsage != nil {
				s.OnUsage(snap)
			}
		},
		ShouldStop: func() runner.StopReason { return s.shouldStop(ctx) },
		OnIteration: func(sum runner.Summary) {
			s.mu.Lock()
			s.job.Iteration, s.job.Tokens, s.job.RunID = sum.Iterations, sum.Tokens, sum.RunID
			j := *s.job
			s.mu.Unlock()
			s.notify(rpc.Event{Type: rpc.EventJob, Job: &j})
		},
	}
	if p.usage.FiveHour.Kind != "" {
		u := p.usage
		opts.Start = &u
	}
	sum, runErr := s.Run(ctx, p.topic, opts)

	// Record the outcome even when the daemon is shutting down.
	rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status := sum.TopicStatus
	stop := string(sum.StopReason)
	if runErr != nil {
		// Setup errors (repo gone, not a git repo, ...) won't fix themselves:
		// don't retry the topic every minute.
		status, stop = topic.Failed, "error: "+runErr.Error()
		s.logf("%s failed: %v", p.topic.ID, runErr)
	}
	if status == "" {
		status = topic.Queued
	}
	finished := store.Run{ID: runRow, TopicID: p.topic.ID, RunID: sum.RunID, RunDir: sum.RunDir, Model: p.topic.ModelOrDefault(),
		EndedAt: s.now(), StopReason: stop, Iterations: sum.Iterations, Succeeded: sum.Succeeded, Failed: sum.Failed,
		Tokens: sum.Tokens, CostUSD: sum.CostUSD, WindowSpent: sum.WindowSpent, Commits: sum.Commits}
	if err := s.Store.FinishRun(rctx, finished); err != nil {
		s.logf("record run: %v", err)
	}
	if err := s.Store.SetTopicStatus(rctx, p.topic.ID, status); err != nil {
		s.logf("set %s %s: %v", p.topic.ID, status, err)
	}
	s.logf("finished %s: %s after %d iterations, %.0f%% of the 5h window", p.topic.ID, stop, sum.Iterations, sum.WindowSpent*100)

	// Clear the job and its "running" reason together, so status never
	// shows no job with a stale reason.
	s.mu.Lock()
	s.job, s.cancelJob = nil, nil
	s.reason = fmt.Sprintf("finished %s (%s)", p.topic.ID, stop)
	s.mu.Unlock()
	if t, err := s.Store.GetTopic(rctx, p.topic.ID); err == nil {
		s.notify(rpc.Event{Type: rpc.EventTopic, Topic: &t})
	}
	s.notify(rpc.Event{Type: rpc.EventJob})
	s.Wake() // decide what's next before the slower follow-up work
	if s.OnFinished != nil {
		t := p.topic
		t.Status = status
		s.OnFinished(t, finished)
	}
}

// shouldStop ends a job at the next iteration boundary when gmwd is
// paused or the user is back.
func (s *Scheduler) shouldStop(ctx context.Context) runner.StopReason {
	settings, err := s.Store.Settings(ctx)
	if err != nil {
		return ""
	}
	if settings.Paused {
		return runner.StopPaused
	}
	if s.Activity != nil {
		if st, err := s.Activity.Check(ctx, settings); err == nil && !st.Idle {
			s.logf("stopping after this iteration: %s", st.Reason)
			return runner.StopUserActive
		}
	}
	return ""
}
