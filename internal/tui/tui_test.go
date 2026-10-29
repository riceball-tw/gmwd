package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"gmwd/internal/config"
	"gmwd/internal/rpc"
	"gmwd/internal/store"
	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

var now = time.Date(2026, 10, 29, 22, 0, 0, 0, time.Local)

// fakeClient is a scripted daemon.
type fakeClient struct {
	mu       sync.Mutex
	calls    []call
	settings config.Settings
	topics   []topic.Topic
	runs     []store.Run
	status   rpc.StatusResult
	fail     map[string]error
	events   chan rpc.Event
}

type call struct {
	method string
	params string
}

func newFake() *fakeClient {
	f := &fakeClient{settings: config.DefaultSettings(), events: make(chan rpc.Event, 16), fail: map[string]error{}}
	f.status = rpc.StatusResult{
		PID: 42, StartedAt: now.Add(-time.Hour),
		Usage: &usage.Snapshot{
			FiveHour: usage.Window{Kind: usage.FiveHour, Utilization: 0.40, ResetsAt: now.Add(2 * time.Hour), ObservedAt: now.Add(-time.Minute), Source: usage.SourceUsageCommand},
			SevenDay: usage.Window{Kind: usage.SevenDay, Utilization: 0.32, ResetsAt: now.Add(50 * time.Hour), ObservedAt: now.Add(-time.Minute), Source: usage.SourceUsageCommand},
		},
		Topics: map[topic.Status]int{topic.Queued: 2, topic.Running: 1},
		Reason: "waiting: user active: Claude Code used 1m ago",
	}
	f.topics = []topic.Topic{
		{ID: "fix-auth", Title: "Fix flaky auth tests", Kind: topic.Solve, Prompt: "make auth tests stable", Repo: "/repo", Priority: 4, Status: topic.Queued},
		{ID: "sqlite", Title: "Compare SQLite drivers", Kind: topic.Research, Prompt: "pure-Go drivers", Priority: 1, Status: topic.Queued, Model: "opus"},
		{ID: "busy", Title: "Busy one", Kind: topic.Research, Prompt: "x", Priority: 3, Status: topic.Running},
	}
	f.runs = []store.Run{{ID: 1, TopicID: "sqlite", StartedAt: now.Add(-3 * time.Hour), EndedAt: now.Add(-2 * time.Hour), StopReason: "window_budget", Iterations: 3, WindowSpent: 0.05}}
	return f
}

func (f *fakeClient) Call(_ context.Context, method string, params, result any) error {
	raw, _ := json.Marshal(params)
	f.mu.Lock()
	f.calls = append(f.calls, call{method, string(raw)})
	err := f.fail[method]
	var res any
	switch method {
	case rpc.MethodStatus:
		st := f.status
		st.Settings = f.settings
		res = st
	case rpc.MethodTopicList:
		res = rpc.TopicsResult{Topics: f.topics}
	case rpc.MethodRunsList:
		res = rpc.RunsResult{Runs: f.runs}
	case rpc.MethodSettingsUpdate:
		var p rpc.SettingsPatch
		json.Unmarshal(raw, &p)
		if err == nil {
			f.settings = p.Apply(f.settings)
		}
		res = f.settings
	case rpc.MethodTopicAdd:
		var p rpc.TopicParams
		json.Unmarshal(raw, &p)
		p.Topic.ID = topic.Slug(p.Topic.Title)
		res = p.Topic
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if result != nil && res != nil {
		b, _ := json.Marshal(res)
		return json.Unmarshal(b, result)
	}
	return nil
}

func (f *fakeClient) Subscribe(context.Context) (<-chan rpc.Event, error) { return f.events, nil }

// callsTo returns the params of every call to method.
func (f *fakeClient) callsTo(method string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c.params)
		}
	}
	return out
}

// driver runs a model like Bubble Tea would: commands run in goroutines
// and their messages are fed back.
type driver struct {
	t       *testing.T
	m       tea.Model
	results chan tea.Msg
	quit    bool
}

func start(t *testing.T, f *fakeClient, w, h int) *driver {
	t.Helper()
	m := New(f)
	m.now = func() time.Time { return now }
	d := &driver{t: t, m: m, results: make(chan tea.Msg, 64)}
	d.apply(tea.WindowSizeMsg{Width: w, Height: h})
	d.run(m.Init())
	d.settle()
	return d
}

