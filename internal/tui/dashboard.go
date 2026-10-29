package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"gmwd/internal/rpc"
	"gmwd/internal/usage"
)

// sliderStep is how much ←/→ move the max-burn slider.
const sliderStep = 5

func (m Model) dashboardKey(key string) (tea.Model, tea.Cmd) {
	var delta float64
	switch key {
	case "left", "h", "-":
		delta = -sliderStep
	case "right", "l", "+", "=":
		delta = sliderStep
	default:
		return m, nil
	}
	cur := m.status.Settings.MaxBurnPct
	// Step, then land on the 5% grid (a value set with gmwd config may be off it).
	next := math.Round((cur+delta)/sliderStep) * sliderStep
	next = math.Max(0, math.Min(100, next))
	if next == cur {
		return m, nil
	}
	// Show it right away; the daemon's answer confirms or corrects it.
	m.status.Settings.MaxBurnPct = next
	return m, m.updateSettings(rpc.SettingsPatch{MaxBurnPct: &next})
}

func (m Model) dashboardView() string {
	if !m.hasStatus {
		return dimStyle.Render("connecting to the daemon…")
	}
	st := m.status
	now := m.now()
	// label (12) + bar + "  40%  " (7) + "resets Mon 15:04 (in 2d19h)" (~28)
	barW := max(min(m.width-12-7-28, 60), 10)
	var b strings.Builder

	if u := st.Usage; u != nil {
		reservePct := st.Settings.ReservePct / 100
		b.WriteString(row("5h window", windowBar(u.FiveHour.Utilization, reservePct, barW)+"  "+
			fmt.Sprintf("%3.0f%%", u.FiveHour.Utilization*100)+"  "+resets(u.FiveHour, now)))
		b.WriteString("\n")
		b.WriteString(row("", dimStyle.Render(fmt.Sprintf("▒ %.0f%% at the end is kept for you", st.Settings.ReservePct))))
		b.WriteString("\n\n")
		target := st.Settings.MaxBurnPct / 100
		b.WriteString(row("Week", weekBar(u.SevenDay.Utilization, target, barW)+"  "+
			fmt.Sprintf("%3.0f%%", u.SevenDay.Utilization*100)+"  "+resets(u.SevenDay, now)))
		b.WriteString("\n")
		b.WriteString(row("", dimStyle.Render("┃ = max burn target")))
		b.WriteString("\n")
		b.WriteString(row("", dimStyle.Render(fmt.Sprintf("usage read %s ago %s", ago(now.Sub(u.FiveHour.ObservedAt)), sourceName(u.FiveHour.Source)))))
	} else {
		msg := "no usage reading yet"
		if st.UsageError != "" {
			msg = "can't read usage: " + st.UsageError
		}
		b.WriteString(row("Usage", warnStyle.Render(msg)))
	}
	b.WriteString("\n\n")

	b.WriteString(row("Max burn", m.slider(barW)))
	b.WriteString("\n")
	b.WriteString(row("", dimStyle.Render(m.sliderNote())))
	b.WriteString("\n\n")

	reason := st.Reason
	if st.Settings.Paused {
		reason = warnStyle.Render("paused") + dimStyle.Render(" — press space to resume")
	}
	b.WriteString(row("Status", reason))
	b.WriteString("\n")
	if j := st.Job; j != nil {
		b.WriteString(row("Job", fmt.Sprintf("%s · iteration %d · %s tokens · running %s",
			j.TopicID, j.Iteration, tokens(j.Tokens.Input+j.Tokens.Output+j.Tokens.CacheCreate), ago(now.Sub(j.StartedAt)))))
	} else {
		b.WriteString(row("Job", dimStyle.Render("none")))
	}
	b.WriteString("\n")
	b.WriteString(row("Topics", topicCounts(st)))
	if c := st.Calibration; c != nil {
		basis := "default until gmwd has seen more usage"
		if c.Ratio.FromData {
			basis = fmt.Sprintf("measured over %.0f points", c.Ratio.WindowPoints*100)
		}
		b.WriteString("\n")
		b.WriteString(row("Calibration", dimStyle.Render(fmt.Sprintf("1 point of 5h ≈ %.2f of the week (%s)", c.Ratio.Ratio, basis))))
		if c.LimitHits > 0 {
			b.WriteString(dimStyle.Render(fmt.Sprintf(" · %d rate-limit hits this week", c.LimitHits)))
		}
	}
	return b.String()
}

