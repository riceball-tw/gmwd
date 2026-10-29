package runner

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

// Budget limits one run. Zero values mean "no limit" except where noted.
type Budget struct {
	// Iterations caps the number of claude calls; 0 means DefaultIterations.
	Iterations int
	// Tokens caps input + output + cache-write tokens. Cache reads are not
	// counted: they are cheap against limits and dominate raw totals.
	Tokens int64
	// WindowPct caps how far the run may push 5h utilization (0..1).
	WindowPct float64
	// ReservePct is the share of the 5h window left for the user (0..1); the
	// run is killed mid-iteration when utilization reaches 1-ReservePct.
	ReservePct float64
}

// DefaultIterations is used when Budget.Iterations is 0.
const DefaultIterations = 10

// MaxConsecutiveFailures marks a topic failed.
const MaxConsecutiveFailures = 3

// StopReason says why a run ended.
type StopReason string

const (
	StopDone        StopReason = "done"
	StopIterations  StopReason = "iterations"
	StopTokens      StopReason = "tokens"
	StopWindow      StopReason = "window_budget"
	StopReserve     StopReason = "reserve"
	StopRateLimited StopReason = "rate_limited"
	StopFailures    StopReason = "failures"
	StopCancelled   StopReason = "cancelled"
)

// Runner runs topics. Home is gmwd's data dir (~/.gmwd).
type Runner struct {
	Claude Claude
	Home   string
	Now    func() time.Time
	// Logf receives one line per notable step; may be nil.
	Logf func(format string, args ...any)
}

// Options are per-run inputs.
type Options struct {
	Budget Budget
	// Start is the usage before the run, if known (e.g. from /usage). It is
	// the baseline for Budget.WindowPct; without it the first
	// rate_limit_event is used.
	Start *usage.Snapshot
}

// Summary is the outcome of a run.
type Summary struct {
	TopicID     string            `json:"topicId"`
	RunID       string            `json:"runId"`
	RunDir      string            `json:"runDir"`
	Iterations  int               `json:"iterations"`
	Succeeded   int               `json:"succeeded"`
	Failed      int               `json:"failed"`
	Tokens      usage.TokenCounts `json:"tokens"`
	CostUSD     float64           `json:"costUSD"`
	WindowSpent float64           `json:"windowSpent"`
	StopReason  StopReason        `json:"stopReason"`
	TopicStatus topic.Status      `json:"topicStatus"`
	Commits     []string          `json:"commits,omitempty"`
	Branch      string            `json:"branch,omitempty"`
	WorkDir     string            `json:"workDir"`
	Report      string            `json:"report,omitempty"`
	// ResetsAt is when to retry after StopRateLimited.
	ResetsAt  time.Time       `json:"resetsAt,omitzero"`
	LastUsage *usage.Snapshot `json:"lastUsage,omitempty"`
	Sessions  []string        `json:"sessions"`
}

// Outcome is the structured result every iteration must end with.
type Outcome struct {
	Status  string `json:"status"` // progress | done | failed
	Summary string `json:"summary"`
}

const outcomeSchema = `{"type":"object","properties":{"status":{"type":"string","enum":["progress","done","failed"]},"summary":{"type":"string"}},"required":["status","summary"]}`

// Paths.

func (r *Runner) topicDir(id string) string    { return filepath.Join(r.Home, "topics", id) }
func (r *Runner) notesPath(id string) string   { return filepath.Join(r.topicDir(id), "notes.md") }
func (r *Runner) researchDir(id string) string { return filepath.Join(r.topicDir(id), "work") }
func (r *Runner) worktreeDir(id string) string { return filepath.Join(r.Home, "worktrees", id) }

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// Run works on t until it is done or a limit is reached.
func (r *Runner) Run(ctx context.Context, t topic.Topic, opts Options) (Summary, error) {
	if err := t.Validate(); err != nil {
		return Summary{}, err
	}
	b := opts.Budget
	if b.Iterations == 0 {
		b.Iterations = DefaultIterations
	}
	if t.MaxTokens > 0 && (b.Tokens == 0 || t.MaxTokens < b.Tokens) {
		b.Tokens = t.MaxTokens
	}

	runID := r.now().Format("20060102-150405")
	s := Summary{TopicID: t.ID, RunID: runID, RunDir: filepath.Join(r.topicDir(t.ID), "runs", runID)}
	if err := os.MkdirAll(s.RunDir, 0o700); err != nil {
		return s, err
	}
	ws, err := r.prepare(ctx, t, &s)
	if err != nil {
		return s, err
	}

	win := &windowTracker{}
	if opts.Start != nil {
		win.observe(opts.Start.FiveHour)
	}
	failures := 0
	for {
		if reason, stop := r.checkLimits(ctx, b, s, win, failures); stop {
			s.StopReason = reason
			break
		}
		s.Iterations++
		it, err := r.iterate(ctx, t, ws, b, win, &s)
		if err != nil {
			return r.finish(t, s), err
		}
		if it.reason != "" {
			s.StopReason = it.reason
			break
		}
		if it.ok {
			failures = 0
			s.Succeeded++
		} else {
			failures++
			s.Failed++
		}
		if it.done {
			s.StopReason = StopDone
			break
		}
	}
	return r.finish(t, s), nil
}