func (d *driver) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() { d.results <- cmd() }()
}

func (d *driver) apply(msg tea.Msg) {
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			d.run(c)
		}
	case tea.QuitMsg:
		d.quit = true
	default:
		var cmd tea.Cmd
		d.m, cmd = d.m.Update(msg)
		d.run(cmd)
	}
}

// settle processes messages until none arrive for a moment.
func (d *driver) settle() {
	for {
		select {
		case msg := <-d.results:
			d.apply(msg)
		case <-time.After(150 * time.Millisecond):
			return
		}
	}
}

func (d *driver) send(msgs ...tea.Msg) {
	for _, m := range msgs {
		d.apply(m)
		d.settle()
	}
}

func (d *driver) keys(keys ...string) {
	for _, k := range keys {
		d.send(keyMsg(k))
	}
}

func (d *driver) typeText(s string) {
	d.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
}

func keyMsg(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func (d *driver) view() string { return ansi.Strip(d.m.View()) }

func (d *driver) model() Model { return d.m.(Model) }

func has(t *testing.T, view string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(view, w) {
			t.Errorf("view lacks %q:\n%s", w, view)
		}
	}
}

func TestDashboard(t *testing.T) {
	d := start(t, newFake(), 100, 30)
	if os.Getenv("GMWD_SHOW_VIEW") != "" {
		t.Log("\n" + d.m.View())
	}
	has(t, d.view(),
		"gmwd", "1 Dashboard", "● active",
		"5h window", " 40%", "resets", "(in 2h00m)", "▒ 15% at the end is kept for you",
		"Week", " 32%", "┃ = max burn target", "usage read 1m ago via /usage",
		"Max burn", "◀", "▶  90%", "gmwd may fill your week up to 90% · 58 points left to use",
		"Status", "waiting: user active: Claude Code used 1m ago",
		"Job", "none", "1 running · 2 queued",
		"←/→ max burn")
}

func TestSlider(t *testing.T) {
	f := newFake()
	d := start(t, f, 100, 30)
	d.keys("right")
	has(t, d.view(), " 95%")
	d.keys("right", "right")
	if got := f.callsTo(rpc.MethodSettingsUpdate); len(got) != 2 || got[0] != `{"maxBurnPct":95}` || got[1] != `{"maxBurnPct":100}` {
		t.Errorf("settings calls = %v (no call past 100%%)", got)
	}
	has(t, d.view(), "100%")

	f2 := newFake()
	f2.settings.MaxBurnPct = 92 // set off-grid with gmwd config
	d2 := start(t, f2, 100, 30)
	d2.keys("left")
	if got := f2.callsTo(rpc.MethodSettingsUpdate); len(got) != 1 || got[0] != `{"maxBurnPct":85}` {
		t.Errorf("from 92 left = %v", got)
	}
	f3 := newFake()
	f3.settings.MaxBurnPct = 0
	d3 := start(t, f3, 100, 30)
	d3.keys("left")
	if got := f3.callsTo(rpc.MethodSettingsUpdate); len(got) != 0 {
		t.Errorf("went below 0: %v", got)
	}
}

func TestSliderErrorShowsAndReverts(t *testing.T) {
	f := newFake()
	f.fail[rpc.MethodSettingsUpdate] = errors.New("daemon says no")
	d := start(t, f, 100, 30)
	d.keys("right")
	has(t, d.view(), "error: daemon says no")
	d.send(tickMsg(now)) // the next status refresh restores the real value
	has(t, d.view(), "▶  90%")
}

func TestSpacePausesAndResumes(t *testing.T) {
	f := newFake()
	d := start(t, f, 100, 30)
	d.keys(" ")
	has(t, d.view(), "● paused", "paused — press space to resume")
	d.keys(" ")
	has(t, d.view(), "● active")
	if got := f.callsTo(rpc.MethodSettingsUpdate); len(got) != 2 || got[0] != `{"paused":true}` || got[1] != `{"paused":false}` {
		t.Errorf("calls = %v", got)
	}
}

