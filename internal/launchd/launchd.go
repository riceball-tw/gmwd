// Package launchd installs the gmwd daemon as a per-user launchd agent.
package launchd

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Label is the agent's launchd label.
const Label = "com.gmwd.daemon"

// Agent describes the launchd job.
type Agent struct {
	Label   string
	Program string   // absolute path to the gmwd binary
	Args    []string // arguments after the program
	Env     map[string]string
	LogPath string // stdout and stderr
}

// Plist renders the agent. The daemon restarts if it crashes, stays down
// after a clean stop, and runs as a background process so it yields to the
// user's own work.
func (a Agent) Plist() ([]byte, error) {
	if !filepath.IsAbs(a.Program) {
		return nil, fmt.Errorf("program %q must be an absolute path", a.Program)
	}
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	key := func(k string) { fmt.Fprintf(&b, "\t<key>%s</key>\n", esc(k)) }
	str := func(indent, v string) { fmt.Fprintf(&b, "%s<string>%s</string>\n", indent, esc(v)) }

	key("Label")
	str("\t", a.Label)
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, arg := range append([]string{a.Program}, a.Args...) {
		str("\t\t", arg)
	}
	b.WriteString("\t</array>\n")
	if len(a.Env) > 0 {
		key("EnvironmentVariables")
		b.WriteString("\t<dict>\n")
		keys := make([]string, 0, len(a.Env))
		for k := range a.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n", esc(k))
			str("\t\t", a.Env[k])
		}
		b.WriteString("\t</dict>\n")
	}
	key("RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("KeepAlive")
	b.WriteString("\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	key("ThrottleInterval")
	b.WriteString("\t<integer>30</integer>\n")
	key("ProcessType")
	str("\t", "Background")
	if a.LogPath != "" {
		key("StandardOutPath")
		str("\t", a.LogPath)
		key("StandardErrorPath")
		str("\t", a.LogPath)
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes(), nil
}

func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// PlistPath is where the agent lives: ~/Library/LaunchAgents/<label>.plist.
func PlistPath(userHome, label string) string {
	return filepath.Join(userHome, "Library", "LaunchAgents", label+".plist")
}

// Launchctl runs launchctl; tests substitute a fake.
type Launchctl func(args ...string) ([]byte, error)

// System runs the real launchctl.
func System(args ...string) ([]byte, error) {
	return exec.Command("launchctl", args...).CombinedOutput()
}

func domain(uid int) string { return "gui/" + strconv.Itoa(uid) }

// Install writes the plist and (re)loads the agent, so installing again
// picks up a new binary or environment.
func Install(a Agent, plistPath string, uid int, lc Launchctl) error {
	data, err := a.Plist()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return err
	}
	tmp := plistPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, plistPath); err != nil {
		os.Remove(tmp)
		return err
	}
	// Not loaded yet is fine.
	lc("bootout", domain(uid)+"/"+a.Label)
	if out, err := lc("bootstrap", domain(uid), plistPath); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Uninstall stops the agent and removes its plist. Doing it twice is fine.
func Uninstall(label, plistPath string, uid int, lc Launchctl) error {
	lc("bootout", domain(uid)+"/"+label)
	if err := os.Remove(plistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

var pidRe = regexp.MustCompile(`(?m)^\s*pid = (\d+)`)

// Status reports whether the agent is loaded and its pid (0 if not running).
func Status(label string, uid int, lc Launchctl) (loaded bool, pid int) {
	out, err := lc("print", domain(uid)+"/"+label)
	if err != nil {
		return false, 0
	}
	if m := pidRe.FindSubmatch(out); m != nil {
		pid, _ = strconv.Atoi(string(m[1]))
	}
	return true, pid
}
