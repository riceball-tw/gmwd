package launchd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func agent() Agent {
	return Agent{
		Label:   Label,
		Program: "/Users/me/bin & co/gmwd",
		Args:    []string{"daemon"},
		Env:     map[string]string{"PATH": "/opt/homebrew/bin:/usr/bin", "GMWD_HOME": "/Users/me/<x>"},
		LogPath: "/Users/me/.gmwd/daemon.log",
	}
}

func TestPlistIsValidAndEscaped(t *testing.T) {
	data, err := agent().Plist()
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		"<string>/Users/me/bin &amp; co/gmwd</string>",
		"<string>daemon</string>",
		"<key>GMWD_HOME</key>",
		"<string>/Users/me/&lt;x&gt;</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
		"<string>Background</string>",
		"<key>StandardErrorPath</key>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist lacks %q:\n%s", want, s)
		}
	}
	// macOS can check it for real.
	if _, err := exec.LookPath("plutil"); err == nil {
		path := filepath.Join(t.TempDir(), "a.plist")
		os.WriteFile(path, data, 0o644)
		if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
			t.Errorf("plutil: %v: %s", err, out)
		}
		out, err := exec.Command("plutil", "-extract", "ProgramArguments.0", "raw", path).Output()
		if err != nil || strings.TrimSpace(string(out)) != "/Users/me/bin & co/gmwd" {
			t.Errorf("ProgramArguments.0 = %q, %v", out, err)
		}
	}
}

func TestPlistNeedsAbsoluteProgram(t *testing.T) {
	a := agent()
	a.Program = "gmwd"
	if _, err := a.Plist(); err == nil {
		t.Error("relative program accepted")
	}
}

type fakeLaunchctl struct {
	calls [][]string
	fail  map[string]error // by subcommand
	out   map[string]string
}

func (f *fakeLaunchctl) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	return []byte(f.out[args[0]]), f.fail[args[0]]
}

func TestInstallWritesPlistAndReloads(t *testing.T) {
	path := PlistPath(t.TempDir(), Label)
	lc := &fakeLaunchctl{fail: map[string]error{"bootout": errors.New("not loaded")}}
	if err := Install(agent(), path, 501, lc.run); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"bootout", "gui/501/" + Label},
		{"bootstrap", "gui/501", path},
	}
	if !reflect.DeepEqual(lc.calls, want) {
		t.Errorf("calls = %v, want %v", lc.calls, want)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "<string>daemon</string>") {
		t.Errorf("plist: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
}

func TestInstallReportsBootstrapFailure(t *testing.T) {
	path := PlistPath(t.TempDir(), Label)
	lc := &fakeLaunchctl{
		fail: map[string]error{"bootstrap": errors.New("exit status 5")},
		out:  map[string]string{"bootstrap": "Bootstrap failed: 5: Input/output error\n"},
	}
	err := Install(agent(), path, 501, lc.run)
	if err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Errorf("err = %v", err)
	}
}

func TestUninstallIsIdempotent(t *testing.T) {
	path := PlistPath(t.TempDir(), Label)
	lc := &fakeLaunchctl{}
	if err := Install(agent(), path, 501, lc.run); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		lc.calls = nil
		lc.fail = map[string]error{"bootout": errors.New("not loaded")}
		if err := Uninstall(Label, path, 501, lc.run); err != nil {
			t.Fatal(err)
		}
		if len(lc.calls) != 1 || lc.calls[0][0] != "bootout" {
			t.Errorf("calls = %v", lc.calls)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("plist still there")
	}
}

func TestStatus(t *testing.T) {
	lc := &fakeLaunchctl{out: map[string]string{"print": "gui/501/com.gmwd.daemon = {\n\tactive count = 1\n\tstate = running\n\tpid = 4242\n"}}
	if loaded, pid := Status(Label, 501, lc.run); !loaded || pid != 4242 {
		t.Errorf("loaded %v pid %d", loaded, pid)
	}
	lc = &fakeLaunchctl{fail: map[string]error{"print": errors.New("could not find service")}}
	if loaded, pid := Status(Label, 501, lc.run); loaded || pid != 0 {
		t.Errorf("not loaded: loaded %v pid %d", loaded, pid)
	}
}