// workspace is where a topic's iterations run.
type workspace struct {
	dir    string
	branch string // solve only
	addDir []string
}

func (r *Runner) prepare(ctx context.Context, t topic.Topic, s *Summary) (workspace, error) {
	switch t.Kind {
	case topic.Research:
		ws := workspace{dir: r.researchDir(t.ID)}
		if t.Repo != "" {
			ws.addDir = []string{t.Repo}
		}
		s.WorkDir = ws.dir
		s.Report = filepath.Join(ws.dir, "report.md")
		return ws, os.MkdirAll(ws.dir, 0o700)
	case topic.Solve:
		if err := checkRepo(ctx, t.Repo); err != nil {
			return workspace{}, err
		}
		ws := workspace{dir: r.worktreeDir(t.ID), branch: t.Branch()}
		if err := ensureWorktree(ctx, t.Repo, ws.dir, ws.branch); err != nil {
			return workspace{}, err
		}
		// Start every run from a clean tree, as the last run may have died
		// mid-iteration.
		if err := resetHard(ctx, ws.dir, ws.branch); err != nil {
			return workspace{}, err
		}
		s.WorkDir, s.Branch = ws.dir, ws.branch
		return ws, nil
	}
	return workspace{}, fmt.Errorf("unknown kind %q", t.Kind)
}

func (r *Runner) checkLimits(ctx context.Context, b Budget, s Summary, win *windowTracker, failures int) (StopReason, bool) {
	switch {
	case ctx.Err() != nil:
		return StopCancelled, true
	case failures >= MaxConsecutiveFailures:
		return StopFailures, true
	case s.Iterations >= b.Iterations:
		return StopIterations, true
	case b.Tokens > 0 && jobTokens(s.Tokens) >= b.Tokens:
		return StopTokens, true
	case b.WindowPct > 0 && win.spent() >= b.WindowPct:
		return StopWindow, true
	case b.ReservePct > 0 && win.known() && win.last >= 1-b.ReservePct:
		return StopReserve, true
	}
	return "", false
}

func jobTokens(c usage.TokenCounts) int64 { return c.Input + c.Output + c.CacheCreate }

type iterResult struct {
	ok, done bool
	reason   StopReason // set when the run must stop now
}

func (r *Runner) iterate(ctx context.Context, t topic.Topic, ws workspace, b Budget, win *windowTracker, s *Summary) (iterResult, error) {
	n := s.Iterations
	notes, err := readTail(r.notesPath(t.ID), 12<<10)
	if err != nil {
		return iterResult{}, err
	}
	inv := r.invocation(t, ws, n, notes)
	s.Sessions = append(s.Sessions, inv.SessionID)

	logPath := filepath.Join(s.RunDir, fmt.Sprintf("iter-%03d.jsonl", n))
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return iterResult{}, err
	}
	defer logf.Close()

	r.logf("iteration %d: running %s (%s)", n, t.ID, inv.Model)
	ictx, kill := context.WithCancel(ctx)
	defer kill()
	var mu sync.Mutex
	var killReason StopReason
	res, runErr := r.Claude.Run(ictx, inv, logf, func(snap usage.Snapshot) {
		mu.Lock()
		defer mu.Unlock()
		win.observe(snap.FiveHour)
		switch {
		case snap.Status == "rejected":
			killReason = StopRateLimited
		case b.ReservePct > 0 && snap.FiveHour.Utilization >= 1-b.ReservePct:
			killReason = StopReserve
		default:
			return
		}
		kill()
	})
	mu.Lock()
	reason := killReason
	mu.Unlock()

	addTokens(&s.Tokens, res.Usage)
	s.CostUSD += res.CostUSD
	s.WindowSpent = win.spent()
	if res.RateLimit != nil {
		snap := *res.RateLimit
		s.LastUsage = &snap
	}
	if res.Rejected() {
		reason = StopRateLimited
	}
	if reason == StopRateLimited && res.RateLimit != nil {
		s.ResetsAt = res.RateLimit.FiveHour.ResetsAt
		if res.RateLimit.SevenDay.Utilization >= 1 {
			s.ResetsAt = res.RateLimit.SevenDay.ResetsAt
		}
	}
	if reason == "" && ctx.Err() != nil {
		reason = StopCancelled
	}

	out, why := outcome(res, runErr)
	if reason != "" {
		// Killed or limited mid-iteration: the work is incomplete.
		out, why = Outcome{}, "stopped: "+string(reason)
	}
	ok := why == "" && out.Status != "failed"

	commit, err := r.settle(t, ws, ok, n, out)
	if err != nil {
		return iterResult{}, err
	}
	if commit != "" {
		s.Commits = append(s.Commits, commit)
	}
	if err := r.appendNotes(t.ID, n, ok, out, why, commit, res); err != nil {
		return iterResult{}, err
	}
	r.logf("iteration %d: %s (tokens %d, 5h spent %.0f%%)", n, describe(ok, out, why), jobTokens(res.Usage), win.spent()*100)
	return iterResult{ok: ok, done: ok && out.Status == "done", reason: reason}, nil
}

