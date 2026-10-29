package main

import (
	"errors"
	"io"

	tea "github.com/charmbracelet/bubbletea"

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
	_, err = tea.NewProgram(tui.New(c), tea.WithAltScreen()).Run()
	return err
}
