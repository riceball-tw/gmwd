package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gmwd/internal/config"
	"gmwd/internal/store"
)

// shortDir returns a temp dir short enough for a socket path (t.TempDir
// includes the test name and can exceed the 104-byte limit on macOS).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "gr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type echoParams struct {
	Text  string        `json:"text"`
	Sleep time.Duration `json:"sleep"`
}

// start runs a server with a few test handlers and returns its socket path.
func start(t *testing.T) (*Server, string, context.CancelFunc) {
	t.Helper()
	s := NewServer()
	s.Handle("echo", func(ctx context.Context, params json.RawMessage) (any, error) {
		var p echoParams
		if err := Decode(params, &p); err != nil {
			return nil, err
		}
		select {
		case <-time.After(p.Sleep):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return p, nil
	})
	s.Handle("missing", func(context.Context, json.RawMessage) (any, error) {
		return nil, fmt.Errorf("topic %q: %w", "x", store.ErrNotFound)
	})
	s.Handle("dup", func(context.Context, json.RawMessage) (any, error) {
		return nil, fmt.Errorf("topic %q: %w", "x", store.ErrExists)
	})
	s.Handle("fail", func(context.Context, json.RawMessage) (any, error) {
		return nil, errors.New("boom")
	})
	s.Handle("null", func(_ context.Context, params json.RawMessage) (any, error) {
		return string(params), nil
	})
	path := filepath.Join(shortDir(t), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("Serve didn't return promptly after cancel")
		}
	})
	return s, path, cancel
}

