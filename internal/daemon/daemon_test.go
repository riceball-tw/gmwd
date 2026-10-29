package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gmwd/internal/config"
	"gmwd/internal/rpc"
	"gmwd/internal/store"
	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

// shortHome is a data dir short enough for the socket path limit.
func shortHome(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "gd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type fakeProber struct {
	mu    sync.Mutex
	snaps []float64 // 5h utilization per call; the last repeats
	err   error
	calls int
}

func (p *fakeProber) Probe(ctx context.Context) (usage.Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return usage.Snapshot{}, p.err
	}
	u := p.snaps[min(p.calls-1, len(p.snaps)-1)]
	now := time.Now()
	return usage.Snapshot{
		FiveHour: usage.Window{Kind: usage.FiveHour, Utilization: u, ResetsAt: now.Add(time.Hour).Truncate(time.Minute), Source: usage.SourceUsageCommand, ObservedAt: now},
		SevenDay: usage.Window{Kind: usage.SevenDay, Utilization: 0.3, ResetsAt: now.Add(48 * time.Hour).Truncate(time.Minute), Source: usage.SourceUsageCommand, ObservedAt: now},
	}, nil
}

func (p *fakeProber) setErr(err error) {
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
}

type running struct {
	stop func() error // cancels and waits for Run to return
}

// startDaemon runs a daemon on home and waits until its socket answers.
func startDaemon(t *testing.T, home string, p Prober, poll time.Duration) running {
	t.Helper()
	d := &Daemon{Home: home, Prober: p, PollInterval: poll, Logf: t.Logf}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	sock := filepath.Join(home, "gmwd.sock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, err := rpc.Dial(sock); err == nil {
			c.Close()
			break
		}
		select {
		case err := <-done:
			cancel()
			t.Fatalf("daemon exited: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("daemon socket never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var once sync.Once
	var runErr error
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case runErr = <-done:
			case <-time.After(5 * time.Second):
				runErr = errors.New("daemon didn't stop")
			}
		})
		return runErr
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("daemon Run: %v", err)
		}
	})
	return running{stop: stop}
}

func client(t *testing.T, home string) *rpc.Client {
	t.Helper()
	c, err := rpc.Dial(filepath.Join(home, "gmwd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

var ctx = context.Background()

func nextEvent(t *testing.T, ch <-chan rpc.Event, want rpc.EventType) rpc.Event {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("event stream closed")
			}
			if ev.Type == want {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %s event", want)
		}
	}
}

func TestTopicLifecycleOverSocket(t *testing.T) {
	home := shortHome(t)
	startDaemon(t, home, nil, 0)
	c := client(t, home)
	events, err := c.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var a, b topic.Topic
	if err := c.Call(ctx, rpc.MethodTopicAdd, rpc.TopicParams{Topic: topic.Topic{Title: "SQLite drivers", Kind: topic.Research, Prompt: "compare"}}, &a); err != nil {
		t.Fatal(err)
	}
	if a.ID != "sqlite-drivers" || a.Status != topic.Queued || a.Priority != 3 || a.CreatedAt.IsZero() {
		t.Errorf("added %+v", a)
	}
	if ev := nextEvent(t, events, rpc.EventTopic); ev.Topic == nil || ev.Topic.ID != a.ID {
		t.Errorf("event %+v", ev)
	}
	// Same title again: a derived id is made unique.
	if err := c.Call(ctx, rpc.MethodTopicAdd, rpc.TopicParams{Topic: topic.Topic{Title: "SQLite drivers", Kind: topic.Research, Prompt: "again", Priority: 5}}, &b); err != nil {
		t.Fatal(err)
	}
	if b.ID != "sqlite-drivers-2" {
		t.Errorf("second id = %q", b.ID)
	}
	nextEvent(t, events, rpc.EventTopic)

	var list rpc.TopicsResult
	if err := c.Call(ctx, rpc.MethodTopicList, nil, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Topics) != 2 || list.Topics[0].ID != b.ID {
		t.Errorf("list = %+v (want higher priority first)", list.Topics)
	}

	a.Prompt, a.Status = "compare drivers in depth", topic.Paused
	var updated topic.Topic
	if err := c.Call(ctx, rpc.MethodTopicUpdate, rpc.TopicParams{Topic: a}, &updated); err != nil {
		t.Fatal(err)
	}
	var got topic.Topic
	if err := c.Call(ctx, rpc.MethodTopicGet, rpc.IDParams{ID: a.ID}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Prompt != "compare drivers in depth" || got.Status != topic.Paused || !got.CreatedAt.Equal(a.CreatedAt) {
		t.Errorf("after update: %+v", got)
	}
	nextEvent(t, events, rpc.EventTopic)

	if err := c.Call(ctx, rpc.MethodTopicDelete, rpc.IDParams{ID: a.ID}, nil); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, events, rpc.EventTopicDeleted); ev.TopicID != a.ID {
		t.Errorf("delete event %+v", ev)
	}
	if err := c.Call(ctx, rpc.MethodTopicGet, rpc.IDParams{ID: a.ID}, &got); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get deleted: %v", err)
	}
	if err := c.Call(ctx, rpc.MethodTopicDelete, rpc.IDParams{ID: a.ID}, nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}

	var st rpc.StatusResult
	if err := c.Call(ctx, rpc.MethodStatus, nil, &st); err != nil {
		t.Fatal(err)
	}
	if st.Topics[topic.Queued] != 1 || st.PID != os.Getpid() || st.Settings != config.DefaultSettings() {
		t.Errorf("status = %+v", st)
	}
}

