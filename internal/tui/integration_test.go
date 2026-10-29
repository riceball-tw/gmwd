package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"gmwd/internal/daemon"
	"gmwd/internal/rpc"
	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

type staticProber struct{}

func (staticProber) Probe(context.Context) (usage.Snapshot, error) {
	n := time.Now()
	return usage.Snapshot{
		FiveHour: usage.Window{Kind: usage.FiveHour, Utilization: 0.25, ResetsAt: n.Add(3 * time.Hour), ObservedAt: n, Source: usage.SourceUsageCommand},
		SevenDay: usage.Window{Kind: usage.SevenDay, Utilization: 0.40, ResetsAt: n.Add(60 * time.Hour), ObservedAt: n, Source: usage.SourceUsageCommand},
	}, nil
}

// The TUI against a real daemon over the real socket.
func TestAgainstRealDaemon(t *testing.T) {
	home, err := os.MkdirTemp("", "gt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("GMWD_SOCKET", "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	d := &daemon.Daemon{Home: home, Prober: staticProber{}, PollInterval: time.Hour}
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	sock := filepath.Join(home, "gmwd.sock")
	var c *rpc.Client
	for deadline := time.Now().Add(5 * time.Second); ; {
		if c, err = rpc.Dial(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { c.Close() })
	// Wait for the first usage reading so the dashboard has numbers.
	for deadline := time.Now().Add(5 * time.Second); ; {
		var st rpc.StatusResult
		c.Call(ctx, rpc.MethodStatus, nil, &st)
		if st.Usage != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no usage")
		}
		time.Sleep(10 * time.Millisecond)
	}

	m := New(c)
	dr := &driver{t: t, m: m, results: make(chan tea.Msg, 64)}
	dr.apply(tea.WindowSizeMsg{Width: 100, Height: 40})
	dr.run(m.Init())
	dr.settle()
	has(t, dr.view(), " 25%", " 40%", "▶  90%")

	dr.keys("left", "left")
	other, _ := rpc.Dial(sock)
	defer other.Close()
	var st rpc.StatusResult
	other.Call(ctx, rpc.MethodStatus, nil, &st)
	if st.Settings.MaxBurnPct != 80 {
		t.Errorf("daemon max burn = %v, want 80", st.Settings.MaxBurnPct)
	}

	dr.keys("2", "a")
	dr.typeText("Real daemon topic")
	dr.keys("enter", "enter")
	dr.typeText("check it end to end")
	dr.keys("enter", "enter", "enter", "enter", "enter")
	var tp topic.Topic
	if err := other.Call(ctx, rpc.MethodTopicGet, rpc.IDParams{ID: "real-daemon-topic"}, &tp); err != nil {
		t.Fatalf("topic not in daemon: %v\n%s", err, dr.view())
	}
	has(t, dr.view(), "real-daemon-topic", "added real-daemon-topic")

	// A change made elsewhere shows up live.
	other.Call(ctx, rpc.MethodTopicAdd, rpc.TopicParams{Topic: topic.Topic{ID: "from-cli", Title: "Added from the CLI", Kind: topic.Research, Prompt: "p"}}, nil)
	dr.settle()
	has(t, dr.view(), "from-cli", "Added from the CLI")
	paused := true
	other.Call(ctx, rpc.MethodSettingsUpdate, rpc.SettingsPatch{Paused: &paused}, nil)
	dr.settle()
	has(t, dr.view(), "● paused")
}
