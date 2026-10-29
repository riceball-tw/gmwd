package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gmwd/internal/daemon"
	"gmwd/internal/launchd"
	"gmwd/internal/rpc"
	"gmwd/internal/store"
	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

// Run through a symlink named claude, the test binary acts as claude: it
// answers /usage and finishes any topic in one iteration. (An env marker
// wouldn't survive the runner's env scrub.)
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "claude" {
		fakeClaude()
		return
	}
	os.Exit(m.Run())
}

func fakeClaude() {
	prompt := ""
	for i, a := range os.Args {
		if a == "-p" && i+1 < len(os.Args) {
			prompt = os.Args[i+1]
		}
	}
	if prompt == "/usage" {
		reset := time.Now().UTC().Add(2 * time.Hour).Format("Jan 2 at 3:04pm")
		week := time.Now().UTC().Add(50 * time.Hour).Format("Jan 2 at 3pm")
		text := fmt.Sprintf("Current session: 12%% used · resets %s (UTC)\nCurrent week (all models): 30%% used · resets %s (UTC)\n", reset, week)
		b, _ := json.Marshal(map[string]any{"type": "result", "is_error": false, "result": text})
		fmt.Println(string(b))
		return
	}
	os.WriteFile("report.md", []byte("# done\n"), 0o600)
	emit := func(v any) { b, _ := json.Marshal(v); fmt.Println(string(b)) }
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": "s"})
	emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "session_id": "s", "num_turns": 1,
		"usage":             map[string]any{"input_tokens": 5, "output_tokens": 50},
		"structured_output": map[string]any{"status": "done", "summary": "Wrote the report."}})
}

// env points gmwd at a fresh, short home, an empty $HOME (no real session
// logs or LaunchAgents), and the fake claude.
func env(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "gc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("GMWD_HOME", home)
	t.Setenv("GMWD_SOCKET", "")
	t.Setenv("HOME", t.TempDir())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(t.TempDir(), "claude")
	if err := os.Symlink(exe, fake); err != nil {
		t.Fatal(err)
	}
	old := claudeBin
	claudeBin = fake
	t.Cleanup(func() { claudeBin = old })
	return home
}

// startDaemon runs a daemon like `gmwd daemon` would, until the test ends
// or stop is called.
func startDaemon(t *testing.T, home string) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	d := &daemon.Daemon{Home: home, Prober: usage.Prober{Claude: claudeBin, Dir: filepath.Join(home, "probe")}, PollInterval: time.Hour}
	go func() { done <- d.Run(ctx) }()
	if _, err := waitForDaemon(10 * time.Second); err != nil {
		cancel()
		t.Fatalf("daemon didn't come up: %v", err)
	}
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Errorf("daemon: %v", err)
		}
	}
	t.Cleanup(stop)
	return stop
}

// run invokes a command and returns its output.
func run(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := commands[name](args, &out)
	return out.String(), err
}

func mustRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := run(t, name, args...)
	if err != nil {
		t.Fatalf("gmwd %s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

func contains(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, got)
		}
	}
}

func TestCLIAgainstDaemon(t *testing.T) {
	home := env(t)
	startDaemon(t, home)

	contains(t, "add", mustRun(t, "add", "--kind", "research", "--title", "SQLite drivers", "--prompt", "compare them", "--priority", "4"),
		"added sqlite-drivers (research, priority 4)")

	// Prompt from stdin; title from its first line.
	r, w, _ := os.Pipe()
	fmt.Fprint(w, "Why is CI slow?\nLook at the last 20 runs.\n")
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	out := mustRun(t, "add", "--kind", "research", "--prompt", "-", "--json")
	os.Stdin = oldStdin
	var ci topic.Topic
	if err := json.Unmarshal([]byte(out), &ci); err != nil {
		t.Fatalf("add --json: %v\n%s", err, out)
	}
	if ci.ID != "why-is-ci-slow" || !strings.Contains(ci.Prompt, "last 20 runs") {
		t.Errorf("stdin topic = %+v", ci)
	}

	contains(t, "ls", mustRun(t, "ls"), "ID", "sqlite-drivers", "research  4", "queued", "sonnet", "why-is-ci-slow")

	contains(t, "pause", mustRun(t, "pause"), "gmwd paused")
	contains(t, "status", mustRun(t, "status"), "Daemon: running", "PAUSED", "12% used", "30% used", "2 queued")
	contains(t, "resume", mustRun(t, "resume"), "gmwd resumed")
	contains(t, "status", mustRun(t, "status"), "active")

	contains(t, "pause topic", mustRun(t, "pause", "why-is-ci-slow"), "why-is-ci-slow paused")
	contains(t, "pause topic again", mustRun(t, "pause", "why-is-ci-slow"), "why-is-ci-slow is paused")
	contains(t, "ls", mustRun(t, "ls"), "paused")
	contains(t, "resume topic", mustRun(t, "resume", "why-is-ci-slow"), "why-is-ci-slow resumed")
	if _, err := run(t, "pause", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("pause missing topic: %v", err)
	}

	contains(t, "run", mustRun(t, "run", "sqlite-drivers", "--window-pct", "0"), "Topic sqlite-drivers: done (stopped: done)", "report.md")
	contains(t, "ls after run", mustRun(t, "ls"), "done")
	c, err := dialDaemon()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var runs rpc.RunsResult
	if err := c.Call(context.Background(), rpc.MethodRunsList, rpc.RunsParams{TopicID: "sqlite-drivers"}, &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs.Runs) != 1 || runs.Runs[0].StopReason != "done" || runs.Runs[0].Tokens.Output != 50 || runs.Runs[0].EndedAt.IsZero() {
		t.Errorf("runs = %+v", runs.Runs)
	}

	contains(t, "rm", mustRun(t, "rm", "sqlite-drivers"), "removed sqlite-drivers")
	if out := mustRun(t, "ls"); strings.Contains(out, "sqlite-drivers") {
		t.Errorf("removed topic still listed:\n%s", out)
	}
}

