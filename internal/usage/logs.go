package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TokenCounts sums the usage fields of assistant messages.
type TokenCounts struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheCreate int64 `json:"cacheCreate"`
	CacheRead   int64 `json:"cacheRead"`
	Requests    int   `json:"requests"`
}

func (c *TokenCounts) add(u logUsage) {
	c.Input += u.InputTokens
	c.Output += u.OutputTokens
	c.CacheCreate += u.CacheCreationInputTokens
	c.CacheRead += u.CacheReadInputTokens
	c.Requests++
}

// Total is every token kind added up, unweighted.
func (c TokenCounts) Total() int64 {
	return c.Input + c.Output + c.CacheCreate + c.CacheRead
}

// RateLimitHit is a 429 that Claude Code logged.
type RateLimitHit struct {
	At   time.Time     `json:"at"`
	Info RateLimitInfo `json:"info"`
}

// Tally is what the session logs say about usage since some time.
type Tally struct {
	Since   time.Time              `json:"since"`
	ByModel map[string]TokenCounts `json:"byModel"`
	// Interactive is usage from `claude` sessions (entrypoint "cli");
	// Headless from `claude -p` (entrypoint "sdk-cli"), which includes gmwd.
	Interactive TokenCounts `json:"interactive"`
	Headless    TokenCounts `json:"headless"`
	// LastInteractive is the newest user or assistant record from an
	// interactive session.
	LastInteractive time.Time      `json:"lastInteractive"`
	RateLimitHits   []RateLimitHit `json:"rateLimitHits,omitempty"`
}

// Total is all usage in the tally.
func (t Tally) Total() TokenCounts {
	var c TokenCounts
	for _, m := range t.ByModel {
		c.Input += m.Input
		c.Output += m.Output
		c.CacheCreate += m.CacheCreate
		c.CacheRead += m.CacheRead
		c.Requests += m.Requests
	}
	return c
}

type logUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// logRecord holds only the metadata we read. Message content is never decoded.
type logRecord struct {
	Type       string    `json:"type"`
	Timestamp  time.Time `json:"timestamp"`
	Entrypoint string    `json:"entrypoint"`
	RequestID  string    `json:"requestId"`
	Error      string    `json:"error"`
	Message    struct {
		ID    string   `json:"id"`
		Model string   `json:"model"`
		Usage logUsage `json:"usage"`
	} `json:"message"`
	QuotaLimits *RateLimitInfo `json:"quotaLimits"`
}

// DefaultLogRoot is ~/.claude/projects.
func DefaultLogRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// ScanLogs reads every session log under root (subagent logs included) and
// tallies records at or after since.
func ScanLogs(root string, since time.Time) (Tally, error) {
	t := Tally{Since: since, ByModel: map[string]TokenCounts{}}
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(since) {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil // rotated or removed under us
		}
		defer f.Close()
		return scanFile(f, since, seen, &t)
	})
	return t, err
}

var (
	assistantTag = []byte(`"type":"assistant"`)
	userTag      = []byte(`"type":"user"`)
)

func scanFile(r io.Reader, since time.Time, seen map[string]bool, t *Tally) error {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && (bytes.Contains(line, assistantTag) || bytes.Contains(line, userTag)) {
			addRecord(line, since, seen, t)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func addRecord(line []byte, since time.Time, seen map[string]bool, t *Tally) {
	var rec logRecord
	if json.Unmarshal(line, &rec) != nil || rec.Timestamp.Before(since) {
		return
	}
	if rec.Type != "assistant" && rec.Type != "user" {
		return
	}
	if rec.Entrypoint == "cli" && rec.Timestamp.After(t.LastInteractive) {
		t.LastInteractive = rec.Timestamp
	}
	if rec.Type != "assistant" {
		return
	}
	if rec.QuotaLimits != nil && rec.Error == "rate_limit" {
		t.RateLimitHits = append(t.RateLimitHits, RateLimitHit{At: rec.Timestamp, Info: *rec.QuotaLimits})
		return
	}
	if rec.Message.Model == "" || rec.Message.Model == "<synthetic>" {
		return
	}
	// Claude Code writes one record per content block, each repeating the
	// message's usage.
	key := rec.Message.ID + "\x00" + rec.RequestID
	if seen[key] {
		return
	}
	seen[key] = true

	m := t.ByModel[rec.Message.Model]
	m.add(rec.Message.Usage)
	t.ByModel[rec.Message.Model] = m
	if rec.Entrypoint == "cli" {
		t.Interactive.add(rec.Message.Usage)
	} else {
		t.Headless.add(rec.Message.Usage)
	}
}
