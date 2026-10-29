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
}

// DefaultSettings are used until the user changes something.
func DefaultSettings() Settings {
	return Settings{MaxBurnPct: 90, ReservePct: 15}
}

// Validate reports settings that make no sense.
func (s Settings) Validate() error {
	if s.MaxBurnPct < 0 || s.MaxBurnPct > 100 {
		return fmt.Errorf("max burn %v%%: want 0..100", s.MaxBurnPct)
	}
	if s.ReservePct < 0 || s.ReservePct > 100 {
		return fmt.Errorf("reserve %v%%: want 0..100", s.ReservePct)
	}
	return nil
}