func (m Model) slider(width int) string {
	pct := m.status.Settings.MaxBurnPct
	cells := 20
	n := int(math.Round(pct / 100 * float64(cells)))
	bar := usedStyle.Render(strings.Repeat("■", n)) + dimStyle.Render(strings.Repeat("□", cells-n))
	return fmt.Sprintf("◀ %s ▶ %3.0f%%", bar, pct)
}

func (m Model) sliderNote() string {
	pct := m.status.Settings.MaxBurnPct
	u := m.status.Usage
	if u == nil {
		return fmt.Sprintf("gmwd may fill your week up to %.0f%%", pct)
	}
	left := pct - u.SevenDay.Utilization*100
	if left <= 0 {
		return fmt.Sprintf("week already at %.0f%%: gmwd waits for the weekly reset", u.SevenDay.Utilization*100)
	}
	note := fmt.Sprintf("gmwd may fill your week up to %.0f%% · %.0f points left to use", pct, left)
	// With a measured token rate, say roughly what that buys.
	if c := m.status.Calibration; c != nil && c.Ratio.Ratio > 0 {
		if r, ok := c.Rate("sonnet"); ok {
			note += fmt.Sprintf(" (≈ %s sonnet tokens)", tokens(int64(left/c.Ratio.Ratio*r.TokensPerPoint)))
		}
	}
	return note
}

// windowBar shows 5h usage with the user's reserve shaded at the end.
func windowBar(used, reservePct float64, width int) string {
	usedN := cellsFor(used, width)
	reserveStart := cellsFor(1-reservePct, width)
	var b strings.Builder
	for i := range width {
		switch {
		case i < usedN:
			b.WriteString(usedStyle.Render("█"))
		case i >= reserveStart:
			b.WriteString(reserveStyle.Render("▒")) // distinct without color too
		default:
			b.WriteString(dimStyle.Render("░"))
		}
	}
	return b.String()
}

// weekBar shows weekly usage with the max-burn target marked.
func weekBar(used, target float64, width int) string {
	usedN := cellsFor(used, width)
	mark := min(cellsFor(target, width), width-1)
	var b strings.Builder
	for i := range width {
		switch {
		case i == mark:
			b.WriteString(targetStyle.Render("┃"))
		case i < usedN:
			b.WriteString(usedStyle.Render("█"))
		default:
			b.WriteString(dimStyle.Render("░"))
		}
	}
	return b.String()
}

func cellsFor(frac float64, width int) int {
	return max(0, min(width, int(math.Round(frac*float64(width)))))
}

func sourceName(s usage.Source) string {
	switch s {
	case usage.SourceUsageCommand:
		return "via /usage"
	case usage.SourceRateLimitEvent:
		return "from a running job"
	}
	return ""
}

func resets(w usage.Window, now time.Time) string {
	if w.ResetsAt.IsZero() {
		return dimStyle.Render("reset unknown")
	}
	if !w.ResetsAt.After(now) {
		return dimStyle.Render("reset passed")
	}
	return dimStyle.Render(fmt.Sprintf("resets %s (in %s)", w.ResetsAt.Local().Format("Mon 15:04"), ago(w.ResetsAt.Sub(now))))
}

func topicCounts(st rpc.StatusResult) string {
	var parts []string
	for _, s := range []string{"running", "queued", "paused", "done", "failed"} {
		for status, n := range st.Topics {
			if string(status) == s && n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, s))
			}
		}
	}
	if len(parts) == 0 {
		return dimStyle.Render("none yet — add one on the Topics screen")
	}
	return strings.Join(parts, " · ")
}

func row(label, value string) string {
	return labelStyle.Render(label) + value
}

func ago(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Minute)
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	h, mi := d/time.Hour, (d%time.Hour)/time.Minute
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, mi)
	}
	return fmt.Sprintf("%dm", mi)
}

func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func truncate(s string, width int) string {
	return ansi.Truncate(s, width, "…")
}
