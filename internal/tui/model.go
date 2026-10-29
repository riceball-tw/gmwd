package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"gmwd/internal/config"
	"gmwd/internal/rpc"
	"gmwd/internal/store"
	"gmwd/internal/topic"
)

type tab int

const (
	tabDashboard tab = iota
	tabTopics
	tabJobs
	tabDigest
	tabCount
)

var tabNames = [...]string{"Dashboard", "Topics", "Jobs", "Digest"}

// refreshEvery re-reads status so countdowns and the pacer's reason stay
// current; everything else arrives as events.
const refreshEvery = 3 * time.Second

// maxLogLines bounds the live job log.
const maxLogLines = 500

// Messages.
type (
	statusMsg       rpc.StatusResult
	topicsMsg       []topic.Topic
	runsMsg         []store.Run
	digestMsg       rpc.DigestResult
	settingsMsg     config.Settings
	eventMsg        rpc.Event
	errMsg          struct{ err error }
	subscribedMsg   struct{ ch <-chan rpc.Event }
	disconnectedMsg struct{}
	tickMsg         time.Time
	// doneMsg reports that an action finished, with a line for the footer.
	doneMsg string
)

// Model is the root Bubble Tea model.
type Model struct {
	client Client
	now    func() time.Time

	width, height int
	tab           tab
	showHelp      bool

	status    rpc.StatusResult
	hasStatus bool
	topics    []topic.Topic
	runs      []store.Run
	logLines  []string

	events       <-chan rpc.Event
	disconnected bool
	err          error
	flash        string // last action's result

	topicTable table.Model
	runTable   table.Model
	logView    viewport.Model
	digestView viewport.Model

	// Digest tab state.
	digest     rpc.DigestResult
	digestDate string // "" = today

	// Topics tab state.
	details       bool
	confirmDelete string // topic id awaiting y/n
	form          *huh.Form
	formValues    *topicForm
}

