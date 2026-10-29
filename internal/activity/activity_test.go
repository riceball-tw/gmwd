package activity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gmwd/internal/config"
)

var ctx = context.Background()

// logsWithCLIActivity makes a log root whose newest interactive record is
// at the given time.
func logsWithCLIActivity(t *testing.T, at time.Time) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "proj", "s.jsonl")
	os.MkdirAll(filepath.Dir(path), 0o700)
	line := fmt.Sprintf(`{"type":"user","entrypoint":"cli","timestamp":%q}`+"\n", at.UTC().Format(time.RFC3339Nano))
	os.WriteFile(path, []byte(line), 0o600)
	os.Chtimes(path, at, at)
	return root
}

func settings(mode config.IdleMode, quiet int) config.Settings {
	s := config.DefaultSettings()
	s.IdleMode, s.QuietMinutes = mode, quiet
	return s
}

func hid(d time.Duration, err error) func(context.Context) (time.Duration, error) {
	return func(context.Context) (time.Duration, error) { return d, err }
}

func TestNotUsingClaude(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		lastUse  time.Duration // ago
		wantIdle bool
		reason   string
	}{
		{"just used", 2 * time.Minute, false, "user active: Claude Code used 2m ago"},
		{"used 9m ago", 9 * time.Minute, false, "user active"},
		{"quiet for longer", 25 * time.Minute, true, "idle: no Claude Code use for 10m"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &Detector{LogRoot: logsWithCLIActivity(t, now.Add(-c.lastUse)), Now: func() time.Time { return now },
				HIDIdle: func(context.Context) (time.Duration, error) {
					t.Error("HID read in not-using-claude mode")
					return 0, nil
				}}
			st, err := d.Check(ctx, settings(config.IdleNotUsingClaude, 10))
			if err != nil {
				t.Fatal(err)
			}
			if st.Idle != c.wantIdle || !strings.HasPrefix(st.Reason, c.reason) {
				t.Errorf("got %+v", st)
			}
		})
	}
}

func TestNoLogsAtAllIsIdle(t *testing.T) {
	d := &Detector{LogRoot: filepath.Join(t.TempDir(), "none")}
	st, err := d.Check(ctx, settings(config.IdleNotUsingClaude, 10))
	if err != nil || !st.Idle {
		t.Errorf("got %+v, %v", st, err)
	}
}

func TestAwayMode(t *testing.T) {
	now := time.Now()
	quietLogs := logsWithCLIActivity(t, now.Add(-time.Hour))
	cases := []struct {
		name     string
		root     string
		hid      func(context.Context) (time.Duration, error)
		wantIdle bool
		reason   string
		wantErr  bool
	}{
		{"typing, not in Claude", quietLogs, hid(30*time.Second, nil), false, "user active: input 30s ago", false},
		{"away", quietLogs, hid(40*time.Minute, nil), true, "idle: away for 40m", false},
		{"away but Claude used", logsWithCLIActivity(t, now.Add(-time.Minute)), hid(40*time.Minute, nil), false, "user active: Claude Code", false},
		{"can't read HID", quietLogs, hid(0, errors.New("no ioreg")), false, "can't read keyboard idle time", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &Detector{LogRoot: c.root, HIDIdle: c.hid, Now: func() time.Time { return now }}
			st, err := d.Check(ctx, settings(config.IdleAway, 10))
			if (err != nil) != c.wantErr {
				t.Errorf("err = %v", err)
			}
			if st.Idle != c.wantIdle || !strings.HasPrefix(st.Reason, c.reason) {
				t.Errorf("got %+v", st)
			}
		})
	}
}

func TestQuietZeroDisablesDetection(t *testing.T) {
	d := &Detector{LogRoot: logsWithCLIActivity(t, time.Now()), HIDIdle: hid(0, errors.New("unused"))}
	st, err := d.Check(ctx, settings(config.IdleAway, 0))
	if err != nil || !st.Idle || st.Reason != "idle detection off" {
		t.Errorf("got %+v, %v", st, err)
	}
}

func TestParseHIDIdle(t *testing.T) {
	out := []byte(`+-o IOHIDSystem  <class IOHIDSystem>
    {
      "HIDIdleTime" = 125000000000
      "HIDKeyboardModifierMappingPairs" = ()
    }`)
	if d, err := parseHIDIdle(out); err != nil || d != 125*time.Second {
		t.Errorf("got %v, %v", d, err)
	}
	if _, err := parseHIDIdle([]byte("nothing")); err == nil {
		t.Error("parsed garbage")
	}
}

func TestSystemHIDIdleOnMac(t *testing.T) {
	if _, err := exec.LookPath("ioreg"); err != nil {
		t.Skip("no ioreg")
	}
	d, err := SystemHIDIdle(ctx)
	if err != nil && os.Getenv("CI") != "" {
		t.Skipf("no HID idle time on this CI machine: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if d < 0 || d > 30*24*time.Hour {
		t.Errorf("implausible idle time %v", d)
	}
}
