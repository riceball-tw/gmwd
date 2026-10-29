package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

func newRunner(t *testing.T, f *fake) *Runner {
	t.Helper()
	return &Runner{Claude: f.claude(), Home: t.TempDir(), Logf: t.Logf}
}

func research(id string) topic.Topic {
	return topic.Topic{ID: id, Title: "Research " + id, Kind: topic.Research, Prompt: "find things out"}
}

func solve(id, repo string) topic.Topic {
	return topic.Topic{ID: id, Title: "Solve " + id, Kind: topic.Solve, Prompt: "fix the thing", Repo: repo}
}

func samePath(t *testing.T, a, b string) bool {
	t.Helper()
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestResearchRunsUntilDone(t *testing.T) {
	f := newFake(t,
		fakeStep{Status: "progress", Summary: "Surveyed three libraries.", Write: map[string]string{"report.md": "# v1\n"}, Tokens: 100},
		fakeStep{Status: "done", Summary: "Compared them; report complete.", Write: map[string]string{"report.md": "# v2\n"}, Tokens: 200},
	)
	r := newRunner(t, f)
	s, err := r.Run(context.Background(), research("libs"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s.StopReason != StopDone || s.TopicStatus != topic.Done {
		t.Fatalf("stop %q status %q, want done", s.StopReason, s.TopicStatus)
	}
	if s.Iterations != 2 || s.Succeeded != 2 || f.calls() != 2 {
		t.Errorf("iterations %d succeeded %d calls %d", s.Iterations, s.Succeeded, f.calls())
	}
	if got := readFile(t, s.Report); got != "# v2\n" {
		t.Errorf("report = %q", got)
	}
	if s.Tokens.Output != 300 || s.Tokens.CacheRead != 1000 {
		t.Errorf("tokens = %+v", s.Tokens)
	}

	notes := readFile(t, r.notesPath("libs"))
	for _, want := range []string{"iteration 1 — progress", "Surveyed three libraries.", "iteration 2 — done", "report complete"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes missing %q:\n%s", want, notes)
		}
	}
	// The second iteration sees the first one's notes.
	if p := argAfter(f.call(2).Args, "-p"); !strings.Contains(p, "Surveyed three libraries.") || !strings.Contains(p, "iteration 2") {
		t.Errorf("iteration 2 prompt lacks notes:\n%s", p)
	}
	if p := argAfter(f.call(1).Args, "-p"); !strings.Contains(p, "first iteration") {
		t.Errorf("iteration 1 prompt should say there are no notes:\n%s", p)
	}
	for _, name := range []string{"iter-001.jsonl", "iter-002.jsonl", "summary.json"} {
		if !exists(filepath.Join(s.RunDir, name)) {
			t.Errorf("missing %s in run dir", name)
		}
	}
	var saved Summary
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(s.RunDir, "summary.json"))), &saved); err != nil || saved.StopReason != StopDone {
		t.Errorf("summary.json: %v %+v", err, saved)
	}
}

func TestResearchInvocationIsScoped(t *testing.T) {
	f := newFake(t, fakeStep{Status: "done", Summary: "ok"})
	r := newRunner(t, f)
	tp := research("scoped")
	tp.Repo = t.TempDir()
	if _, err := r.Run(context.Background(), tp, Options{}); err != nil {
		t.Fatal(err)
	}
	c := f.call(1)
	if !samePath(t, c.Dir, r.researchDir("scoped")) {
		t.Errorf("cwd = %s, want %s", c.Dir, r.researchDir("scoped"))
	}
	want := map[string]string{
		"--permission-mode": "dontAsk",
		"--setting-sources": "project,local",
		"--model":           "sonnet",
		"--output-format":   "stream-json",
		"--json-schema":     outcomeSchema,
		"--add-dir":         tp.Repo,
		"--tools":           "Read,Write,Edit,Glob,Grep,WebSearch,WebFetch",
	}
	for flag, v := range want {
		if got := argAfter(c.Args, flag); got != v {
			t.Errorf("%s = %q, want %q", flag, got, v)
		}
	}
	for _, flag := range []string{"--strict-mcp-config", "--verbose"} {
		if !slices.Contains(c.Args, flag) {
			t.Errorf("missing %s", flag)
		}
	}
	for _, bad := range []string{"--dangerously-skip-permissions", "bypassPermissions", "Bash", "--bare"} {
		if slices.Contains(c.Args, bad) {
			t.Errorf("research args contain %q: %v", bad, c.Args)
		}
	}
	if !slices.Contains(c.Args, "Write(./**)") || slices.Contains(c.Args, "Write") {
		t.Errorf("writes must be scoped to ./**: %v", c.Args)
	}
}