// New returns a model talking to the daemon through c.
func New(c Client) Model {
	m := Model{client: c, now: time.Now, width: 100, height: 30}
	m.topicTable = table.New(table.WithColumns(topicColumns(m.width)), table.WithFocused(true))
	m.runTable = table.New(table.WithColumns(runColumns(m.width)))
	m.logView = viewport.New(m.width, 10)
	m.digestView = viewport.New(m.width, 10)
	m.resize()
	return m
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.subscribe(), m.loadStatus(), m.loadTopics(), m.loadRuns(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(refreshEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Commands that talk to the daemon.

func (m Model) call(method string, params, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return m.client.Call(ctx, method, params, result)
}

func (m Model) subscribe() tea.Cmd {
	return func() tea.Msg {
		ch, err := m.client.Subscribe(context.Background())
		if err != nil {
			return errMsg{err}
		}
		return subscribedMsg{ch}
	}
}

// listen waits for the next event from the daemon.
func listen(ch <-chan rpc.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return disconnectedMsg{}
		}
		return eventMsg(ev)
	}
}

func (m Model) loadStatus() tea.Cmd {
	return func() tea.Msg {
		var st rpc.StatusResult
		if err := m.call(rpc.MethodStatus, nil, &st); err != nil {
			return errMsg{err}
		}
		return statusMsg(st)
	}
}

func (m Model) loadTopics() tea.Cmd {
	return func() tea.Msg {
		var res rpc.TopicsResult
		if err := m.call(rpc.MethodTopicList, nil, &res); err != nil {
			return errMsg{err}
		}
		return topicsMsg(res.Topics)
	}
}

func (m Model) loadRuns() tea.Cmd {
	return func() tea.Msg {
		var res rpc.RunsResult
		if err := m.call(rpc.MethodRunsList, rpc.RunsParams{Limit: 50}, &res); err != nil {
			return errMsg{err}
		}
		return runsMsg(res.Runs)
	}
}

func (m Model) loadDigest(date string) tea.Cmd {
	return func() tea.Msg {
		var res rpc.DigestResult
		if err := m.call(rpc.MethodDigest, rpc.DigestParams{Date: date}, &res); err != nil {
			return errMsg{err}
		}
		return digestMsg(res)
	}
}

func (m Model) updateSettings(p rpc.SettingsPatch) tea.Cmd {
	return func() tea.Msg {
		var s config.Settings
		if err := m.call(rpc.MethodSettingsUpdate, p, &s); err != nil {
			return errMsg{err}
		}
		return settingsMsg(s)
	}
}

// action runs a call whose result arrives as an event; it reports done.
func (m Model) action(done, method string, params any) tea.Cmd {
	return func() tea.Msg {
		if err := m.call(method, params, nil); err != nil {
			return errMsg{err}
		}
		return doneMsg(done)
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		return m, nil
	case subscribedMsg:
		m.events = msg.ch
		return m, listen(msg.ch)
	case eventMsg:
		cmd := m.applyEvent(rpc.Event(msg))
		return m, tea.Batch(cmd, listen(m.events))
	case disconnectedMsg:
		m.disconnected = true
		return m, nil
	case statusMsg:
		m.status, m.hasStatus = rpc.StatusResult(msg), true
		return m, nil
	case topicsMsg:
		m.topics = msg
		m.syncTopicTable()
		return m, nil
	case runsMsg:
		m.runs = msg
		m.syncRunTable()
		return m, nil
	case settingsMsg:
		m.status.Settings = config.Settings(msg)
		return m, nil
	case digestMsg:
		m.digest = rpc.DigestResult(msg)
		m.digestView.SetContent(renderMarkdown(m.digest.Markdown, m.width))
		m.digestView.GotoTop()
		return m, nil
	case errMsg:
		m.err = msg.err
		return m, nil
	case doneMsg:
		m.flash, m.err = string(msg), nil
		return m, nil
	case tickMsg:
		if m.disconnected {
			return m, nil
		}
		return m, tea.Batch(m.loadStatus(), tick())
	}

	if m.form != nil {
		return m.updateForm(msg)
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		return m.handleKey(key)
	}
	return m, nil
}

func (m *Model) applyEvent(ev rpc.Event) tea.Cmd {
	switch ev.Type {
	case rpc.EventUsage:
		m.status.Usage = ev.Usage
	case rpc.EventSettings:
		if ev.Settings != nil {
			m.status.Settings = *ev.Settings
		}
	case rpc.EventTopic, rpc.EventTopicDeleted:
		return tea.Batch(m.loadTopics(), m.loadStatus())
	case rpc.EventJob:
		m.status.Job = ev.Job
		if ev.Job == nil {
			return tea.Batch(m.loadRuns(), m.loadStatus(), m.loadDigest(m.digestDate))
		}
	case rpc.EventLog:
		line := ev.At.Local().Format("15:04:05") + "  " + ev.Log
		m.logLines = append(m.logLines, line)
		if len(m.logLines) > maxLogLines {
			m.logLines = m.logLines[len(m.logLines)-maxLogLines:]
		}
		follow := m.logView.AtBottom()
		m.logView.SetContent(strings.Join(m.logLines, "\n"))
		if follow {
			m.logView.GotoBottom()
		}
	}
	return nil
}

func (m Model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if m.confirmDelete != "" {
		id := m.confirmDelete
		m.confirmDelete = ""
		if key == "y" || key == "Y" {
			return m, m.action("deleted "+id, rpc.MethodTopicDelete, rpc.IDParams{ID: id})
		}
		m.flash = "kept " + id
		return m, nil
	}
	switch key {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "tab":
		return m.switchTab((m.tab + 1) % tabCount)
	case "shift+tab":
		return m.switchTab((m.tab + tabCount - 1) % tabCount)
	case "1", "2", "3", "4":
		return m.switchTab(tab(key[0] - '1'))
	case "?":
		m.showHelp = !m.showHelp
		return m, nil
	case " ":
		paused := !m.status.Settings.Paused
		return m, m.updateSettings(rpc.SettingsPatch{Paused: &paused})
	case "r":
		return m, tea.Batch(m.loadStatus(), m.loadTopics(), m.loadRuns())
	}
	switch m.tab {
	case tabDashboard:
		return m.dashboardKey(key)
	case tabTopics:
		return m.topicsKey(k)
	case tabJobs:
		return m.jobsKey(k)
	case tabDigest:
		return m.digestKey(k)
	}
	return m, nil
}

func (m Model) switchTab(t tab) (tea.Model, tea.Cmd) {
	m.tab = t
	if t == tabDigest {
		return m, m.loadDigest(m.digestDate)
	}
	return m, nil
}

// resize lays out the tables and log for the window.
func (m *Model) resize() {
	body := max(m.height-5, 6) // header, tab bar, footer
	m.topicTable.SetColumns(topicColumns(m.width))
	m.topicTable.SetWidth(m.width)
	m.topicTable.SetHeight(max(body-8, 3))
	m.runTable.SetColumns(runColumns(m.width))
	m.runTable.SetWidth(m.width)
	// The runs table only takes the rows it needs; the log gets the rest.
	runsH := min(max(len(m.runs), 1)+1, max(body/2-2, 3))
	m.runTable.SetHeight(runsH)
	m.logView.Width = m.width
	m.logView.Height = max(body-runsH-5, 3)
	m.digestView.Width = m.width
	m.digestView.Height = max(body-2, 3)
}

func (m Model) View() string {
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n\n")
	if m.form != nil {
		b.WriteString(m.form.View())
	} else {
		switch m.tab {
		case tabDashboard:
			b.WriteString(m.dashboardView())
		case tabTopics:
			b.WriteString(m.topicsView())
		case tabJobs:
			b.WriteString(m.jobsView())
		case tabDigest:
			b.WriteString(m.digestScreen())
		}
	}
	body := b.String()
	footer := m.footer()
	// Pin the footer to the bottom of the window.
	pad := m.height - lipgloss.Height(body) - lipgloss.Height(footer)
	if pad > 0 {
		body += strings.Repeat("\n", pad)
	} else {
		body += "\n"
	}
	return clip(body+footer, m.width)
}

func (m Model) header() string {
	var tabs []string
	for i, name := range tabNames {
		label := fmt.Sprintf("%d %s", i+1, name)
		if tab(i) == m.tab {
			tabs = append(tabs, activeTabStyle.Render(label))
		} else {
			tabs = append(tabs, tabStyle.Render(label))
		}
	}
	left := titleStyle.Render("gmwd") + "  " + strings.Join(tabs, "")
	var state string
	switch {
	case m.disconnected:
		state = errStyle.Render("● disconnected")
	case !m.hasStatus:
		state = dimStyle.Render("● connecting")
	case m.status.Settings.Paused:
		state = warnStyle.Render("● paused")
	case m.status.Job != nil:
		state = okStyle.Render("● working")
	default:
		state = okStyle.Render("● active")
	}
	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(state), 1)
	return left + strings.Repeat(" ", gap) + state
}