func TestCLIWithoutDaemon(t *testing.T) {
	home := env(t)
	if _, err := run(t, "add", "--kind", "research", "--title", "x", "--prompt", "y"); !errors.Is(err, errNoDaemon) {
		t.Errorf("add without daemon: %v", err)
	}
	if _, err := run(t, "ls"); !errors.Is(err, errNoDaemon) {
		t.Errorf("ls without daemon: %v", err)
	}
	contains(t, "status", mustRun(t, "status"), "Daemon: not running", "12% used")

	// gmwd run works on its own, straight against the store.
	out := mustRun(t, "run", "--kind", "research", "--title", "Offline topic", "--prompt", "p", "--window-pct", "0")
	contains(t, "run", out, "Topic offline-topic: done")
	st, err := store.Open(filepath.Join(home, "gmwd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.GetTopic(context.Background(), "offline-topic")
	if err != nil || got.Status != topic.Done {
		t.Errorf("topic after run: %+v %v", got, err)
	}
}

func TestRunRefusesTopicAlreadyRunning(t *testing.T) {
	home := env(t)
	st, err := store.Open(filepath.Join(home, "gmwd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	st.AddTopic(ctx, topic.Topic{ID: "busy", Kind: topic.Research, Prompt: "p", Priority: 3})
	if err := st.ClaimTopic(ctx, "busy"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "run", "busy"); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("err = %v", err)
	}
	// The refused run must not release the other runner's claim.
	if got, _ := st.GetTopic(ctx, "busy"); got.Status != topic.Running {
		t.Errorf("status = %q", got.Status)
	}
	if runs, _ := st.ListRuns(ctx, "busy", 10); len(runs) != 0 {
		t.Errorf("refused run was recorded: %+v", runs)
	}
}

func TestInstallAndUninstall(t *testing.T) {
	home := env(t)
	t.Setenv("PATH", "/opt/test/bin:/usr/bin")
	exe := filepath.Join(t.TempDir(), "gmwd")
	os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755)
	oldExe, oldLC := executable, launchctl
	t.Cleanup(func() { executable, launchctl = oldExe, oldLC })
	executable = func() (string, error) { return exe, nil }

	// A fake launchctl that starts and stops an in-process daemon, the way
	// launchd would start `gmwd daemon`.
	var calls []string
	var stop func()
	launchctl = func(args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "bootstrap":
			stop = startDaemon(t, home)
		case "bootout":
			if stop == nil {
				return []byte("not loaded"), errors.New("exit status 3")
			}
			stop()
			stop = nil
		}
		return nil, nil
	}

	out := mustRun(t, "install")
	contains(t, "install", out, "installed", "daemon running (pid", "daemon.log")
	userHome, _ := os.UserHomeDir()
	plist, err := os.ReadFile(launchd.PlistPath(userHome, launchd.Label))
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(exe)
	contains(t, "plist", string(plist), "<string>"+resolved+"</string>", "<string>daemon</string>",
		"<string>/opt/test/bin:/usr/bin</string>", "<key>GMWD_HOME</key>", "<string>"+home+"</string>")

	// Installing again reloads it.
	mustRun(t, "install")
	contains(t, "uninstall", mustRun(t, "uninstall"), "daemon stopped")
	if _, err := os.Stat(launchd.PlistPath(userHome, launchd.Label)); !os.IsNotExist(err) {
		t.Error("plist still installed")
	}
	if _, err := dialDaemon(); !errors.Is(err, errNoDaemon) {
		t.Error("daemon still answering after uninstall")
	}
	want := []string{"bootout gui/", "bootstrap gui/", "bootout gui/", "bootstrap gui/", "bootout gui/"}
	if len(calls) != len(want) {
		t.Fatalf("launchctl calls = %v", calls)
	}
	for i := range want {
		if !strings.HasPrefix(calls[i], want[i]) {
			t.Errorf("call %d = %q, want %q…", i, calls[i], want[i])
		}
	}
}

func TestInstallRefusesGoRunBinary(t *testing.T) {
	env(t)
	dir := filepath.Join(t.TempDir(), "go-build123", "b001", "exe")
	os.MkdirAll(dir, 0o755)
	exe := filepath.Join(dir, "gmwd")
	os.WriteFile(exe, nil, 0o755)
	old := executable
	t.Cleanup(func() { executable = old })
	executable = func() (string, error) { return exe, nil }
	called := false
	oldLC := launchctl
	t.Cleanup(func() { launchctl = oldLC })
	launchctl = func(...string) ([]byte, error) { called = true; return nil, nil }
	if _, err := run(t, "install"); err == nil || !strings.Contains(err.Error(), "go run") {
		t.Errorf("err = %v", err)
	}
	if called {
		t.Error("launchctl was called")
	}
}

func TestParseArgsAllowsFlagsAfterPositionals(t *testing.T) {
	fs := newFlags("x")
	n := fs.Int("n", 0, "")
	v := fs.Bool("v", false, "")
	pos, err := parseArgs(fs, []string{"a", "-n", "3", "b", "-v"})
	if err != nil || *n != 3 || !*v || strings.Join(pos, ",") != "a,b" {
		t.Errorf("pos %v n %d v %v err %v", pos, *n, *v, err)
	}
	if _, err := parseArgs(newFlags("y"), []string{"a", "--nope"}); err == nil {
		t.Error("unknown flag after positional accepted")
	}
}
