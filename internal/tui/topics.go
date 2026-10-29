package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"

	"gmwd/internal/rpc"
	"gmwd/internal/topic"
)

func topicColumns(width int) []table.Column {
	fixed := 10 + 4 + 8 + 7 + 5*2 // kind, pri, status, model, cell padding
	idW := min(max((width-fixed)/3, 12), 32)
	return []table.Column{
		{Title: "ID", Width: idW},
		{Title: "Kind", Width: 10},
		{Title: "Pri", Width: 4},
		{Title: "Status", Width: 8},
		{Title: "Model", Width: 7},
		{Title: "Title", Width: max(width-fixed-idW-2, 10)},
	}
}

func (m *Model) syncTopicTable() {
	rows := make([]table.Row, len(m.topics))
	for i, t := range m.topics {
		rows[i] = table.Row{t.ID, string(t.Kind), strconv.Itoa(t.Priority), string(t.Status), t.ModelOrDefault(), t.Title}
	}
	m.topicTable.SetRows(rows)
	if c := m.topicTable.Cursor(); c >= len(rows) && len(rows) > 0 {
		m.topicTable.SetCursor(len(rows) - 1)
	}
}

// selected returns the topic under the cursor.
func (m Model) selected() (topic.Topic, bool) {
	i := m.topicTable.Cursor()
	if i < 0 || i >= len(m.topics) {
		return topic.Topic{}, false
	}
	return m.topics[i], true
}

func (m Model) topicsKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "a":
		return m.openForm(topic.Topic{Kind: topic.Research, Priority: 3, Model: topic.DefaultModel})
	case "enter":
		m.details = !m.details
		return m, nil
	}
	t, ok := m.selected()
	switch k.String() {
	case "e":
		if ok {
			return m.openForm(t)
		}
	case "d", "delete", "backspace":
		if ok {
			m.confirmDelete = t.ID
		}
		return m, nil
	case "p":
		if !ok {
			return m, nil
		}
		switch t.Status {
		case topic.Paused:
			t.Status = topic.Queued
		case topic.Running:
			m.flash = t.ID + " is running; press space to pause gmwd (it stops after this iteration)"
			return m, nil
		default:
			t.Status = topic.Paused
		}
		return m, m.action(fmt.Sprintf("%s %s", t.ID, t.Status), rpc.MethodTopicUpdate, rpc.TopicParams{Topic: t})
	case "+", "=", "-":
		if !ok {
			return m, nil
		}
		p := t.Priority + 1
		if k.String() == "-" {
			p = t.Priority - 1
		}
		if p < 1 || p > 5 {
			return m, nil
		}
		t.Priority = p
		return m, m.action(fmt.Sprintf("%s priority %d", t.ID, p), rpc.MethodTopicUpdate, rpc.TopicParams{Topic: t})
	}
	var cmd tea.Cmd
	m.topicTable, cmd = m.topicTable.Update(k)
	return m, cmd
}

func (m Model) topicsView() string {
	if len(m.topics) == 0 {
		return dimStyle.Render("No topics yet. Press a to add one: something to research, or a bug to fix in a repo.")
	}
	view := m.topicTable.View()
	if m.details {
		if t, ok := m.selected(); ok {
			view += "\n\n" + topicDetails(t, m.runs)
		}
	}
	return view
}

func topicDetails(t topic.Topic, runs []storeRun) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(t.Title) + dimStyle.Render("  "+t.ID) + "\n")
	b.WriteString(row("Prompt", strings.ReplaceAll(strings.TrimSpace(t.Prompt), "\n", " ")) + "\n")
	if t.Repo != "" {
		b.WriteString(row("Repo", t.Repo) + "\n")
	}
	if t.Kind == topic.Solve {
		b.WriteString(row("Branch", t.Branch()) + "\n")
	}
	if t.DoneWhen != "" {
		b.WriteString(row("Done when", t.DoneWhen) + "\n")
	}
	n := 0
	for _, r := range runs {
		if r.TopicID != t.ID {
			continue
		}
		if n == 0 {
			b.WriteString(row("Runs", ""))
		} else {
			b.WriteString(row("", ""))
		}
		b.WriteString(fmt.Sprintf("%s  %s  %d iterations  %.0f%% of 5h\n", r.StartedAt.Local().Format("Mon 15:04"), runStop(r), r.Iterations, r.WindowSpent*100))
		if n++; n == 3 {
			break
		}
	}
	return b.String()
}

