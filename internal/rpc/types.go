// Package rpc is the protocol between the gmwd daemon and its clients (CLI,
// TUI): JSON-RPC 2.0, one message per line, over a Unix socket. Clients that
// call "subscribe" also receive "event" notifications.
package rpc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gmwd/internal/config"
	"gmwd/internal/store"
	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

// Methods.
const (
	MethodStatus         = "status"          // → StatusResult
	MethodTopicAdd       = "topic.add"       // TopicParams → topic.Topic
	MethodTopicList      = "topic.list"      // → TopicsResult
	MethodTopicGet       = "topic.get"       // IDParams → topic.Topic
	MethodTopicUpdate    = "topic.update"    // TopicParams → topic.Topic (replaces all fields)
	MethodTopicDelete    = "topic.delete"    // IDParams → nil
	MethodSettingsUpdate = "settings.update" // SettingsPatch → config.Settings
	MethodRunsList       = "runs.list"       // RunsParams → RunsResult
	MethodUsageRefresh   = "usage.refresh"   // → usage.Snapshot (runs claude /usage now)
	MethodSubscribe      = "subscribe"       // → nil; events follow as notifications

	// NotifyEvent is the method name of server→client notifications.
	NotifyEvent = "event"
)

// StatusResult is the daemon's state at a glance.
type StatusResult struct {
	PID       int             `json:"pid"`
	StartedAt time.Time       `json:"startedAt"`
	Settings  config.Settings `json:"settings"`
	// Usage is the latest observation; UsageError the last probe failure.
	Usage      *usage.Snapshot      `json:"usage,omitempty"`
	UsageError string               `json:"usageError,omitempty"`
	Topics     map[topic.Status]int `json:"topics"`
	// Job is the running job, if any.
	Job *JobInfo `json:"job,omitempty"`
	// Reason says why gmwd is or isn't working right now.
	Reason string `json:"reason,omitempty"`
}

// JobInfo describes a running job.
type JobInfo struct {
	TopicID   string            `json:"topicId"`
	RunID     string            `json:"runId"`
	StartedAt time.Time         `json:"startedAt"`
	Iteration int               `json:"iteration"`
	Tokens    usage.TokenCounts `json:"tokens"`
}

type TopicParams struct {
	Topic topic.Topic `json:"topic"`
}

type IDParams struct {
	ID string `json:"id"`
}

type TopicsResult struct {
	Topics []topic.Topic `json:"topics"`
}

// SettingsPatch changes only the fields that are set.
type SettingsPatch struct {
	Paused     *bool    `json:"paused,omitempty"`
	MaxBurnPct *float64 `json:"maxBurnPct,omitempty"`
	ReservePct *float64 `json:"reservePct,omitempty"`
}

// Apply returns s with the patch applied.
func (p SettingsPatch) Apply(s config.Settings) config.Settings {
	if p.Paused != nil {
		s.Paused = *p.Paused
	}
	if p.MaxBurnPct != nil {
		s.MaxBurnPct = *p.MaxBurnPct
	}
	if p.ReservePct != nil {
		s.ReservePct = *p.ReservePct
	}
	return s
}

type RunsParams struct {
	TopicID string `json:"topicId,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

type RunsResult struct {
	Runs []store.Run `json:"runs"`
}

// EventType names what an Event carries.
type EventType string

const (
	EventUsage        EventType = "usage"         // Usage
	EventSettings     EventType = "settings"      // Settings
	EventTopic        EventType = "topic"         // Topic (added or changed)
	EventTopicDeleted EventType = "topic_deleted" // TopicID
	EventJob          EventType = "job"           // Job (nil when a job ends)
	EventLog          EventType = "log"           // Log, TopicID
)

// Event is pushed to subscribed clients.
type Event struct {
	Type     EventType        `json:"type"`
	At       time.Time        `json:"at"`
	Usage    *usage.Snapshot  `json:"usage,omitempty"`
	Settings *config.Settings `json:"settings,omitempty"`
	Topic    *topic.Topic     `json:"topic,omitempty"`
	TopicID  string           `json:"topicId,omitempty"`
	Job      *JobInfo         `json:"job,omitempty"`
	Log      string           `json:"log,omitempty"`
}

// Error codes: the JSON-RPC 2.0 ones, plus application codes.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeApp            = -32000
	CodeNotFound       = -32004
	CodeExists         = -32009
)

// Error is a JSON-RPC error. errors.Is matches store.ErrNotFound and
// store.ErrExists by code, so callers on either side of the socket can
// test for them the same way.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Is(target error) bool {
	switch target {
	case store.ErrNotFound:
		return e.Code == CodeNotFound
	case store.ErrExists:
		return e.Code == CodeExists
	}
	return false
}

func toError(err error) *Error {
	var e *Error
	switch {
	case errors.As(err, &e):
		return e
	case errors.Is(err, store.ErrNotFound):
		return &Error{Code: CodeNotFound, Message: err.Error()}
	case errors.Is(err, store.ErrExists):
		return &Error{Code: CodeExists, Message: err.Error()}
	}
	return &Error{Code: CodeApp, Message: err.Error()}
}

// maxSocketPath is the sun_path limit on macOS (104 with the NUL).
const maxSocketPath = 103

// SocketPath is $GMWD_SOCKET, or gmwd.sock in the data dir.
func SocketPath(home string) (string, error) {
	p := os.Getenv("GMWD_SOCKET")
	if p == "" {
		p = filepath.Join(home, "gmwd.sock")
	}
	if len(p) > maxSocketPath {
		return "", fmt.Errorf("socket path %s is %d bytes, over the %d-byte limit; set GMWD_SOCKET to a shorter path", p, len(p), maxSocketPath)
	}
	return p, nil
}
