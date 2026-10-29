package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLastNote(t *testing.T) {
	p := Paths{Home: t.TempDir()}
	if got, err := p.LastNote("none"); got != "" || err != nil {
		t.Errorf("no notes: %q %v", got, err)
	}
	os.MkdirAll(p.Topic("x"), 0o700)
	os.WriteFile(p.Notes("x"), []byte("## a — iteration 1 — progress\nfirst\n\n## b — iteration 2 — done\nsecond\n(gmwd: committed abc)\n\n"), 0o600)
	got, err := p.LastNote("x")
	if err != nil || got != "## b — iteration 2 — done\nsecond\n(gmwd: committed abc)" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestBranchStat(t *testing.T) {
	ctx := context.Background()
	repo := gitRepo(t)
	if info, err := BranchStat(ctx, repo, "gmwd/none"); err != nil || info.Exists {
		t.Errorf("missing branch: %+v %v", info, err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := ensureWorktree(ctx, repo, wt, "gmwd/x"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("1\n2\n"), 0o600)
	if _, err := commitAll(ctx, wt, "gmwd/x", "one"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt, "b.txt"), []byte("3\n"), 0o600)
	commitAll(ctx, wt, "gmwd/x", "two")
	info, err := BranchStat(ctx, repo, "gmwd/x")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Exists || info.Base != "main" || info.Ahead != 2 || !strings.Contains(info.ShortStat, "2 files changed, 3 insertions(+)") {
		t.Errorf("info = %+v", info)
	}
	if _, err := BranchStat(ctx, filepath.Join(t.TempDir(), "gone"), "gmwd/x"); err == nil {
		t.Error("missing repo: no error")
	}
}
