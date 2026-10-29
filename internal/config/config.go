// Package config holds gmwd's data directory and the user's settings.
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// Home is gmwd's data dir: $GMWD_HOME, or ~/.gmwd.
func Home() (string, error) {
	if h := os.Getenv("GMWD_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gmwd"), nil
}

// Settings are the user's live knobs. The daemon stores them; the TUI and
// CLI change them over the socket. Percentages are 0..100.
type Settings struct {
	// Paused stops new iterations from starting.
	Paused bool `json:"paused"`
	// MaxBurnPct is the usage slider: the share of the weekly cap gmwd may
	// bring total usage up to.
	MaxBurnPct float64 `json:"maxBurnPct"`
	// ReservePct of every 5h window is left for the user.
	ReservePct float64 `json:"reservePct"`
	// IdleMode decides when the user counts as idle.
	IdleMode IdleMode `json:"idleMode"`
	// QuietMinutes is how long the user must have been idle. 0 turns idle
	// detection off: gmwd runs whenever the budget allows.
	QuietMinutes int `json:"quietMinutes"`
}

// IdleMode is when gmwd may run.
type IdleMode string

const (
	// IdleNotUsingClaude: no interactive Claude Code session was written to
	// for QuietMinutes.
	IdleNotUsingClaude IdleMode = "not-using-claude"
	// IdleAway: additionally, no keyboard or mouse input for QuietMinutes.
	IdleAway IdleMode = "away"
)

// DefaultSettings are used until the user changes something. Stored
// settings are decoded over these, so new fields get their defaults.
func DefaultSettings() Settings {
	return Settings{MaxBurnPct: 90, ReservePct: 15, IdleMode: IdleNotUsingClaude, QuietMinutes: 10}
}

// Validate reports settings that make no sense.
func (s Settings) Validate() error {
	if s.MaxBurnPct < 0 || s.MaxBurnPct > 100 {
		return fmt.Errorf("max burn %v%%: want 0..100", s.MaxBurnPct)
	}
	if s.ReservePct < 0 || s.ReservePct > 100 {
		return fmt.Errorf("reserve %v%%: want 0..100", s.ReservePct)
	}
	if s.IdleMode != IdleNotUsingClaude && s.IdleMode != IdleAway {
		return fmt.Errorf("idle mode %q: want %s or %s", s.IdleMode, IdleNotUsingClaude, IdleAway)
	}
	if s.QuietMinutes < 0 || s.QuietMinutes > 24*60 {
		return fmt.Errorf("quiet minutes %d: want 0..1440", s.QuietMinutes)
	}
	return nil
}