func dial(t *testing.T, path string) *Client {
	t.Helper()
	c, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestCallRoundTrip(t *testing.T) {
	_, path, _ := start(t)
	c := dial(t, path)
	var got echoParams
	if err := c.Call(context.Background(), "echo", echoParams{Text: "héllo\nworld"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Text != "héllo\nworld" {
		t.Errorf("got %q", got.Text)
	}
	var raw string
	if err := c.Call(context.Background(), "null", nil, &raw); err != nil || raw != "null" {
		t.Errorf("no params reached handler as %q, %v", raw, err)
	}
}

func TestErrors(t *testing.T) {
	_, path, _ := start(t)
	c := dial(t, path)
	ctx := context.Background()
	code := func(err error) int {
		var e *Error
		if !errors.As(err, &e) {
			t.Fatalf("not an *rpc.Error: %v", err)
		}
		return e.Code
	}
	if err := c.Call(ctx, "nope", nil, nil); code(err) != CodeMethodNotFound {
		t.Errorf("unknown method: %v", err)
	}
	if err := c.Call(ctx, "echo", "not an object", nil); code(err) != CodeInvalidParams {
		t.Errorf("bad params: %v", err)
	}
	if err := c.Call(ctx, "missing", nil, nil); !errors.Is(err, store.ErrNotFound) || !strings.Contains(err.Error(), `topic "x"`) {
		t.Errorf("not found: %v", err)
	}
	if err := c.Call(ctx, "dup", nil, nil); !errors.Is(err, store.ErrExists) {
		t.Errorf("exists: %v", err)
	}
	if err := c.Call(ctx, "fail", nil, nil); code(err) != CodeApp || err.Error() != "boom" || errors.Is(err, store.ErrNotFound) {
		t.Errorf("app error: %v", err)
	}
	// The connection is still usable after errors.
	if err := c.Call(ctx, "echo", echoParams{Text: "ok"}, nil); err != nil {
		t.Error(err)
	}
}

func TestConcurrentCallsMatchReplies(t *testing.T) {
	_, path, _ := start(t)
	c := dial(t, path)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := fmt.Sprint("msg-", i)
			var got echoParams
			// Varying delays make replies arrive out of order.
			err := c.Call(context.Background(), "echo", echoParams{Text: want, Sleep: time.Duration(50-i) * time.Millisecond}, &got)
			if err != nil || got.Text != want {
				t.Errorf("call %d: got %q, %v", i, got.Text, err)
			}
		}()
	}
	wg.Wait()
}

func TestSlowCallDoesNotBlockConnection(t *testing.T) {
	_, path, _ := start(t)
	c := dial(t, path)
	slow := make(chan error, 1)
	go func() { slow <- c.Call(context.Background(), "echo", echoParams{Sleep: time.Second}, nil) }()
	time.Sleep(50 * time.Millisecond)
	began := time.Now()
	if err := c.Call(context.Background(), "echo", echoParams{Text: "fast"}, nil); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 500*time.Millisecond {
		t.Errorf("fast call waited %v behind a slow one", took)
	}
	if err := <-slow; err != nil {
		t.Error(err)
	}
}

func TestCallContextCancel(t *testing.T) {
	_, path, _ := start(t)
	c := dial(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, "echo", echoParams{Sleep: time.Second}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
	// A late reply for the abandoned call must not confuse the next one.
	time.Sleep(time.Second)
	var got echoParams
	if err := c.Call(context.Background(), "echo", echoParams{Text: "next"}, &got); err != nil || got.Text != "next" {
		t.Errorf("next call: %q %v", got.Text, err)
	}
}

func TestEventsGoOnlyToSubscribers(t *testing.T) {
	s, path, _ := start(t)
	ctx := context.Background()
	sub1, sub2, other := dial(t, path), dial(t, path), dial(t, path)
	ev1, err := sub1.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ev2, err := sub2.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	otherEvents := make(chan Event, 1)
	other.mu.Lock()
	other.events = otherEvents // listening, but never subscribed
	other.mu.Unlock()

	s.Notify(Event{Type: EventTopicDeleted, TopicID: "gone"})
	for i, ch := range []<-chan Event{ev1, ev2} {
		select {
		case ev := <-ch:
			if ev.Type != EventTopicDeleted || ev.TopicID != "gone" || ev.At.IsZero() {
				t.Errorf("subscriber %d got %+v", i+1, ev)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("subscriber %d got nothing", i+1)
		}
	}
	// Round-trip a call so any stray event would have arrived by now.
	if err := other.Call(ctx, "echo", echoParams{}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-otherEvents:
		t.Errorf("unsubscribed client got %+v", ev)
	default:
	}
}

func TestStalledSubscriberIsDropped(t *testing.T) {
	s, path, _ := start(t)
	// A raw client that subscribes and then never reads.
	nc, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	fmt.Fprintln(nc, `{"jsonrpc":"2.0","id":1,"method":"subscribe"}`)
	time.Sleep(100 * time.Millisecond)

	big := strings.Repeat("x", 64<<10)
	done := make(chan struct{})
	go func() {
		for range 2000 {
			s.Notify(Event{Type: EventLog, Log: big})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Notify blocked on a client that doesn't read")
	}
	// The server gave up on it: reading eventually hits EOF.
	nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := bufioDrain(nc); err != nil && !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "reset") {
		t.Errorf("connection not closed: %v", err)
	}
}

func bufioDrain(nc net.Conn) (int64, error) {
	r := bufio.NewReader(nc)
	var n int64
	buf := make([]byte, 1<<20)
	for {
		k, err := r.Read(buf)
		n += int64(k)
		if err != nil {
			return n, err
		}
	}
}

func TestMalformedInputGetsErrorAndConnectionSurvives(t *testing.T) {
	_, path, _ := start(t)
	nc, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	r := bufio.NewScanner(nc)
	read := func() message {
		t.Helper()
		nc.SetReadDeadline(time.Now().Add(2 * time.Second))
		if !r.Scan() {
			t.Fatalf("no reply: %v", r.Err())
		}
		var m message
		if err := json.Unmarshal(r.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	fmt.Fprintln(nc, `{not json`)
	if m := read(); m.Error == nil || m.Error.Code != CodeParse {
		t.Errorf("parse error reply = %+v", m)
	}
	fmt.Fprintln(nc, `{"jsonrpc":"1.0","id":7,"method":"echo"}`)
	if m := read(); m.Error == nil || m.Error.Code != CodeInvalidRequest || m.ID == nil || *m.ID != 7 {
		t.Errorf("invalid request reply = %+v", m)
	}
	fmt.Fprintln(nc, `{"jsonrpc":"2.0","id":8,"method":"echo","params":{"text":"still here"}}`)
	if m := read(); m.Error != nil || !strings.Contains(string(m.Result), "still here") {
		t.Errorf("reply = %+v", m)
	}
}

func TestShutdownEndsClients(t *testing.T) {
	_, path, cancel := start(t)
	c := dial(t, path)
	events, err := c.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pending := make(chan error, 1)
	go func() { pending <- c.Call(context.Background(), "echo", echoParams{Sleep: 3 * time.Second}, nil) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("client not told the server went away")
	}
	// Depending on timing the handler replies "context canceled" or the
	// socket closes first; either way the call must fail, not hang.
	if err := <-pending; err == nil {
		t.Error("pending call succeeded across shutdown")
	}
	if _, ok := <-events; ok {
		t.Error("event channel not closed")
	}
	if err := c.Call(context.Background(), "echo", nil, nil); !errors.Is(err, ErrClosed) {
		t.Errorf("call after close: %v", err)
	}
}

func TestSocketPath(t *testing.T) {
	t.Setenv("GMWD_SOCKET", "")
	if p, err := SocketPath("/h"); err != nil || p != "/h/gmwd.sock" {
		t.Errorf("default = %q, %v", p, err)
	}
	if _, err := SocketPath("/" + strings.Repeat("d", 120)); err == nil || !strings.Contains(err.Error(), "GMWD_SOCKET") {
		t.Errorf("long path: %v", err)
	}
	t.Setenv("GMWD_SOCKET", "/tmp/g.sock")
	if p, _ := SocketPath("/" + strings.Repeat("d", 120)); p != "/tmp/g.sock" {
		t.Errorf("override = %q", p)
	}
}

func TestSettingsPatchApply(t *testing.T) {
	paused, burn := true, 55.0
	got := SettingsPatch{Paused: &paused, MaxBurnPct: &burn}.Apply(config.DefaultSettings())
	if !got.Paused || got.MaxBurnPct != 55 || got.ReservePct != 15 {
		t.Errorf("got %+v", got)
	}
}

func TestSettingsPatchIdleFields(t *testing.T) {
	mode, quiet := config.IdleAway, 0
	got := SettingsPatch{IdleMode: &mode, QuietMinutes: &quiet}.Apply(config.DefaultSettings())
	if got.IdleMode != config.IdleAway || got.QuietMinutes != 0 || got.MaxBurnPct != 90 {
		t.Errorf("got %+v", got)
	}
}