func (m Model) footer() string {
	var line string
	switch {
	case m.disconnected:
		line = errStyle.Render("lost the daemon; restart it (gmwd daemon) and reopen gmwd")
	case m.err != nil:
		line = errStyle.Render("error: " + m.err.Error())
	case m.confirmDelete != "":
		line = warnStyle.Render(fmt.Sprintf("delete %s? its notes, reports and branch are kept  (y/n)", m.confirmDelete))
	case m.flash != "":
		line = dimStyle.Render(m.flash)
	}
	help := m.helpLine()
	if m.showHelp {
		help = m.fullHelp()
	}
	if line == "" {
		return helpStyle.Render(help)
	}
	return line + "\n" + helpStyle.Render(help)
}

func (m Model) helpLine() string {
	if m.form != nil {
		return "enter next · shift+tab back · esc cancel"
	}
	common := "tab switch · space pause/resume · ? help · q quit"
	switch m.tab {
	case tabDashboard:
		return "←/→ max burn · " + common
	case tabTopics:
		return "a add · e edit · d delete · p pause · +/- priority · enter details · " + common
	case tabJobs:
		return "↑/↓ runs · pgup/pgdn log · " + common
	case tabDigest:
		return "←/→ day · ↑/↓ scroll · " + common
	}
	return common
}

func (m Model) fullHelp() string {
	return strings.Join([]string{
		"Global:     tab/shift+tab or 1-4 switch screen · space pause/resume gmwd · r reload · q quit (the daemon keeps running)",
		"Dashboard:  ←/→ change max burn by 5% (how full gmwd may fill your week)",
		"Topics:     a add · e edit · d delete · p pause/resume topic · +/- priority · enter show details · ↑/↓ select",
		"Jobs:       ↑/↓ select a run · pgup/pgdn scroll the live log",
		"Digest:     ←/→ previous/next day · ↑/↓ pgup/pgdn scroll",
	}, "\n")
}

// clip cuts every line to width so nothing wraps.
func clip(s string, width int) string {
	if width <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if lipgloss.Width(l) > width {
			lines[i] = truncate(l, width)
		}
	}
	return strings.Join(lines, "\n")
}
