// Package runner runs topics through `claude -p`, one iteration at a time.
package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"gmwd/internal/usage"
)

// Claude runs the claude CLI.
type Claude struct {
	// Bin is the claude binary; "claude" when empty.
	Bin string
	// Env is the child's whole environment. Nil means MinimalEnv().
	Env []string
}

// envAllowlist is what a child process inherits. OAuth comes from the
// keychain, which needs HOME and USER.
var envAllowlist = []string{"HOME", "USER", "LOGNAME", "PATH", "SHELL", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "TERM"}

// MinimalEnv returns the allowlisted variables from the current environment.
func MinimalEnv() []string {
	var env []string
	for _, k := range envAllowlist {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// Invocation is one `claude -p` call.
type Invocation struct {
	Dir          string
	Prompt       string
	Model        string
	SessionID    string
	Tools        []string // available tools (--tools)
	AllowedTools []string // pre-approved rules (--allowedTools)
	AddDirs      []string
	Settings     string // JSON for --settings
	JSONSchema   string
	ExtraEnv     []string
}

// Args builds the command line. Every run is unattended: dontAsk denies
// anything that isn't pre-approved, and the user's hooks, plugins and MCP
// servers are not loaded.
func (inv Invocation) Args() []string {
	args := []string{"-p", inv.Prompt,
		"--output-format", "stream-json", "--verbose",
		"--permission-mode", "dontAsk",
		"--setting-sources", "project,local",
		"--strict-mcp-config",
	}
	if inv.Model != "" {
		args = append(args, "--model", inv.Model)
	}
	if inv.SessionID != "" {
		args = append(args, "--session-id", inv.SessionID)
	}
	args = append(args, "--tools", strings.Join(inv.Tools, ","))
	if len(inv.AllowedTools) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, inv.AllowedTools...)
	}
	for _, d := range inv.AddDirs {
		args = append(args, "--add-dir", d)
	}
	if inv.Settings != "" {
		args = append(args, "--settings", inv.Settings)
	}
	if inv.JSONSchema != "" {
		args = append(args, "--json-schema", inv.JSONSchema)
	}
	return args
}

// Result is what one call produced.
type Result struct {
	SessionID      string
	IsError        bool
	Subtype        string
	APIErrorStatus int
	Text           string
	Structured     json.RawMessage
	Usage          usage.TokenCounts
	CostUSD        float64
	NumTurns       int
	Denials        int
	// RateLimit is the last rate_limit_event seen, if any.
	RateLimit *usage.Snapshot
	// GotResult is false when the process ended without a result event.
	GotResult bool
}

// Rejected reports whether the call hit a rate limit.
func (r Result) Rejected() bool {
	return r.APIErrorStatus == 429 || (r.RateLimit != nil && r.RateLimit.Status == "rejected")
}

// Run starts claude, copies the raw stream to log, and calls onRateLimit
// for every rate_limit_event as it arrives. Cancelling ctx kills the child.
func (c Claude) Run(ctx context.Context, inv Invocation, log io.Writer, onRateLimit func(usage.Snapshot)) (Result, error) {
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	cmd := exec.CommandContext(ctx, bin, inv.Args()...)
	cmd.Dir = inv.Dir
	env := c.Env
	if env == nil {
		env = MinimalEnv()
	}
	cmd.Env = append(append([]string{}, env...), inv.ExtraEnv...)
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start claude: %w", err)
	}
	res, perr := parseStream(io.TeeReader(out, log), onRateLimit)
	werr := cmd.Wait()
	if perr != nil {
		return res, perr
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if werr != nil && !res.GotResult {
		return res, fmt.Errorf("claude: %w: %s", werr, strings.TrimSpace(stderr.String()))
	}
	return res, nil
}

type streamLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`

	// rate_limit_event
	RateLimitInfo *usage.RateLimitInfo `json:"rate_limit_info"`

	// result
	IsError           bool            `json:"is_error"`
	APIErrorStatus    *int            `json:"api_error_status"`
	Result            string          `json:"result"`
	StructuredOutput  json.RawMessage `json:"structured_output"`
	TotalCostUSD      float64         `json:"total_cost_usd"`
	NumTurns          int             `json:"num_turns"`
	PermissionDenials []any           `json:"permission_denials"`
	Usage             struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

func parseStream(r io.Reader, onRateLimit func(usage.Snapshot)) (Result, error) {
	var res Result
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var l streamLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if l.SessionID != "" {
			res.SessionID = l.SessionID
		}
		switch l.Type {
		case "rate_limit_event":
			if l.RateLimitInfo == nil {
				continue
			}
			s := l.RateLimitInfo.Snapshot(time.Now())
			res.RateLimit = &s
			if onRateLimit != nil {
				onRateLimit(s)
			}
		case "result":
			res.GotResult = true
			res.Subtype = l.Subtype
			res.IsError = l.IsError
			if l.APIErrorStatus != nil {
				res.APIErrorStatus = *l.APIErrorStatus
			}
			res.Text = l.Result
			if len(l.StructuredOutput) > 0 && string(l.StructuredOutput) != "null" {
				res.Structured = l.StructuredOutput
			}
			res.CostUSD = l.TotalCostUSD
			res.NumTurns = l.NumTurns
			res.Denials = len(l.PermissionDenials)
			res.Usage = usage.TokenCounts{
				Input:       l.Usage.InputTokens,
				Output:      l.Usage.OutputTokens,
				CacheCreate: l.Usage.CacheCreationInputTokens,
				CacheRead:   l.Usage.CacheReadInputTokens,
				Requests:    l.NumTurns,
			}
		}
	}
	// Drain so the child never blocks on a full pipe.
	_, _ = io.Copy(io.Discard, r)
	return res, sc.Err()
}