func TestTopicsScreen(t *testing.T) {
	f := newFake()
	d := start(t, f, 110, 30)
	d.keys("2")
	has(t, d.view(), "fix-auth", "solve", "Fix flaky auth tests", "sqlite", "opus", "busy", "running", "a add")

	d.keys("+") // fix-auth 4 → 5
	upd := f.callsTo(rpc.MethodTopicUpdate)
	if len(upd) == 0 || !strings.Contains(upd[0], `"id":"fix-auth"`) || !strings.Contains(upd[0], `"priority":5`) {
		t.Fatalf("priority update = %v", upd)
	}
	d.keys("down", "-") // sqlite is at 1: no lower
	if n := len(f.callsTo(rpc.MethodTopicUpdate)); n != len(upd) {
		t.Errorf("lowered priority below 1")
	}
	d.keys("p")
	upd = f.callsTo(rpc.MethodTopicUpdate)
	if last := upd[len(upd)-1]; !strings.Contains(last, `"id":"sqlite"`) || !strings.Contains(last, `"status":"paused"`) {
		t.Errorf("pause = %s", last)
	}
	d.keys("down", "p") // busy is running: refused with a hint
	if n := len(f.callsTo(rpc.MethodTopicUpdate)); n != len(upd) {
		t.Error("paused a running topic")
	}
	has(t, d.view(), "busy is running")

	d.keys("d")
	has(t, d.view(), "delete busy?", "(y/n)")
	d.keys("n")
	has(t, d.view(), "kept busy")
	d.keys("d", "y")
	if del := f.callsTo(rpc.MethodTopicDelete); len(del) != 1 || del[0] != `{"id":"busy"}` {
		t.Errorf("delete = %v", del)
	}

	d.keys("enter")
	has(t, d.view(), "Busy one", "Prompt")
}

func TestTopicDetailsShowRuns(t *testing.T) {
	d := start(t, newFake(), 110, 40)
	d.keys("2", "down", "enter")
	has(t, d.view(), "Compare SQLite drivers", "pure-Go drivers", "Runs", "window budget", "3 iterations")
}

func TestAddTopicByTyping(t *testing.T) {
	f := newFake()
	d := start(t, f, 100, 40)
	d.keys("2", "a")
	has(t, d.view(), "Add a topic", "Kind", "Prompt")
	d.typeText("Why is CI slow")
	d.keys("enter") // → kind
	d.keys("enter") // research → prompt
	d.typeText("Look at the last 20 runs")
	d.keys("enter") // → repo (optional for research)
	d.keys("enter") // → group 2: priority
	d.keys("enter") // 3 → model
	d.keys("enter") // sonnet → done when
	d.typeText("report names the slowest step")
	d.keys("enter") // submit
	add := f.callsTo(rpc.MethodTopicAdd)
	if len(add) != 1 {
		t.Fatalf("topic.add calls = %v\nview:\n%s", add, d.view())
	}
	var p rpc.TopicParams
	json.Unmarshal([]byte(add[0]), &p)
	got := p.Topic
	if got.Title != "Why is CI slow" || got.Kind != topic.Research || got.Prompt != "Look at the last 20 runs" ||
		got.Priority != 3 || got.Model != "" || got.DoneWhen != "report names the slowest step" || got.Repo != "" {
		t.Errorf("added %+v", got)
	}
	if d.model().form != nil {
		t.Error("form still open")
	}
	has(t, d.view(), "added why-is-ci-slow")
}

func TestFormEscCancels(t *testing.T) {
	f := newFake()
	d := start(t, f, 100, 40)
	d.keys("2", "a")
	d.typeText("half")
	d.keys("esc")
	if d.model().form != nil || len(f.callsTo(rpc.MethodTopicAdd)) != 0 {
		t.Error("esc didn't cancel")
	}
	has(t, d.view(), "cancelled")
	// Keys work normally again.
	d.keys("1")
	has(t, d.view(), "Max burn")
}

