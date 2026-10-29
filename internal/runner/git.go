package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// git runs git in dir and returns trimmed stdout.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(MinimalEnv(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=gmwd", "GIT_AUTHOR_EMAIL=gmwd@localhost",
		"GIT_COMMITTER_NAME=gmwd", "GIT_COMMITTER_EMAIL=gmwd@localhost",
	)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// defaultBranch is origin's HEAD if known, else main or master, else the
// repo's current branch.
func defaultBranch(ctx context.Context, repo string) (string, error) {
	if ref, err := git(ctx, repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return ref, nil // e.g. "origin/main"
	}
	for _, b := range []string{"main", "master"} {
		if _, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+b); err == nil {
			return b, nil
		}
	}
	return git(ctx, repo, "symbolic-ref", "--short", "HEAD")
}

// ensureWorktree makes sure path is a worktree of repo on branch, creating
// the branch from the default branch if needed.
func ensureWorktree(ctx context.Context, repo, path, branch string) error {
	if err := checkBranchName(branch); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		return checkOnBranch(ctx, path, branch)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		_, err := git(ctx, repo, "worktree", "add", path, branch)
		return err
	}
	base, err := defaultBranch(ctx, repo)
	if err != nil {
		return fmt.Errorf("find default branch: %w", err)
	}
	_, err = git(ctx, repo, "worktree", "add", "-b", branch, path, base)
	return err
}

func checkBranchName(branch string) error {
	if !strings.HasPrefix(branch, "gmwd/") {
		return fmt.Errorf("refusing to work on branch %q: not a gmwd/* branch", branch)
	}
	return nil
}

// checkOnBranch guards every write: gmwd only ever touches its own branches.
func checkOnBranch(ctx context.Context, dir, branch string) error {
	if err := checkBranchName(branch); err != nil {
		return err
	}
	cur, err := git(ctx, dir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return err
	}
	if cur != branch {
		return fmt.Errorf("worktree %s is on %q, expected %q", dir, cur, branch)
	}
	return nil
}

// commitAll commits every change in the worktree. It returns the new commit,
// or "" if there was nothing to commit.
func commitAll(ctx context.Context, dir, branch, msg string) (string, error) {
	if err := checkOnBranch(ctx, dir, branch); err != nil {
		return "", err
	}
	if _, err := git(ctx, dir, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := git(ctx, dir, "diff", "--cached", "--quiet"); err == nil {
		return "", nil
	}
	if _, err := git(ctx, dir, "commit", "--no-verify", "-q", "-m", msg); err != nil {
		return "", err
	}
	return git(ctx, dir, "rev-parse", "HEAD")
}

// resetHard throws away everything since the last commit.
func resetHard(ctx context.Context, dir, branch string) error {
	if err := checkOnBranch(ctx, dir, branch); err != nil {
		return err
	}
	if _, err := git(ctx, dir, "reset", "-q", "--hard"); err != nil {
		return err
	}
	_, err := git(ctx, dir, "clean", "-q", "-fd")
	return err
}

var errNotRepo = errors.New("not a git repository")

func checkRepo(ctx context.Context, repo string) error {
	if _, err := git(ctx, repo, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("%s: %w", repo, errNotRepo)
	}
	return nil
}