// topicForm holds the add/edit form's values.
type topicForm struct {
	editing  string // id of the topic being edited, "" when adding
	orig     topic.Topic
	Title    string
	Kind     topic.Kind
	Prompt   string
	Repo     string
	Priority int
	Model    string
	DoneWhen string
}

func (m Model) openForm(t topic.Topic) (tea.Model, tea.Cmd) {
	v := &topicForm{orig: t, editing: t.ID, Title: t.Title, Kind: t.Kind, Prompt: t.Prompt, Repo: t.Repo,
		Priority: t.Priority, Model: t.ModelOrDefault(), DoneWhen: t.DoneWhen}
	title := "Add a topic"
	if v.editing != "" {
		title = "Edit " + v.editing
	}
	m.formValues = v
	m.form = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().Title(title).Description("Short title").Value(&v.Title).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return errors.New("give it a title")
					}
					return nil
				}),
			huh.NewSelect[topic.Kind]().Title("Kind").Options(
				huh.NewOption("research: write a report with sources", topic.Research),
				huh.NewOption("solve: commit fixes on a gmwd/* branch", topic.Solve),
			).Value(&v.Kind),
			huh.NewText().Title("Prompt").Description("What to research or fix").Value(&v.Prompt).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return errors.New("say what to do")
					}
					return nil
				}),
			huh.NewInput().Title("Repo").Description("Solve: the git repo. Research: optional read-only context dir.").
				Value(&v.Repo).Validate(func(s string) error { return validateRepo(s, v.Kind) }),
		),
		huh.NewGroup(
			huh.NewSelect[int]().Title("Priority").Options(
				huh.NewOption("5 highest", 5), huh.NewOption("4", 4), huh.NewOption("3 normal", 3),
				huh.NewOption("2", 2), huh.NewOption("1 lowest", 1),
			).Value(&v.Priority),
			huh.NewSelect[string]().Title("Model").Options(
				huh.NewOption("sonnet: more iterations per quota", "sonnet"),
				huh.NewOption("opus: better quality, uses more", "opus"),
				huh.NewOption("haiku: cheapest", "haiku"),
			).Value(&v.Model),
			huh.NewInput().Title("Done when (optional)").Description("When gmwd should consider it finished").Value(&v.DoneWhen),
		),
	).WithShowHelp(false).WithWidth(min(m.width, 90))
	return m, m.form.Init()
}

// validateRepo checks a repo path before it is sent; the daemon checks too.
func validateRepo(s string, kind topic.Kind) error {
	s = strings.TrimSpace(s)
	if s == "" {
		if kind == topic.Solve {
			return errors.New("solve topics need a git repo")
		}
		return nil
	}
	p := expandHome(s)
	if !filepath.IsAbs(p) {
		return errors.New("use an absolute path (or ~/...)")
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		return errors.New("no such directory")
	}
	return nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// topic builds the topic to send from the form.
func (v *topicForm) topic() topic.Topic {
	t := v.orig
	t.Title = strings.TrimSpace(v.Title)
	t.Kind = v.Kind
	t.Prompt = strings.TrimSpace(v.Prompt)
	t.Repo = expandHome(strings.TrimSpace(v.Repo))
	t.Priority = v.Priority
	t.Model = v.Model
	if t.Model == topic.DefaultModel {
		t.Model = ""
	}
	t.DoneWhen = strings.TrimSpace(v.DoneWhen)
	return t
}

func (m Model) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok && (k.String() == "esc" || k.String() == "ctrl+c") {
		m.form, m.formValues = nil, nil
		m.flash = "cancelled"
		return m, nil
	}
	f, cmd := m.form.Update(msg)
	if form, ok := f.(*huh.Form); ok {
		m.form = form
	}
	switch m.form.State {
	case huh.StateCompleted:
		return m.submitForm()
	case huh.StateAborted:
		m.form, m.formValues = nil, nil
		return m, nil
	}
	return m, cmd
}

// submitForm sends the form's topic to the daemon and closes the form.
func (m Model) submitForm() (tea.Model, tea.Cmd) {
	v := m.formValues
	m.form, m.formValues = nil, nil
	t := v.topic()
	if v.editing != "" {
		return m, m.action("saved "+t.ID, rpc.MethodTopicUpdate, rpc.TopicParams{Topic: t})
	}
	return m, func() tea.Msg {
		var added topic.Topic
		if err := m.call(rpc.MethodTopicAdd, rpc.TopicParams{Topic: t}, &added); err != nil {
			return errMsg{err}
		}
		return doneMsg("added " + added.ID)
	}
}
