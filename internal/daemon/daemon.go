// Package daemon is the long-lived gmwd process: it owns the store, serves
// the socket and keeps usage fresh. Clients (CLI, TUI) only talk to it.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gmwd/internal/rpc"
	"gmwd/internal/scheduler"
	"gmwd/internal/store"
	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

// Prober reads current usage; usage.Prober in production.
type Prober interface {
	Probe(ctx context.Context) (usage.Snapshot, error)
}

// DefaultPollInterval is how often the daemon runs `claude /usage`, which
// costs no quota.
const DefaultPollInterval = 5 * time.Minute

type Daemon struct {
	Home         string
	Prober       Prober
	PollInterval time.Duration
	Logf         func(format string, args ...any)
	// Runner runs jobs. Without it the daemon serves and polls but never
	// starts work on its own.
	Runner       scheduler.RunFunc
	Activity     scheduler.ActivityChecker
	TickInterval time.Duration

	store   *store.Store
	srv     *rpc.Server
	sched   *scheduler.Scheduler
	started time.Time

	probeMu  sync.Mutex // one probe at a time
	mu       sync.Mutex
	usage    *usage.Snapshot
	usageErr string
}

func (d *Daemon) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

// Run serves until ctx is done. It fails if another daemon holds the lock.
func (d *Daemon) Run(ctx context.Context) error {
	if err := os.MkdirAll(d.Home, 0o700); err != nil {
		return err
	}
	unlock, err := lock(filepath.Join(d.Home, "daemon.lock"))
	if err != nil {
		return err
	}
	defer unlock()

	d.store, err = store.Open(filepath.Join(d.Home, "gmwd.db"))
	if err != nil {
		return err
	}
	defer d.store.Close()
	if snap, err := d.store.LatestUsage(ctx); err == nil {
		d.usage = &snap
	}
	// A previous daemon (or gmwd run) that died mid-job left its topic
	// running; put it back in the queue.
	if ids, err := d.store.RecoverAbandoned(ctx, processAlive); err != nil {
		d.logf("recover abandoned jobs: %v", err)
	} else if len(ids) > 0 {
		d.logf("re-queued topics abandoned by a dead process: %s", strings.Join(ids, ", "))
	}

	sock, err := rpc.SocketPath(d.Home)
	if err != nil {
		return err
	}
	// We hold the lock, so a socket file left behind is from a dead daemon.
	if fi, err := os.Lstat(sock); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return fmt.Errorf("%s exists and is not a socket", sock)
		}
		os.Remove(sock)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer os.Remove(sock)
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return err
	}

	d.started = time.Now()
	d.srv = rpc.NewServer()
	d.register()

	pollCtx, stopPoll := context.WithCancel(ctx)
	defer stopPoll()
	go d.poll(pollCtx)

	schedDone := make(chan struct{})
	if d.Runner != nil {
		d.sched = &scheduler.Scheduler{
			Store:    d.store,
			Run:      d.Runner,
			Activity: d.Activity,
			OnUsage:  func(snap usage.Snapshot) { d.ObserveUsage(context.Background(), snap) },
			Notify:   d.srv.Notify,
			Logf:     d.logf,
			Interval: d.TickInterval,
		}
		go func() {
			defer close(schedDone)
			d.sched.Loop(ctx)
		}()
	} else {
		close(schedDone)
	}

	d.logf("daemon started (pid %d), socket %s", os.Getpid(), sock)
	err = d.srv.Serve(ctx, ln)
	<-schedDone // a running job is stopped and recorded before the store closes
	d.logf("daemon stopped")
	return err
}