// settle commits a successful solve iteration and rolls back a failed one.
// It ignores ctx so a cancelled run still leaves a clean tree.
func (r *Runner) settle(t topic.Topic, ws workspace, ok bool, n int, out Outcome) (string, error) {
	if t.Kind != topic.Solve {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if !ok {
		return "", resetHard(ctx, ws.dir, ws.branch)
	}
	msg := fmt.Sprintf("gmwd(%s): iteration %d\n\n%s", t.ID, n, out.Summary)
	return commitAll(ctx, ws.dir, ws.branch, msg)
}

// outcome decides whether an iteration succeeded. why is "" on success.
func outcome(res Result, runErr error) (Outcome, string) {
	if runErr != nil {
		return Outcome{}, runErr.Error()
	}
	if !res.GotResult {
		return Outcome{}, "claude exited without a result"
	}
	if res.IsError {
		return Outcome{}, fmt.Sprintf("claude error (%s): %s", res.Subtype, firstLine(res.Text))
	}
	var out Outcome
	if len(res.Structured) == 0 || json.Unmarshal(res.Structured, &out) != nil {
		return Outcome{}, "no structured result"
	}
	switch out.Status {
	case "progress", "done", "failed":
	default:
		return out, fmt.Sprintf("unknown status %q", out.Status)
	}
	return out, ""
}

func describe(ok bool, out Outcome, why string) string {
	switch {
	case why != "":
		return "failed: " + why
	case !ok:
		return "failed: " + firstLine(out.Summary)
	}
	return out.Status + ": " + firstLine(out.Summary)
}

func (r *Runner) finish(t topic.Topic, s Summary) Summary {
	switch s.StopReason {
	case StopDone:
		s.TopicStatus = topic.Done
	case StopFailures:
		s.TopicStatus = topic.Failed
	default:
		s.TopicStatus = topic.Queued
	}
	if data, err := json.MarshalIndent(s, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(s.RunDir, "summary.json"), data, 0o600)
	}
	return s
}

func (r *Runner) appendNotes(id string, n int, ok bool, out Outcome, why, commit string, res Result) error {
	f, err := os.OpenFile(r.notesPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	status := out.Status
	if !ok {
		status = "failed"
	}
	fmt.Fprintf(f, "## %s — iteration %d — %s\n", r.now().Format("2006-01-02 15:04"), n, status)
	if out.Summary != "" {
		fmt.Fprintf(f, "%s\n", strings.TrimSpace(out.Summary))
	}
	if why != "" {
		fmt.Fprintf(f, "(gmwd: %s)\n", why)
	}
	if !ok {
		fmt.Fprintf(f, "(gmwd: changes from this iteration were discarded)\n")
	}
	if commit != "" {
		fmt.Fprintf(f, "(gmwd: committed %s)\n", commit[:min(12, len(commit))])
	}
	if res.Denials > 0 {
		fmt.Fprintf(f, "(gmwd: %d tool calls were denied; stay inside the working directory)\n", res.Denials)
	}
	_, err = fmt.Fprintln(f)
	return err
}

func addTokens(dst *usage.TokenCounts, c usage.TokenCounts) {
	dst.Input += c.Input
	dst.Output += c.Output
	dst.CacheCreate += c.CacheCreate
	dst.CacheRead += c.CacheRead
	dst.Requests += c.Requests
}

// readTail returns the last limit bytes of a file, or "" if it doesn't exist.
func readTail(path string, limit int64) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	off := max(st.Size()-limit, 0)
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return "", err
	}
	s := string(buf)
	if off > 0 {
		// Start at a section boundary.
		if i := strings.Index(s, "\n## "); i >= 0 {
			s = s[i+1:]
		}
	}
	return s, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// windowTracker measures how much 5h utilization a run has used, across a
// window reset.
type windowTracker struct {
	resetsAt   time.Time
	base, last float64
	carried    float64 // spent in earlier windows
	seen       bool
}

func (w *windowTracker) observe(win usage.Window) {
	if win.Kind == "" {
		return
	}
	switch {
	case !w.seen:
		w.seen, w.resetsAt, w.base = true, win.ResetsAt, win.Utilization
	case !win.ResetsAt.IsZero() && !win.ResetsAt.Equal(w.resetsAt):
		w.carried += w.last - w.base
		w.resetsAt, w.base = win.ResetsAt, 0
	}
	w.last = win.Utilization
}

func (w *windowTracker) known() bool { return w.seen }

func (w *windowTracker) spent() float64 {
	if !w.seen {
		return 0
	}
	return max(w.carried+w.last-w.base, 0)
}