func TestAddValidates(t *testing.T) {
	home := shortHome(t)
	startDaemon(t, home, nil, 0)
	c := client(t, home)
	repo := t.TempDir()
	cases := map[string]topic.Topic{
		"relative repo":   {Title: "x", Kind: topic.Solve, Prompt: "p", Repo: "rel/path"},
		"missing repo":    {Title: "x", Kind: topic.Solve, Prompt: "p", Repo: filepath.Join(repo, "nope")},
		"priority":        {Title: "x", Kind: topic.Research, Prompt: "p", Priority: 9},
		"solve, no repo":  {Title: "x", Kind: topic.Solve, Prompt: "p"},
		"empty prompt":    {Title: "x", Kind: topic.Research},
		"bad explicit id": {ID: "Not OK", Kind: topic.Research, Prompt: "p"},
	}
	for name, tp := range cases {
		if err := c.Call(ctx, rpc.MethodTopicAdd, rpc.TopicParams{Topic: tp}, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := topic.Topic{ID: "fix", Kind: topic.Solve, Prompt: "p", Repo: repo}
	if err := c.Call(ctx, rpc.MethodTopicAdd, rpc.TopicParams{Topic: ok}, nil); err != nil {
		t.Fatal(err)
	}
	// An explicit id is never renamed.
	if err := c.Call(ctx, rpc.MethodTopicAdd, rpc.TopicParams{Topic: ok}, nil); !errors.Is(err, store.ErrExists) {
		t.Errorf("duplicate explicit id: %v", err)
	}
	var list rpc.TopicsResult
	c.Call(ctx, rpc.MethodTopicList, nil, &list)
	if len(list.Topics) != 1 {
		t.Errorf("rejected topics were stored: %+v", list.Topics)
	}
}

func TestRunningTopicIsProtected(t *testing.T) {
	home := shortHome(t)
	startDaemon(t, home, nil, 0)
	c := client(t, home)
	var tp topic.Topic
	if err := c.Call(ctx, rpc.MethodTopicAdd, rpc.TopicParams{Topic: topic.Topic{ID: "busy", Kind: topic.Research, Prompt: "p"}}, &tp); err != nil {
		t.Fatal(err)
	}
	// A foreground `gmwd run` claims it through its own store handle.
	s, err := store.Open(filepath.Join(home, "gmwd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ClaimTopic(ctx, "busy"); err != nil {
		t.Fatal(err)
	}

	if err := c.Call(ctx, rpc.MethodTopicDelete, rpc.IDParams{ID: "busy"}, nil); err == nil || !strings.Contains(err.Error(), "running") {
		t.Errorf("deleted a running topic: %v", err)
	}
	tp.Status = topic.Queued
	if err := c.Call(ctx, rpc.MethodTopicUpdate, rpc.TopicParams{Topic: tp}, nil); err == nil {
		t.Error("moved a running topic out of running")
	}
	// Editing other fields of a running topic is fine; status stays.
	tp.Status, tp.Priority = "", 5
	var got topic.Topic
	if err := c.Call(ctx, rpc.MethodTopicUpdate, rpc.TopicParams{Topic: tp}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != topic.Running || got.Priority != 5 {
		t.Errorf("got %+v", got)
	}
	// And nobody can claim running through update.
	var other topic.Topic
	c.Call(ctx, rpc.MethodTopicAdd, rpc.TopicParams{Topic: topic.Topic{ID: "idle", Kind: topic.Research, Prompt: "p"}}, &other)
	other.Status = topic.Running
	if err := c.Call(ctx, rpc.MethodTopicUpdate, rpc.TopicParams{Topic: other}, nil); err == nil {
		t.Error("update set status to running")
	}
}

func TestSettingsPersistAcrossRestart(t *testing.T) {
	home := shortHome(t)
	d := startDaemon(t, home, nil, 0)
	c := client(t, home)
	events, _ := c.Subscribe(ctx)
	paused, burn := true, 70.0
	var got config.Settings
	if err := c.Call(ctx, rpc.MethodSettingsUpdate, rpc.SettingsPatch{Paused: &paused, MaxBurnPct: &burn}, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Paused || got.MaxBurnPct != 70 || got.ReservePct != 15 {
		t.Errorf("got %+v", got)
	}
	if ev := nextEvent(t, events, rpc.EventSettings); ev.Settings == nil || !ev.Settings.Paused {
		t.Errorf("event %+v", ev)
	}
	bad := 150.0
	if err := c.Call(ctx, rpc.MethodSettingsUpdate, rpc.SettingsPatch{ReservePct: &bad}, nil); err == nil {
		t.Error("invalid reserve accepted")
	}
	c.Close()
	if err := d.stop(); err != nil {
		t.Fatal(err)
	}

	startDaemon(t, home, nil, 0)
	var st rpc.StatusResult
	if err := client(t, home).Call(ctx, rpc.MethodStatus, nil, &st); err != nil {
		t.Fatal(err)
	}
	if !st.Settings.Paused || st.Settings.MaxBurnPct != 70 || st.Settings.ReservePct != 15 || st.Reason != "paused" {
		t.Errorf("after restart: %+v", st)
	}
}

func TestSingleInstance(t *testing.T) {
	home := shortHome(t)
	first := startDaemon(t, home, nil, 0)
	second := &Daemon{Home: home}
	err := second.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "already running (pid") {
		t.Fatalf("second daemon: %v", err)
	}
	// The failed start didn't disturb the first one.
	var st rpc.StatusResult
	if err := client(t, home).Call(ctx, rpc.MethodStatus, nil, &st); err != nil {
		t.Fatal(err)
	}
	if err := first.stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "gmwd.sock")); !os.IsNotExist(err) {
		t.Errorf("socket left behind: %v", err)
	}
	startDaemon(t, home, nil, 0) // the lock was released
}

func TestStaleSocketIsReplaced(t *testing.T) {
	home := shortHome(t)
	sock := filepath.Join(home, "gmwd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close() // as if a daemon crashed
	startDaemon(t, home, nil, 0)
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode %v, want 0600", perm)
	}
}

func TestRefusesNonSocketAtSocketPath(t *testing.T) {
	home := shortHome(t)
	sock := filepath.Join(home, "gmwd.sock")
	if err := os.WriteFile(sock, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := (&Daemon{Home: home}).Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("err = %v", err)
	}
	if b, _ := os.ReadFile(sock); string(b) != "precious" {
		t.Error("file at socket path was clobbered")
	}
}

func TestUsagePolling(t *testing.T) {
	home := shortHome(t)
	p := &fakeProber{snaps: []float64{0.10, 0.20}}
	d := startDaemon(t, home, p, 50*time.Millisecond)
	c := client(t, home)
	events, _ := c.Subscribe(ctx)
	ev := nextEvent(t, events, rpc.EventUsage)
	for ev.Usage.FiveHour.Utilization != 0.20 {
		ev = nextEvent(t, events, rpc.EventUsage)
	}
	var st rpc.StatusResult
	c.Call(ctx, rpc.MethodStatus, nil, &st)
	if st.Usage == nil || st.Usage.FiveHour.Utilization != 0.20 || st.UsageError != "" {
		t.Errorf("status usage = %+v %q", st.Usage, st.UsageError)
	}

	// A failing probe is reported but the last good usage is kept.
	p.setErr(errors.New("claude not found"))
	deadline := time.Now().Add(3 * time.Second)
	for st.UsageError == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		c.Call(ctx, rpc.MethodStatus, nil, &st)
	}
	if st.UsageError != "claude not found" || st.Usage == nil || st.Usage.FiveHour.Utilization != 0.20 {
		t.Errorf("after probe failure: usage %+v err %q", st.Usage, st.UsageError)
	}
	c.Close()
	d.stop()

	// The sample was stored: a restarted daemon knows usage before probing.
	startDaemon(t, home, &fakeProber{err: errors.New("offline")}, time.Hour)
	var st2 rpc.StatusResult
	client(t, home).Call(ctx, rpc.MethodStatus, nil, &st2)
	if st2.Usage == nil || st2.Usage.FiveHour.Utilization != 0.20 {
		t.Errorf("usage not restored from store: %+v", st2.Usage)
	}
}

func TestUsageRefreshOnDemand(t *testing.T) {
	home := shortHome(t)
	p := &fakeProber{snaps: []float64{0.10, 0.42}}
	startDaemon(t, home, p, time.Hour) // poller probes once at start, then waits
	c := client(t, home)
	var snap usage.Snapshot
	if err := c.Call(ctx, rpc.MethodUsageRefresh, nil, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FiveHour.Utilization != 0.42 {
		t.Errorf("refresh = %+v", snap.FiveHour)
	}
	p.setErr(errors.New("boom"))
	if err := c.Call(ctx, rpc.MethodUsageRefresh, nil, &snap); err == nil || err.Error() != "boom" {
		t.Errorf("refresh error = %v", err)
	}
}