// wake asks the scheduler to reconsider now.
func (d *Daemon) wake() {
	if d.sched != nil {
		d.sched.Wake()
	}
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// lock takes an exclusive flock on path and writes our pid into it.
func lock(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		b, _ := os.ReadFile(path)
		f.Close()
		if pid := strings.TrimSpace(string(b)); pid != "" {
			return nil, fmt.Errorf("gmwd daemon already running (pid %s)", pid)
		}
		return nil, errors.New("gmwd daemon already running")
	}
	f.Truncate(0)
	f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func (d *Daemon) poll(ctx context.Context) {
	if d.Prober == nil {
		return
	}
	every := d.PollInterval
	if every <= 0 {
		every = DefaultPollInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := d.refreshUsage(ctx); err != nil && ctx.Err() == nil {
			d.logf("usage probe: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (d *Daemon) refreshUsage(ctx context.Context) (usage.Snapshot, error) {
	if d.Prober == nil {
		return usage.Snapshot{}, errors.New("no usage prober configured")
	}
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	pctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	snap, err := d.Prober.Probe(pctx)
	if err != nil {
		d.mu.Lock()
		d.usageErr = err.Error()
		d.mu.Unlock()
		return snap, err
	}
	d.ObserveUsage(ctx, snap)
	return snap, nil
}

// ObserveUsage records a usage observation from any source (the poller now;
// running jobs' rate_limit_events later) and tells subscribers.
func (d *Daemon) ObserveUsage(ctx context.Context, snap usage.Snapshot) {
	// A rejected rate_limit_event may name only the window that hit its
	// limit; keep the last known value of the other.
	d.mu.Lock()
	if prev := d.usage; prev != nil {
		if snap.FiveHour.Kind == "" {
			snap.FiveHour = prev.FiveHour
		}
		if snap.SevenDay.Kind == "" {
			snap.SevenDay = prev.SevenDay
		}
	}
	d.mu.Unlock()
	if snap.FiveHour.Kind == "" || snap.SevenDay.Kind == "" {
		d.logf("ignoring a partial usage reading with nothing to fill it from")
		return
	}
	if err := d.store.AddUsage(ctx, snap); err != nil {
		d.logf("store usage: %v", err)
	}
	d.mu.Lock()
	d.usage, d.usageErr = &snap, ""
	d.mu.Unlock()
	d.srv.Notify(rpc.Event{Type: rpc.EventUsage, Usage: &snap})
}

func (d *Daemon) register() {
	handle := func(method string, h rpc.Handler) { d.srv.Handle(method, h) }
	handle(rpc.MethodStatus, d.status)
	handle(rpc.MethodTopicAdd, d.topicAdd)
	handle(rpc.MethodTopicList, func(ctx context.Context, _ json.RawMessage) (any, error) {
		ts, err := d.store.ListTopics(ctx)
		return rpc.TopicsResult{Topics: ts}, err
	})
	handle(rpc.MethodTopicGet, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p rpc.IDParams
		if err := rpc.Decode(params, &p); err != nil {
			return nil, err
		}
		return d.store.GetTopic(ctx, p.ID)
	})
	handle(rpc.MethodTopicUpdate, d.topicUpdate)
	handle(rpc.MethodTopicDelete, d.topicDelete)
	handle(rpc.MethodSettingsUpdate, d.settingsUpdate)
	handle(rpc.MethodRunsList, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p rpc.RunsParams
		if err := rpc.Decode(params, &p); err != nil {
			return nil, err
		}
		if p.Limit <= 0 {
			p.Limit = 20
		}
		runs, err := d.store.ListRuns(ctx, p.TopicID, p.Limit)
		return rpc.RunsResult{Runs: runs}, err
	})
	handle(rpc.MethodUsageRefresh, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return d.refreshUsage(ctx)
	})
}

func (d *Daemon) status(ctx context.Context, _ json.RawMessage) (any, error) {
	settings, err := d.store.Settings(ctx)
	if err != nil {
		return nil, err
	}
	ts, err := d.store.ListTopics(ctx)
	if err != nil {
		return nil, err
	}
	res := rpc.StatusResult{PID: os.Getpid(), StartedAt: d.started, Settings: settings, Topics: map[topic.Status]int{}}
	for _, t := range ts {
		res.Topics[t.Status]++
	}
	d.mu.Lock()
	res.Usage, res.UsageError = d.usage, d.usageErr
	d.mu.Unlock()
	switch {
	case d.sched != nil:
		res.Job, res.Reason = d.sched.Status()
		if res.Reason == "" {
			res.Reason = "starting"
		}
	case settings.Paused:
		res.Reason = "paused"
	default:
		res.Reason = "automatic runs are off in this daemon; use gmwd run"
	}
	return res, nil
}

func (d *Daemon) topicAdd(ctx context.Context, params json.RawMessage) (any, error) {
	var p rpc.TopicParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	t := p.Topic
	t.Status, t.CreatedAt = topic.Queued, time.Now()
	if t.Priority == 0 {
		t.Priority = 3
	}
	if err := checkTopicFields(t); err != nil {
		return nil, err
	}
	if t.ID != "" {
		return d.addAndNotify(ctx, t)
	}
	// Derive the id from the title and make it unique.
	base := topic.Slug(t.Title)
	for i := 1; ; i++ {
		t.ID = base
		if i > 1 {
			t.ID = fmt.Sprintf("%s-%d", base, i)
		}
		added, err := d.addAndNotify(ctx, t)
		if !errors.Is(err, store.ErrExists) || i >= 100 {
			return added, err
		}
	}
}

func (d *Daemon) addAndNotify(ctx context.Context, t topic.Topic) (topic.Topic, error) {
	added, err := d.store.AddTopic(ctx, t)
	if err != nil {
		return added, err
	}
	d.srv.Notify(rpc.Event{Type: rpc.EventTopic, Topic: &added})
	d.wake()
	return added, nil
}

// checkTopicFields validates what the store can't: priority range and that
// a repo path exists. The daemon may run in another cwd, so paths must be
// absolute.
func checkTopicFields(t topic.Topic) error {
	if t.Priority < 1 || t.Priority > 5 {
		return &rpc.Error{Code: rpc.CodeInvalidParams, Message: fmt.Sprintf("priority %d: want 1..5", t.Priority)}
	}
	if t.Repo != "" {
		if !filepath.IsAbs(t.Repo) {
			return &rpc.Error{Code: rpc.CodeInvalidParams, Message: fmt.Sprintf("repo %q: must be an absolute path", t.Repo)}
		}
		if fi, err := os.Stat(t.Repo); err != nil || !fi.IsDir() {
			return &rpc.Error{Code: rpc.CodeInvalidParams, Message: fmt.Sprintf("repo %q: not a directory", t.Repo)}
		}
	}
	return nil
}

func (d *Daemon) topicUpdate(ctx context.Context, params json.RawMessage) (any, error) {
	var p rpc.TopicParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	t := p.Topic
	old, err := d.store.GetTopic(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	if t.Status == "" {
		t.Status = old.Status
	}
	// Running is entered and left only by whoever runs the topic.
	if (t.Status == topic.Running) != (old.Status == topic.Running) {
		return nil, &rpc.Error{Code: rpc.CodeApp, Message: fmt.Sprintf("topic %q: can't change status from %s to %s here", t.ID, old.Status, t.Status)}
	}
	if err := checkTopicFields(t); err != nil {
		return nil, err
	}
	t.CreatedAt = old.CreatedAt
	if err := d.store.UpdateTopic(ctx, t); err != nil {
		return nil, err
	}
	d.srv.Notify(rpc.Event{Type: rpc.EventTopic, Topic: &t})
	d.wake()
	return t, nil
}

func (d *Daemon) topicDelete(ctx context.Context, params json.RawMessage) (any, error) {
	var p rpc.IDParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	t, err := d.store.GetTopic(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if t.Status == topic.Running {
		return nil, &rpc.Error{Code: rpc.CodeApp, Message: fmt.Sprintf("topic %q is running; stop it first", p.ID)}
	}
	if err := d.store.DeleteTopic(ctx, p.ID); err != nil {
		return nil, err
	}
	d.srv.Notify(rpc.Event{Type: rpc.EventTopicDeleted, TopicID: p.ID})
	return nil, nil
}

func (d *Daemon) settingsUpdate(ctx context.Context, params json.RawMessage) (any, error) {
	var p rpc.SettingsPatch
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	cur, err := d.store.Settings(ctx)
	if err != nil {
		return nil, err
	}
	next := p.Apply(cur)
	if err := next.Validate(); err != nil {
		return nil, &rpc.Error{Code: rpc.CodeInvalidParams, Message: err.Error()}
	}
	if err := d.store.SaveSettings(ctx, next); err != nil {
		return nil, err
	}
	d.srv.Notify(rpc.Event{Type: rpc.EventSettings, Settings: &next})
	d.wake()
	return next, nil
}
