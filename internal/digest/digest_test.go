package digest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gmwd/internal/runner"
	"gmwd/internal/store"
	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

var ctx = context.Background()

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// setup makes a store with a day of work: a solve topic with two runs and a
// branch, a research topic with a report, a failed topic, and a run on the
// day before that must not count.
func setup(t *testing.T) (*store.Store, string, time.Time, string) {
	t.Helper()
	home := t.TempDir()
	st, err := store.Open(filepath.Join(home, "gmwd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	repo := t.TempDir()
	mustGit(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o600)
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-q", "-m", "init")
	mustGit(t, repo, "checkout", "-q", "-b", "gmwd/fix-auth")
	os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o600)
	mustGit(t, repo, "commit", "-q", "-am", "fix")
	mustGit(t, repo, "checkout", "-q", "main")

	paths := runner.Paths{Home: home}
	for _, tp := range []topic.Topic{
		{ID: "fix-auth", Title: "Fix flaky auth tests", Kind: topic.Solve, Prompt: "p", Repo: repo, Priority: 3},
		{ID: "sqlite", Title: "Compare SQLite drivers", Kind: topic.Research, Prompt: "p", Priority: 3},
		{ID: "broken", Title: "Broken one", Kind: topic.Research, Prompt: "p", Priority: 3},
	} {
		if _, err := st.AddTopic(ctx, tp); err != nil {
			t.Fatal(err)
		}
	}
	st.SetTopicStatus(ctx, "sqlite", topic.Done)
	st.SetTopicStatus(ctx, "broken", topic.Failed)
	os.MkdirAll(paths.Work("sqlite"), 0o700)
	os.WriteFile(paths.Report("sqlite"), []byte("# report\n"), 0o600)
	os.MkdirAll(paths.Topic("fix-auth"), 0o700)
	os.WriteFile(paths.Notes("fix-auth"), []byte("## 2026-10-29 03:10 — iteration 1 — progress\nMade A.\n\n## 2026-10-29 04:00 — iteration 2 — progress\nAdded func A; tests pass.\n(gmwd: committed abc123)\n\n"), 0o600)

	day := time.Date(2026, 10, 29, 15, 0, 0, 0, time.Local)
	at := func(h int) time.Time { return time.Date(2026, 10, 29, h, 0, 0, 0, time.Local) }
	add := func(r store.Run) {
		id, err := st.StartRun(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		r.ID = id
		if err := st.FinishRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	add(store.Run{TopicID: "fix-auth", StartedAt: at(3), EndedAt: at(3).Add(40 * time.Minute), StopReason: "window_budget", Iterations: 2, WindowSpent: 0.04,
		Tokens: usage.TokenCounts{Input: 100, Output: 9_900, CacheRead: 500_000}, Commits: []string{"a1"}})
	add(store.Run{TopicID: "sqlite", StartedAt: at(5), EndedAt: at(5).Add(10 * time.Minute), StopReason: "done", Iterations: 1, WindowSpent: 0.02,
		Tokens: usage.TokenCounts{Output: 5_000}})
	add(store.Run{TopicID: "fix-auth", StartedAt: at(9), EndedAt: at(9).Add(5 * time.Minute), StopReason: "user_active", Iterations: 1, WindowSpent: 0.01,
		Tokens: usage.TokenCounts{Output: 5_000}})
	add(store.Run{TopicID: "broken", StartedAt: at(10), EndedAt: at(10).Add(time.Minute), StopReason: "error: not a git repository"})
	add(store.Run{TopicID: "sqlite", StartedAt: at(0).Add(-time.Hour), StopReason: "done", Iterations: 9, WindowSpent: 0.5}) // the day before
	return st, home, day, repo
}

func TestBuild(t *testing.T) {
	st, home, day, _ := setup(t)
	d, err := Build(ctx, st, home, day, 0.15)
	if err != nil {
		t.Fatal(err)
	}
	if d.Runs != 4 || d.Iterations != 4 || len(d.Topics) != 3 || d.Tokens.Output != 19_900 {
		t.Errorf("totals: runs %d iterations %d topics %d tokens %+v", d.Runs, d.Iterations, len(d.Topics), d.Tokens)
	}
	if d.WindowSpent < 0.0699 || d.WindowSpent > 0.0701 {
		t.Errorf("window spent %v", d.WindowSpent)
	}
	fix := d.Topics[0]
	if fix.ID != "fix-auth" || fix.Runs != 2 || fix.NewCommits != 1 || strings.Join(fix.Stops, ",") != "window_budget,user_active" {
		t.Errorf("fix-auth = %+v", fix)
	}
	if fix.Branch == nil || fix.Branch.Ahead != 1 || fix.Branch.Base != "main" || !strings.Contains(fix.Branch.ShortStat, "1 file changed") {
		t.Errorf("branch = %+v", fix.Branch)
	}
	if !strings.Contains(fix.LastNote, "Added func A; tests pass.") || strings.Contains(fix.LastNote, "Made A.") {
		t.Errorf("last note = %q", fix.LastNote)
	}
	if d.Topics[1].Report == "" || d.Topics[1].Status != topic.Done {
		t.Errorf("sqlite = %+v", d.Topics[1])
	}
}

func TestMarkdown(t *testing.T) {
	st, home, day, repo := setup(t)
	d, _ := Build(ctx, st, home, day, 0.15)
	md := d.Markdown()
	for _, want := range []string{
		"# gmwd digest — Thu 29 Oct 2026",
		"gmwd ran 4 jobs on 3 topics: 4 iterations, 20.0k tokens (+500.0k cache reads), 7% of 5h windows (≈1% of the week).",
		"## To review",
		"- **Fix flaky auth tests** (solve) — 1 new commit on `gmwd/fix-auth` (1 file changed, 2 insertions(+))",
		"`git -C " + repo + " log --stat main..gmwd/fix-auth`",
		"- **Compare SQLite drivers** (research) — report finished",
		"- **Broken one** (research) — failed — needs attention",
		"### Fix flaky auth tests · `fix-auth` · queued",
		"- 2 runs, 3 iterations, 15.0k tokens, 5% of 5h windows",
		"- stopped: window_budget, user_active",
		"- branch `gmwd/fix-auth`: 1 commit ahead of main",
		"  > Added func A; tests pass.",
		"- report: " + filepath.Join(home, "topics", "sqlite", "work", "report.md"),
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
}

func TestEmptyDay(t *testing.T) {
	st, home, _, _ := setup(t)
	d, err := Build(ctx, st, home, time.Date(2026, 11, 5, 12, 0, 0, 0, time.Local), 0.15)
	if err != nil {
		t.Fatal(err)
	}
	if d.Runs != 0 || !strings.Contains(d.Markdown(), "No gmwd runs on this day.") {
		t.Errorf("got %+v\n%s", d, d.Markdown())
	}
}

func TestWriteAndDays(t *testing.T) {
	st, home, day, _ := setup(t)
	if days, err := Days(home); err != nil || len(days) != 0 {
		t.Errorf("no digests yet: %v %v", days, err)
	}
	d, _ := Build(ctx, st, home, day, 0.15)
	p, err := Write(home, d)
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(home, "digest", "2026-10-29.md") {
		t.Errorf("path = %s", p)
	}
	b, _ := os.ReadFile(p)
	if string(b) != d.Markdown() {
		t.Error("file differs from markdown")
	}
	prev, _ := Build(ctx, st, home, day.AddDate(0, 0, -1), 0.15)
	Write(home, prev)
	os.WriteFile(filepath.Join(home, "digest", "notes.md"), nil, 0o600) // not a digest
	days, err := Days(home)
	if err != nil || strings.Join(days, ",") != "2026-10-29,2026-10-28" {
		t.Errorf("days = %v %v", days, err)
	}
}
