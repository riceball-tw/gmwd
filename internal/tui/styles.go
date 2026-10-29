package tui

import "github.com/charmbracelet/lipgloss"

var (
	accent  = lipgloss.AdaptiveColor{Light: "#5A4FCF", Dark: "#9D8CFF"}
	subtle  = lipgloss.AdaptiveColor{Light: "#8A8A8A", Dark: "#6C6C6C"}
	warn    = lipgloss.AdaptiveColor{Light: "#B25E00", Dark: "#FFB454"}
	bad     = lipgloss.AdaptiveColor{Light: "#C0392B", Dark: "#FF6B6B"}
	good    = lipgloss.AdaptiveColor{Light: "#1E8E3E", Dark: "#5FD787"}
	reserve = lipgloss.AdaptiveColor{Light: "#D9C7A3", Dark: "#5C4B2E"}

	titleStyle     = lipgloss.NewStyle().Bold(true).Foreground(accent)
	tabStyle       = lipgloss.NewStyle().Padding(0, 1).Foreground(subtle)
	activeTabStyle = lipgloss.NewStyle().Padding(0, 1).Bold(true).Foreground(accent).Underline(true)
	labelStyle     = lipgloss.NewStyle().Width(12).Foreground(subtle)
	dimStyle       = lipgloss.NewStyle().Foreground(subtle)
	usedStyle      = lipgloss.NewStyle().Foreground(accent)
	reserveStyle   = lipgloss.NewStyle().Foreground(reserve)
	targetStyle    = lipgloss.NewStyle().Foreground(warn).Bold(true)
	errStyle       = lipgloss.NewStyle().Foreground(bad)
	okStyle        = lipgloss.NewStyle().Foreground(good)
	warnStyle      = lipgloss.NewStyle().Foreground(warn)
	helpStyle      = lipgloss.NewStyle().Foreground(subtle)
)
