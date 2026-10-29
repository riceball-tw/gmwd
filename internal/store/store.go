// Package store keeps gmwd's state in SQLite (~/.gmwd/gmwd.db): topics,
// runs, usage samples and settings.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"gmwd/internal/config"
	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

// Store is safe for concurrent use, also across processes (WAL mode).
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and migrates it.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrations[i] moves the schema from version i to i+1. Append only.
var migrations = []string{
	`CREATE TABLE topics (
		id         TEXT PRIMARY KEY,
		title      TEXT NOT NULL,
		kind       TEXT NOT NULL,
		prompt     TEXT NOT NULL,
		repo       TEXT NOT NULL DEFAULT '',
		priority   INTEGER NOT NULL DEFAULT 3,
		model      TEXT NOT NULL DEFAULT '',
		max_tokens INTEGER NOT NULL DEFAULT 0,
		done_when  TEXT NOT NULL DEFAULT '',
		status     TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE TABLE runs (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		topic_id            TEXT NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
		run_id              TEXT NOT NULL,
		run_dir             TEXT NOT NULL DEFAULT '',
		started_at          INTEGER NOT NULL,
		ended_at            INTEGER NOT NULL DEFAULT 0,
		stop_reason         TEXT NOT NULL DEFAULT '',
		iterations          INTEGER NOT NULL DEFAULT 0,
		succeeded           INTEGER NOT NULL DEFAULT 0,
		failed              INTEGER NOT NULL DEFAULT 0,
		input_tokens        INTEGER NOT NULL DEFAULT 0,
		output_tokens       INTEGER NOT NULL DEFAULT 0,
		cache_create_tokens INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
		cost_usd            REAL NOT NULL DEFAULT 0,
		window_spent        REAL NOT NULL DEFAULT 0,
		commits             TEXT NOT NULL DEFAULT '[]'
	);
	CREATE INDEX runs_topic ON runs(topic_id, started_at);
	CREATE TABLE usage_samples (
		at                  INTEGER NOT NULL,
		source              TEXT NOT NULL,
		status              TEXT NOT NULL DEFAULT '',
		five_hour           REAL NOT NULL,
		five_hour_resets_at INTEGER NOT NULL DEFAULT 0,
		seven_day           REAL NOT NULL,
		seven_day_resets_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX usage_samples_at ON usage_samples(at);
	CREATE TABLE settings (
		id   INTEGER PRIMARY KEY CHECK (id = 1),
		json TEXT NOT NULL
	);`,
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var v int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema v%d is newer than this gmwd (v%d)", v, len(migrations))
	}
	for ; v < len(migrations); v++ {
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			return fmt.Errorf("v%d: %w", v+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		return err
	}
	return tx.Commit()
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

// Topics.

const topicCols = `id, title, kind, prompt, repo, priority, model, max_tokens, done_when, status, created_at`

func scanTopic(row interface{ Scan(...any) error }) (topic.Topic, error) {
	var t topic.Topic
	var created int64
	err := row.Scan(&t.ID, &t.Title, &t.Kind, &t.Prompt, &t.Repo, &t.Priority, &t.Model, &t.MaxTokens, &t.DoneWhen, &t.Status, &created)
	t.CreatedAt = fromMS(created)
	return t, err
}

// AddTopic stores a new topic. It fills CreatedAt and Status if unset.
func (s *Store) AddTopic(ctx context.Context, t topic.Topic) (topic.Topic, error) {
	if err := t.Validate(); err != nil {
		return t, err
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	if t.Status == "" {
		t.Status = topic.Queued
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO topics (`+topicCols+`, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Title, t.Kind, t.Prompt, t.Repo, t.Priority, t.Model, t.MaxTokens, t.DoneWhen, t.Status, ms(t.CreatedAt), ms(time.Now()))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint") {
		return t, fmt.Errorf("topic %q: %w", t.ID, ErrExists)
	}
	return t, err
}

func (s *Store) GetTopic(ctx context.Context, id string) (topic.Topic, error) {
	t, err := scanTopic(s.db.QueryRowContext(ctx, `SELECT `+topicCols+` FROM topics WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("topic %q: %w", id, ErrNotFound)
	}
	return t, err
}

// ListTopics returns all topics, highest priority first, then oldest first.
func (s *Store) ListTopics(ctx context.Context) ([]topic.Topic, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+topicCols+` FROM topics ORDER BY priority DESC, created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ts []topic.Topic
	for rows.Next() {
		t, err := scanTopic(rows)
		if err != nil {
			return nil, err
		}
		ts = append(ts, t)
	}
	return ts, rows.Err()
}

// UpdateTopic replaces every field of an existing topic.
func (s *Store) UpdateTopic(ctx context.Context, t topic.Topic) error {
	if err := t.Validate(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE topics SET title=?, kind=?, prompt=?, repo=?, priority=?, model=?, max_tokens=?, done_when=?, status=?, updated_at=? WHERE id=?`,
		t.Title, t.Kind, t.Prompt, t.Repo, t.Priority, t.Model, t.MaxTokens, t.DoneWhen, t.Status, ms(time.Now()), t.ID)
	return mustAffect(res, err, "topic", t.ID)
}

func (s *Store) SetTopicStatus(ctx context.Context, id string, st topic.Status) error {
	res, err := s.db.ExecContext(ctx, `UPDATE topics SET status=?, updated_at=? WHERE id=?`, st, ms(time.Now()), id)
	return mustAffect(res, err, "topic", id)
}

// ClaimTopic atomically moves a topic to running unless it already is. It
// keeps two processes from running the same topic.
func (s *Store) ClaimTopic(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE topics SET status=?, updated_at=? WHERE id=? AND status != ?`,
		topic.Running, ms(time.Now()), id, topic.Running)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.GetTopic(ctx, id); err != nil {
			return err
		}
		return fmt.Errorf("topic %q is already running", id)
	}
	return nil
}

func (s *Store) DeleteTopic(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM topics WHERE id=?`, id)
	return mustAffect(res, err, "topic", id)
}

func mustAffect(res sql.Result, err error, what, id string) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%s %q: %w", what, id, ErrNotFound)
	}
	return nil
}

// Runs.

// Run is one `gmwd run` / scheduled job on a topic.
type Run struct {
	ID          int64             `json:"id"`
	TopicID     string            `json:"topicId"`
	RunID       string            `json:"runId"`
	RunDir      string            `json:"runDir"`
	StartedAt   time.Time         `json:"startedAt"`
	EndedAt     time.Time         `json:"endedAt,omitzero"`
	StopReason  string            `json:"stopReason,omitempty"`
	Iterations  int               `json:"iterations"`
	Succeeded   int               `json:"succeeded"`
	Failed      int               `json:"failed"`
	Tokens      usage.TokenCounts `json:"tokens"`
	CostUSD     float64           `json:"costUSD"`
	WindowSpent float64           `json:"windowSpent"`
	Commits     []string          `json:"commits,omitempty"`
}

// StartRun records a run that has begun and returns its row id.
func (s *Store) StartRun(ctx context.Context, r Run) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO runs (topic_id, run_id, run_dir, started_at) VALUES (?,?,?,?)`,
		r.TopicID, r.RunID, r.RunDir, ms(r.StartedAt))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishRun stores a run's outcome.
func (s *Store) FinishRun(ctx context.Context, r Run) error {
	commits, _ := json.Marshal(r.Commits)
	if r.Commits == nil {
		commits = []byte("[]")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET run_id=?, run_dir=?, ended_at=?, stop_reason=?, iterations=?, succeeded=?, failed=?,
		input_tokens=?, output_tokens=?, cache_create_tokens=?, cache_read_tokens=?, cost_usd=?, window_spent=?, commits=? WHERE id=?`,
		r.RunID, r.RunDir, ms(r.EndedAt), r.StopReason, r.Iterations, r.Succeeded, r.Failed,
		r.Tokens.Input, r.Tokens.Output, r.Tokens.CacheCreate, r.Tokens.CacheRead, r.CostUSD, r.WindowSpent, string(commits), r.ID)
	return mustAffect(res, err, "run", fmt.Sprint(r.ID))
}

// ListRuns returns the newest runs, optionally for one topic.
func (s *Store) ListRuns(ctx context.Context, topicID string, limit int) ([]Run, error) {
	q := `SELECT id, topic_id, run_id, run_dir, started_at, ended_at, stop_reason, iterations, succeeded, failed,
		input_tokens, output_tokens, cache_create_tokens, cache_read_tokens, cost_usd, window_spent, commits FROM runs`
	var args []any
	if topicID != "" {
		q += ` WHERE topic_id = ?`
		args = append(args, topicID)
	}
	q += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		var r Run
		var started, ended int64
		var commits string
		if err := rows.Scan(&r.ID, &r.TopicID, &r.RunID, &r.RunDir, &started, &ended, &r.StopReason, &r.Iterations, &r.Succeeded, &r.Failed,
			&r.Tokens.Input, &r.Tokens.Output, &r.Tokens.CacheCreate, &r.Tokens.CacheRead, &r.CostUSD, &r.WindowSpent, &commits); err != nil {
			return nil, err
		}
		r.StartedAt, r.EndedAt = fromMS(started), fromMS(ended)
		if err := json.Unmarshal([]byte(commits), &r.Commits); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// Usage samples.

// AddUsage records an observation of both windows.
func (s *Store) AddUsage(ctx context.Context, snap usage.Snapshot) error {
	at := snap.FiveHour.ObservedAt
	if at.IsZero() {
		at = time.Now()
	}
	src := snap.FiveHour.Source
	_, err := s.db.ExecContext(ctx, `INSERT INTO usage_samples (at, source, status, five_hour, five_hour_resets_at, seven_day, seven_day_resets_at) VALUES (?,?,?,?,?,?,?)`,
		ms(at), src, snap.Status, snap.FiveHour.Utilization, ms(snap.FiveHour.ResetsAt), snap.SevenDay.Utilization, ms(snap.SevenDay.ResetsAt))
	return err
}

// LatestUsage returns the newest sample, or ErrNotFound.
func (s *Store) LatestUsage(ctx context.Context) (usage.Snapshot, error) {
	var snap usage.Snapshot
	var at, r5, r7 int64
	var src string
	err := s.db.QueryRowContext(ctx, `SELECT at, source, status, five_hour, five_hour_resets_at, seven_day, seven_day_resets_at
		FROM usage_samples ORDER BY at DESC LIMIT 1`).Scan(&at, &src, &snap.Status, &snap.FiveHour.Utilization, &r5, &snap.SevenDay.Utilization, &r7)
	if errors.Is(err, sql.ErrNoRows) {
		return snap, fmt.Errorf("usage: %w", ErrNotFound)
	}
	if err != nil {
		return snap, err
	}
	snap.FiveHour.Kind, snap.SevenDay.Kind = usage.FiveHour, usage.SevenDay
	snap.FiveHour.Source, snap.SevenDay.Source = usage.Source(src), usage.Source(src)
	snap.FiveHour.ObservedAt, snap.SevenDay.ObservedAt = fromMS(at), fromMS(at)
	snap.FiveHour.ResetsAt, snap.SevenDay.ResetsAt = fromMS(r5), fromMS(r7)
	return snap, nil
}

// Settings.

// Settings returns the stored settings, or the defaults.
func (s *Store) Settings(ctx context.Context) (config.Settings, error) {
	st := config.DefaultSettings()
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT json FROM settings WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal([]byte(raw), &st)
}

func (s *Store) SaveSettings(ctx context.Context, st config.Settings) error {
	if err := st.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO settings (id, json) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET json = excluded.json`, string(raw))
	return err
}
