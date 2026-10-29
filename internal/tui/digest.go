package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func (m Model) digestKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "left", "h", "[":
		return m.stepDay(+1) // older: further down the newest-first list
	case "right", "l", "]":
		return m.stepDay(-1)
	}
	var cmd tea.Cmd
	m.digestView, cmd = m.digestView.Update(k)
	return m, cmd
}

// stepDay moves through the days that have a digest (newest first).
func (m Model) stepDay(delta int) (tea.Model, tea.Cmd) {
	days := m.digest.Days
	cur := m.digest.Date
	i := -1
	for j, d := range days {
		if d == cur {
			i = j
		}
	}
	var next string
	switch {
	case i < 0 && delta > 0 && len(days) > 0:
		// Today has no file yet: older means the newest one there is.
		next = days[0]
		if next == cur && len(days) > 1 {
			next = days[1]
		}
	case i >= 0 && i+delta >= 0 && i+delta < len(days):
		next = days[i+delta]
	default:
		return m, nil
	}
	m.digestDate = next
	return m, m.loadDigest(next)
}

func (m Model) digestScreen() string {
	if m.digest.Date == "" {
		return dimStyle.Render("loading…")
	}
	return m.digestView.View()
}

var (
	h1Style    = lipgloss.NewStyle().Bold(true).Foreground(accent)
	h2Style    = lipgloss.NewStyle().Bold(true)
	quoteStyle = lipgloss.NewStyle().Foreground(subtle).Italic(true)
)

// renderMarkdown styles the digest's markdown for the terminal: headings,
// quotes, and **bold** and `code` markers dropped. Lines are clipped.
func renderMarkdown(md string, width int) string {
	var out []string
	plain := strings.NewReplacer("**", "", "`", "")
	for _, l := range strings.Split(strings.TrimRight(md, "\n"), "\n") {
		l = plain.Replace(strings.TrimRight(l, " "))
		switch {
		case strings.HasPrefix(l, "# "):
			l = h1Style.Render(strings.TrimPrefix(l, "# "))
		case strings.HasPrefix(l, "## "):
			l = h2Style.Render(strings.TrimPrefix(l, "## "))
		case strings.HasPrefix(l, "### "):
			l = h2Style.Render(strings.TrimPrefix(l, "### "))
		case strings.HasPrefix(strings.TrimSpace(l), "> "):
			l = quoteStyle.Render("  │ " + strings.TrimPrefix(strings.TrimSpace(l), "> "))
		}
		out = append(out, truncate(l, width))
	}
	return strings.Join(out, "\n")
}
