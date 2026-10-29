package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitRepo makes a repo on main with one commit and returns its path.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if _, err := git(context.Background(), dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return dir
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestGitGuardsRefuseNonGmwdBranches(t *testing.T) {
	ctx := context.Background()
	repo := gitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := commitAll(ctx, repo, "main", "nope"); err == nil {
		t.Error("commitAll on main succeeded")
	}
	if _, err := commitAll(ctx, repo, "gmwd/x", "nope"); err == nil {
		t.Error("commitAll on a checkout of main claiming gmwd/x succeeded")
	}
	if err := resetHard(ctx, repo, "gmwd/x"); err == nil {
		t.Error("resetHard on main succeeded")
	}
	if !exists(filepath.Join(repo, "x")) {
		t.Error("user's file was removed")
	}
	if err := ensureWorktree(ctx, repo, filepath.Join(t.TempDir(), "wt"), "feature"); err == nil {
		t.Error("ensureWorktree accepted a non-gmwd branch")
	}
}

func TestDefaultBranchPrefersOriginHead(t *testing.T) {
	ctx := context.Background()
	upstream := gitRepo(t)
	clone := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "-q", upstream, clone).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	gitOut(t, clone, "checkout", "-q", "-b", "wip")
	if b, err := defaultBranch(ctx, clone); err != nil || b != "origin/main" {
		t.Errorf("defaultBranch = %q, %v", b, err)
	}
	if b, err := defaultBranch(ctx, upstream); err != nil || b != "main" {
		t.Errorf("defaultBranch = %q, %v", b, err)
	}
}
