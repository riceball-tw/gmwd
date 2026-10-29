package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"gmwd/internal/store"
)

type storeRun = store.Run

func runColumns(width int) []table.Column {
	fixed := 11 + 8 + 5 + 8 + 7 + 6*2
	return []table.Column{
		{Title: "Started", Width: 11},
		{Title: "Topic", Width: max(width-fixed-26, 12)},
		{Title: "Took", Width: 8},
		{Title: "Iter", Width: 5},
		{Title: "Tokens", Width: 8},
		{Title: "5h", Width: 7},
		{Title: "Stopped", Width: 24},
	}
}

func (m *Model) syncRunTable() {
	rows := make([]table.Row, len(m.runs))
	for i, r := range m.runs {
		took := "running"
		if !r.EndedAt.IsZero() {
			took = ago(r.EndedAt.Sub(r.StartedAt))
		}
		rows[i] = table.Row{
			r.StartedAt.Local().Format("Mon 15:04"), r.TopicID, took, fmt.Sprint(r.Iterations),
			tokens(r.Tokens.Input + r.Tokens.Output + r.Tokens.CacheCreate),
			fmt.Sprintf("%.0f%%", r.WindowSpent*100), runStop(r),
		}
	}
	m.runTable.SetRows(rows)
	m.resize()
}

// runStop explains how a run ended in a few words.
func runStop(r store.Run) string {
	switch r.StopReason {
	case "":
		if r.EndedAt.IsZero() {
			return "running"
		}
		return "?"
	case "done":
		return "done"
	case "iterations":
		return "iteration limit"
	case "tokens":
		return "token budget"
	case "window_budget":
		return "window budget"
	case "reserve":
		return "reserve reached"
	case "rate_limited":
		return "rate limited"
	case "failures":
		return "3 failures"
	case "user_active":
		return "you came back"
	case "paused":
		return "paused"
	case "cancelled":
		return "cancelled"
	case "abandoned":
		return "abandoned (crash)"
	}
	return r.StopReason
}

func (m Model) jobsKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "pgup", "pgdown", "ctrl+u", "ctrl+d", "home", "end":
		var cmd tea.Cmd
		switch k.String() {
		case "home":
			m.logView.GotoTop()
		case "end":
			m.logView.GotoBottom()
		default:
			m.logView, cmd = m.logView.Update(k)
		}
		return m, cmd
	}
	m.runTable.Focus()
	var cmd tea.Cmd
	m.runTable, cmd = m.runTable.Update(k)
	return m, cmd
}

func (m Model) jobsView() string {
	var b strings.Builder
	if j := m.status.Job; j != nil {
		b.WriteString(row("Running", fmt.Sprintf("%s · iteration %d · %s tokens · %s",
			j.TopicID, j.Iteration, tokens(j.Tokens.Input+j.Tokens.Output+j.Tokens.CacheCreate), ago(m.now().Sub(j.StartedAt)))))
	} else {
		b.WriteString(row("Running", dimStyle.Render("nothing — "+m.status.Reason)))
	}
	b.WriteString("\n\n")
	if len(m.runs) == 0 {
		b.WriteString(dimStyle.Render("No runs yet."))
	} else {
		b.WriteString(m.runTable.View())
	}
	b.WriteString("\n\n")
	b.WriteString(titleStyle.Render("Live log"))
	b.WriteString("\n")
	if len(m.logLines) == 0 {
		b.WriteString(dimStyle.Render("Iterations will show up here while a job runs."))
	} else {
		b.WriteString(m.logView.View())
	}
	return b.String()
}
