// Package topic defines the backlog items gmwd works on.
package topic

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Kind is what a topic produces.
type Kind string

const (
	// Research produces a report.md with sources.
	Research Kind = "research"
	// Solve produces commits on a gmwd/* branch in a git worktree.
	Solve Kind = "solve"
)

// Status is where a topic is in its lifecycle.
type Status string

const (
	Queued  Status = "queued"
	Running Status = "running"
	Paused  Status = "paused"
	Done    Status = "done"
	Failed  Status = "failed"
)

// DefaultModel is used when a topic doesn't name one.
const DefaultModel = "sonnet"

// Topic is one backlog item.
type Topic struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Kind   Kind   `json:"kind"`
	Prompt string `json:"prompt"`
	// Repo is the git repo for Solve, or an optional read-only context
	// directory for Research.
	Repo      string    `json:"repo,omitempty"`
	Priority  int       `json:"priority"`
	Model     string    `json:"model,omitempty"`
	MaxTokens int64     `json:"maxTokens,omitempty"`
	DoneWhen  string    `json:"doneWhen,omitempty"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

// ModelOrDefault returns the model to run the topic with.
func (t Topic) ModelOrDefault() string {
	if t.Model == "" {
		return DefaultModel
	}
	return t.Model
}

// Branch is the git branch a Solve topic works on.
func (t Topic) Branch() string {
	return "gmwd/" + t.ID
}

// Validate reports whether the topic can be run.
func (t Topic) Validate() error {
	switch {
	case !idRe.MatchString(t.ID):
		return fmt.Errorf("topic id %q: want lowercase letters, digits and dashes", t.ID)
	case strings.TrimSpace(t.Prompt) == "":
		return fmt.Errorf("topic %s: empty prompt", t.ID)
	case t.Kind != Research && t.Kind != Solve:
		return fmt.Errorf("topic %s: kind %q, want research or solve", t.ID, t.Kind)
	case t.Kind == Solve && t.Repo == "":
		return fmt.Errorf("topic %s: solve topics need a repo", t.ID)
	}
	return nil
}

var (
	idRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	nonSlug = regexp.MustCompile(`[^a-z0-9]+`)
)

// Slug turns a title into an id fragment: "Fix flaky auth tests!" → "fix-flaky-auth-tests".
func Slug(title string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		s = "topic"
	}
	return s
}
