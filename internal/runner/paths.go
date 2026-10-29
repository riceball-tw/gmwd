package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Paths locates a topic's files under gmwd's home, for readers such as the
// digest.
type Paths struct{ Home string }

func (p Paths) Topic(id string) string    { return filepath.Join(p.Home, "topics", id) }
func (p Paths) Notes(id string) string    { return filepath.Join(p.Topic(id), "notes.md") }
func (p Paths) Work(id string) string     { return filepath.Join(p.Topic(id), "work") }
func (p Paths) Report(id string) string   { return filepath.Join(p.Work(id), "report.md") }
func (p Paths) Worktree(id string) string { return filepath.Join(p.Home, "worktrees", id) }
func (p Paths) Cache(id string) string    { return filepath.Join(p.Home, "cache", id) }

// LastNote returns the newest section of a topic's notes.md (one iteration's
// summary), or "" if there are none.
func (p Paths) LastNote(id string) (string, error) {
	s, err := readTail(p.Notes(id), 16<<10)
	if err != nil || s == "" {
		return "", err
	}
	if i := strings.LastIndex(s, "\n## "); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s), nil
}

// BranchInfo describes a gmwd branch against the repo's default branch.
type BranchInfo struct {
	Branch    string `json:"branch"`
	Base      string `json:"base"`
	Exists    bool   `json:"exists"`
	Ahead     int    `json:"ahead"`               // commits on Branch not on Base
	ShortStat string `json:"shortStat,omitempty"` // "3 files changed, 40 insertions(+), 2 deletions(-)"
}

// BranchStat reports what a topic's branch adds over the default branch.
func BranchStat(ctx context.Context, repo, branch string) (BranchInfo, error) {
	info := BranchInfo{Branch: branch}
	if _, err := os.Stat(repo); err != nil {
		return info, err
	}
	if _, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		return info, nil // never ran, or deleted
	}
	info.Exists = true
	base, err := defaultBranch(ctx, repo)
	if err != nil {
		return info, err
	}
	info.Base = base
	n, err := git(ctx, repo, "rev-list", "--count", base+".."+branch)
	if err != nil {
		return info, err
	}
	if info.Ahead, err = strconv.Atoi(n); err != nil {
		return info, fmt.Errorf("rev-list count %q: %w", n, err)
	}
	info.ShortStat, err = git(ctx, repo, "diff", "--shortstat", base+"..."+branch)
	return info, err
}