func TestSolveCommitsAndRollsBack(t *testing.T) {
	repo := gitRepo(t)
	mainBefore := gitOut(t, repo, "rev-parse", "main")
	f := newFake(t,
		fakeStep{Status: "progress", Summary: "Added a.", Write: map[string]string{"a.txt": "1"}},
		fakeStep{Status: "failed", Summary: "Broke the build.", Write: map[string]string{"a.txt": "2", "b.txt": "junk"}},
		fakeStep{Status: "done", Summary: "Added c.", Write: map[string]string{"c.txt": "3"}},
	)
	r := newRunner(t, f)
	s, err := r.Run(context.Background(), solve("fix", repo), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s.StopReason != StopDone || s.Succeeded != 2 || s.Failed != 1 {
		t.Fatalf("stop %q succeeded %d failed %d", s.StopReason, s.Succeeded, s.Failed)
	}
	wt := s.WorkDir
	if !samePath(t, wt, r.worktreeDir("fix")) || s.Branch != "gmwd/fix" {
		t.Errorf("worktree %s branch %s", wt, s.Branch)
	}
	if got := readFile(t, filepath.Join(wt, "a.txt")); got != "1" {
		t.Errorf("a.txt = %q; failed iteration not rolled back", got)
	}
	if exists(filepath.Join(wt, "b.txt")) {
		t.Error("b.txt from the failed iteration survived")
	}
	if !exists(filepath.Join(wt, "c.txt")) {
		t.Error("c.txt missing")
	}
	if st := gitOut(t, wt, "status", "--porcelain"); st != "" {
		t.Errorf("worktree dirty:\n%s", st)
	}
	if len(s.Commits) != 2 {
		t.Fatalf("commits = %v", s.Commits)
	}
	if got := gitOut(t, repo, "rev-list", "--count", "main..gmwd/fix"); got != "2" {
		t.Errorf("gmwd/fix is %s commits ahead of main, want 2", got)
	}
	if gitOut(t, repo, "rev-parse", "main") != mainBefore {
		t.Error("main moved")
	}
	if msg := gitOut(t, repo, "log", "-1", "--format=%an %s%n%b", "gmwd/fix"); !strings.Contains(msg, "gmwd gmwd(fix): iteration 3") || !strings.Contains(msg, "Added c.") {
		t.Errorf("commit message = %q", msg)
	}
	if notes := readFile(t, r.notesPath("fix")); !strings.Contains(notes, "Broke the build.") || !strings.Contains(notes, "discarded") {
		t.Errorf("notes should record the discarded iteration:\n%s", notes)
	}
}

func TestSolveInvocationIsSandboxed(t *testing.T) {
	repo := gitRepo(t)
	f := newFake(t, fakeStep{Status: "done", Summary: "ok"})
	r := newRunner(t, f)
	if _, err := r.Run(context.Background(), solve("sbx", repo), Options{}); err != nil {
		t.Fatal(err)
	}
	c := f.call(1)
	if !samePath(t, c.Dir, r.worktreeDir("sbx")) {
		t.Errorf("cwd = %s", c.Dir)
	}
	if got := argAfter(c.Args, "--tools"); !strings.Contains(got, "Bash") {
		t.Errorf("--tools = %q, want Bash available", got)
	}
	// Bash must not be pre-approved: that would allow unsandboxed runs.
	i := slices.Index(c.Args, "--allowedTools")
	for _, a := range c.Args[i+1:] {
		if strings.HasPrefix(a, "--") {
			break
		}
		if strings.HasPrefix(a, "Bash") {
			t.Errorf("Bash pre-approved: %v", c.Args)
		}
	}
	var settings struct {
		Sandbox struct {
			Enabled                  bool  `json:"enabled"`
			AutoAllowBashIfSandboxed bool  `json:"autoAllowBashIfSandboxed"`
			AllowUnsandboxedCommands *bool `json:"allowUnsandboxedCommands"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(argAfter(c.Args, "--settings")), &settings); err != nil {
		t.Fatal(err)
	}
	sb := settings.Sandbox
	if !sb.Enabled || !sb.AutoAllowBashIfSandboxed || sb.AllowUnsandboxedCommands == nil || *sb.AllowUnsandboxedCommands {
		t.Errorf("sandbox settings = %+v", sb)
	}
	if !slices.ContainsFunc(c.Env, func(kv string) bool { return strings.HasPrefix(kv, "GOCACHE=") }) {
		t.Error("GOCACHE not redirected")
	}
}

func TestFailuresStopTopic(t *testing.T) {
	f := newFake(t,
		fakeStep{NoResult: true, ExitCode: 1},
		fakeStep{IsError: true, Summary: "boom"},
		fakeStep{Status: "failed", Summary: "stuck"},
		fakeStep{Status: "done", Summary: "never reached"},
	)
	s, err := newRunner(t, f).Run(context.Background(), research("flaky"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s.StopReason != StopFailures || s.TopicStatus != topic.Failed || s.Failed != 3 || f.calls() != 3 {
		t.Errorf("stop %q status %q failed %d calls %d", s.StopReason, s.TopicStatus, s.Failed, f.calls())
	}
}

func TestSuccessResetsFailureCount(t *testing.T) {
	f := newFake(t,
		fakeStep{Status: "failed", Summary: "x"},
		fakeStep{Status: "failed", Summary: "x"},
		fakeStep{Status: "progress", Summary: "y"},
		fakeStep{Status: "failed", Summary: "x"},
		fakeStep{Status: "failed", Summary: "x"},
		fakeStep{Status: "done", Summary: "z"},
	)
	s, err := newRunner(t, f).Run(context.Background(), research("bumpy"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s.StopReason != StopDone || s.Failed != 4 || s.Iterations != 6 {
		t.Errorf("stop %q failed %d iterations %d", s.StopReason, s.Failed, s.Iterations)
	}
}

func TestIterationBudget(t *testing.T) {
	f := newFake(t, fakeStep{Status: "progress", Summary: "more"})
	s, err := newRunner(t, f).Run(context.Background(), research("long"), Options{Budget: Budget{Iterations: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if s.StopReason != StopIterations || s.Iterations != 3 || s.TopicStatus != topic.Queued {
		t.Errorf("stop %q iterations %d status %q", s.StopReason, s.Iterations, s.TopicStatus)
	}
}

func TestTokenBudget(t *testing.T) {
	// 10 input + 990 output = 1000 job tokens per iteration; cache reads don't count.
	f := newFake(t, fakeStep{Status: "progress", Summary: "more", Tokens: 990})
	s, err := newRunner(t, f).Run(context.Background(), research("tok"), Options{Budget: Budget{Tokens: 2500}})
	if err != nil {
		t.Fatal(err)
	}
	if s.StopReason != StopTokens || s.Iterations != 3 {
		t.Errorf("stop %q iterations %d tokens %+v", s.StopReason, s.Iterations, s.Tokens)
	}
}

func TestTopicMaxTokensTightensBudget(t *testing.T) {
	f := newFake(t, fakeStep{Status: "progress", Summary: "more", Tokens: 990})
	tp := research("cap")
	tp.MaxTokens = 1500
	s, err := newRunner(t, f).Run(context.Background(), tp, Options{Budget: Budget{Tokens: 1_000_000}})
	if err != nil {
		t.Fatal(err)
	}
	if s.StopReason != StopTokens || s.Iterations != 2 {
		t.Errorf("stop %q iterations %d", s.StopReason, s.Iterations)
	}
}

func TestWindowBudget(t *testing.T) {
	reset := time.Now().Add(3 * time.Hour).Truncate(10 * time.Minute).Unix()
	step := func(u float64) fakeStep {
		return fakeStep{Status: "progress", Summary: "more", RateLimit: &fakeRateLimit{Status: "allowed", FiveHour: u, SevenDay: 0.3, ResetsAt: reset}}
	}
	f := newFake(t, step(0.13), step(0.16), step(0.19))
	start := &usage.Snapshot{FiveHour: usage.Window{Kind: usage.FiveHour, Utilization: 0.10, ResetsAt: time.Unix(reset, 0)}}
	s, err := newRunner(t, f).Run(context.Background(), research("win"), Options{Start: start, Budget: Budget{WindowPct: 0.05}})
	if err != nil {
		t.Fatal(err)
	}
	if s.StopReason != StopWindow || s.Iterations != 2 {
		t.Errorf("stop %q iterations %d spent %.2f", s.StopReason, s.Iterations, s.WindowSpent)
	}
	if s.LastUsage == nil || s.LastUsage.FiveHour.Utilization != 0.16 {
		t.Errorf("last usage = %+v", s.LastUsage)
	}
}

func TestReserveKillsIterationAndDiscardsWork(t *testing.T) {
	repo := gitRepo(t)
	f := newFake(t, fakeStep{
		Status: "progress", Summary: "half done", Write: map[string]string{"partial.txt": "x"},
		RateLimit:           &fakeRateLimit{Status: "allowed", FiveHour: 0.90, SevenDay: 0.5, ResetsAt: time.Now().Add(time.Hour).Unix()},
		SleepAfterRateLimit: 30 * time.Second,
	})
	r := newRunner(t, f)
	began := time.Now()
	s, err := r.Run(context.Background(), solve("hog", repo), Options{Budget: Budget{ReservePct: 0.15}})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 15*time.Second {
		t.Errorf("took %v; iteration should be killed as soon as the reserve is hit", took)
	}
	if s.StopReason != StopReserve || s.TopicStatus != topic.Queued {
		t.Errorf("stop %q status %q", s.StopReason, s.TopicStatus)
	}
	if exists(filepath.Join(s.WorkDir, "partial.txt")) || len(s.Commits) != 0 {
		t.Error("work from the killed iteration was kept")
	}
}

func TestReserveAlreadyBreachedDoesNotStart(t *testing.T) {
	f := newFake(t, fakeStep{Status: "done", Summary: "x"})
	start := &usage.Snapshot{FiveHour: usage.Window{Kind: usage.FiveHour, Utilization: 0.86}}
	s, err := newRunner(t, f).Run(context.Background(), research("late"), Options{Start: start, Budget: Budget{ReservePct: 0.15}})
	if err != nil {
		t.Fatal(err)
	}
	if s.StopReason != StopReserve || f.calls() != 0 {
		t.Errorf("stop %q calls %d", s.StopReason, f.calls())
	}
}

func TestRateLimitStopsWithoutCountingFailure(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).Truncate(10 * time.Minute).Unix()
	f := newFake(t,
		fakeStep{Status: "progress", Summary: "ok"},
		fakeStep{IsError: true, APIErrorStatus: 429, Summary: "You've hit your limit",
			RateLimit: &fakeRateLimit{Status: "rejected", FiveHour: 1, SevenDay: 0.6, ResetsAt: reset}},
	)
	s, err := newRunner(t, f).Run(context.Background(), research("limited"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s.StopReason != StopRateLimited || s.TopicStatus != topic.Queued || s.Failed != 0 {
		t.Errorf("stop %q status %q failed %d", s.StopReason, s.TopicStatus, s.Failed)
	}
	if s.ResetsAt.Unix() != reset {
		t.Errorf("resetsAt = %v, want %v", s.ResetsAt, time.Unix(reset, 0))
	}
}

func TestCancelStopsPromptly(t *testing.T) {
	repo := gitRepo(t)
	f := newFake(t, fakeStep{Status: "progress", Summary: "slow", Write: map[string]string{"w.txt": "x"}, SleepAfterRateLimit: 30 * time.Second})
	r := newRunner(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	r.Logf = func(format string, args ...any) {
		if strings.HasPrefix(format, "iteration %d: running") {
			time.AfterFunc(500*time.Millisecond, cancel)
		}
	}
	began := time.Now()
	s, err := r.Run(ctx, solve("stop", repo), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 15*time.Second {
		t.Errorf("took %v after cancel", took)
	}
	if s.StopReason != StopCancelled {
		t.Errorf("stop %q", s.StopReason)
	}
	if exists(filepath.Join(s.WorkDir, "w.txt")) {
		t.Error("cancelled iteration's work was kept")
	}
}

func TestSecondRunResumesTopic(t *testing.T) {
	repo := gitRepo(t)
	home := t.TempDir()
	f1 := newFake(t, fakeStep{Status: "progress", Summary: "First run did A.", Write: map[string]string{"a.txt": "a"}})
	r1 := &Runner{Claude: f1.claude(), Home: home}
	s1, err := r1.Run(context.Background(), solve("resume", repo), Options{Budget: Budget{Iterations: 1}})
	if err != nil {
		t.Fatal(err)
	}
	// A crash left junk in the worktree.
	if err := os.WriteFile(filepath.Join(s1.WorkDir, "junk.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	f2 := newFake(t, fakeStep{Status: "done", Summary: "Second run did B.", Write: map[string]string{"b.txt": "b"}})
	r2 := &Runner{Claude: f2.claude(), Home: home, Now: func() time.Time { return time.Now().Add(time.Second) }}
	s2, err := r2.Run(context.Background(), solve("resume", repo), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s2.WorkDir != s1.WorkDir || s2.RunID == s1.RunID {
		t.Errorf("workdir %s vs %s, run ids %s %s", s2.WorkDir, s1.WorkDir, s2.RunID, s1.RunID)
	}
	if exists(filepath.Join(s2.WorkDir, "junk.txt")) {
		t.Error("leftover junk not cleaned before the run")
	}
	if p := argAfter(f2.call(1).Args, "-p"); !strings.Contains(p, "First run did A.") {
		t.Errorf("second run doesn't see first run's notes:\n%s", p)
	}
	if got := gitOut(t, repo, "rev-list", "--count", "main..gmwd/resume"); got != "2" {
		t.Errorf("branch has %s commits, want 2", got)
	}
}

func TestSolveRequiresGitRepo(t *testing.T) {
	f := newFake(t, fakeStep{Status: "done", Summary: "x"})
	_, err := newRunner(t, f).Run(context.Background(), solve("nogit", t.TempDir()), Options{})
	if err == nil || f.calls() != 0 {
		t.Errorf("err %v calls %d", err, f.calls())
	}
}

func TestWindowTrackerAcrossReset(t *testing.T) {
	r1 := time.Unix(1_800_000_000, 0)
	r2 := r1.Add(5 * time.Hour)
	w := &windowTracker{}
	w.observe(usage.Window{Kind: usage.FiveHour, Utilization: 0.70, ResetsAt: r1})
	w.observe(usage.Window{Kind: usage.FiveHour, Utilization: 0.80, ResetsAt: r1})
	w.observe(usage.Window{Kind: usage.FiveHour, Utilization: 0.05, ResetsAt: r2})
	if got := w.spent(); got < 0.149 || got > 0.151 {
		t.Errorf("spent = %v, want 0.15", got)
	}
	w.observe(usage.Window{}) // no data: ignored
	if !w.known() || w.last != 0.05 {
		t.Errorf("tracker changed by empty window: %+v", w)
	}
}

func TestReadTailStartsAtSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.md")
	var b strings.Builder
	for i := range 50 {
		b.WriteString("## iteration " + strings.Repeat("x", i%7) + "\nsome text here\n\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readTail(path, 200)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "## ") || len(got) > 200 || !strings.HasSuffix(b.String(), got) {
		t.Errorf("tail = %q", got)
	}
	if got, err := readTail(filepath.Join(t.TempDir(), "none"), 10); got != "" || err != nil {
		t.Errorf("missing file: %q %v", got, err)
	}
}