func TestEditKeepsIdentity(t *testing.T) {
	f := newFake()
	d := start(t, f, 100, 40)
	d.keys("2", "e")
	has(t, d.view(), "Edit fix-auth")
	m := d.model()
	m.formValues.Prompt = "make auth tests stable on CI"
	var cmd tea.Cmd
	d.m, cmd = m.submitForm()
	d.run(cmd)
	d.settle()
	upd := f.callsTo(rpc.MethodTopicUpdate)
	if len(upd) != 1 {
		t.Fatalf("updates = %v", upd)
	}
	var p rpc.TopicParams
	json.Unmarshal([]byte(upd[0]), &p)
	if p.Topic.ID != "fix-auth" || p.Topic.Repo != "/repo" || p.Topic.Status != topic.Queued || p.Topic.Prompt != "make auth tests stable on CI" || p.Topic.Kind != topic.Solve {
		t.Errorf("edit sent %+v", p.Topic)
	}
}

func TestLiveEvents(t *testing.T) {
	f := newFake()
	d := start(t, f, 110, 40)
	topicLoads := len(f.callsTo(rpc.MethodTopicList))
	runLoads := len(f.callsTo(rpc.MethodRunsList))

	job := &rpc.JobInfo{TopicID: "sqlite", StartedAt: now.Add(-3 * time.Minute), Iteration: 2, Tokens: usage.TokenCounts{Output: 1500}}
	f.events <- rpc.Event{Type: rpc.EventJob, Job: job}
	f.events <- rpc.Event{Type: rpc.EventLog, At: now, Log: "iteration 2: running sqlite (sonnet)"}
	u := *f.status.Usage
	u.FiveHour.Utilization = 0.47
	f.events <- rpc.Event{Type: rpc.EventUsage, Usage: &u}
	d.settle()
	has(t, d.view(), "● working", "sqlite · iteration 2 · 1.5k tokens · running 3m", " 47%")
	d.keys("3")
	has(t, d.view(), "Running", "sqlite · iteration 2", "Live log", "iteration 2: running sqlite (sonnet)", "window budget")

	f.events <- rpc.Event{Type: rpc.EventJob} // job ended
	f.events <- rpc.Event{Type: rpc.EventTopic, Topic: &topic.Topic{ID: "sqlite"}}
	d.settle()
	if len(f.callsTo(rpc.MethodRunsList)) <= runLoads || len(f.callsTo(rpc.MethodTopicList)) <= topicLoads {
		t.Error("job end / topic change didn't reload runs and topics")
	}
	has(t, d.view(), "nothing — waiting")
}

func TestDisconnect(t *testing.T) {
	f := newFake()
	d := start(t, f, 100, 30)
	close(f.events)
	d.settle()
	has(t, d.view(), "● disconnected", "lost the daemon")
	d.keys("q")
	if !d.quit {
		t.Error("q didn't quit")
	}
}

func TestLayoutFitsWindow(t *testing.T) {
	for _, size := range [][2]int{{60, 20}, {80, 24}, {200, 50}} {
		f := newFake()
		f.events <- rpc.Event{Type: rpc.EventLog, At: now, Log: strings.Repeat("long log line ", 30)}
		d := start(t, f, size[0], size[1])
		for _, tabKey := range []string{"1", "2", "3"} {
			d.keys(tabKey)
			view := d.m.View()
			lines := strings.Split(view, "\n")
			for i, l := range lines {
				if w := lipgloss.Width(l); w > size[0] {
					t.Errorf("%dx%d tab %s line %d is %d wide: %q", size[0], size[1], tabKey, i, w, ansi.Strip(l))
				}
			}
			if len(lines) > size[1] && size[1] >= 24 {
				t.Errorf("%dx%d tab %s: %d lines", size[0], size[1], tabKey, len(lines))
			}
		}
	}
}

func TestValidateRepo(t *testing.T) {
	dir := t.TempDir()
	home, _ := os.UserHomeDir()
	cases := []struct {
		path string
		kind topic.Kind
		ok   bool
	}{
		{"", topic.Research, true},
		{"", topic.Solve, false},
		{dir, topic.Solve, true},
		{"relative/path", topic.Solve, false},
		{filepath.Join(dir, "missing"), topic.Research, false},
		{"~/", topic.Solve, home != ""},
	}
	for _, c := range cases {
		if err := validateRepo(c.path, c.kind); (err == nil) != c.ok {
			t.Errorf("validateRepo(%q, %s) = %v", c.path, c.kind, err)
		}
	}
}
