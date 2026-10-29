package main

import (
	"errors"
	"io"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"gmwd/internal/tui"
)

// cmdTUI opens the terminal UI. Quitting it leaves the daemon running.
func cmdTUI(args []string, out io.Writer) error {
	if len(args) > 0 {
		return errors.New("usage: gmwd (no arguments) opens the TUI")
	}
	c, err := dialDaemon()
	if err != nil {
		return err
	}
	defer c.Close()
	// Ask the terminal for its background colour now: the query reads stdin,
	// and once Bubble Tea owns the input it would swallow keystrokes.
	lipgloss.HasDarkBackground()
	_, err = tea.NewProgram(tui.New(c), tea.WithAltScreen()).Run()
	return err
}
