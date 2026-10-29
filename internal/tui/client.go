// Package tui is gmwd's terminal UI. It is a thin client: everything it
// shows comes from the daemon over the socket, and every change it makes is
// a call to the daemon.
package tui

import (
	"context"

	"gmwd/internal/rpc"
)

// Client is the part of rpc.Client the TUI uses; tests substitute a fake.
type Client interface {
	Call(ctx context.Context, method string, params, result any) error
	Subscribe(ctx context.Context) (<-chan rpc.Event, error)
}
