package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gmwd/internal/config"
	"gmwd/internal/launchd"
)

// Swapped out in tests.
var (
	launchctl  launchd.Launchctl = launchd.System
	executable                   = os.Executable
)

func cmdInstall(args []string, out io.Writer) error {
	if len(args) > 0 {
		return errors.New("usage: gmwd install")
	}
	exe, err := executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	if strings.Contains(exe, string(filepath.Separator)+"go-build") {
		return fmt.Errorf("%s is a temporary `go run` binary; build gmwd (go build -o ~/bin/gmwd ./cmd/gmwd) and install from there", exe)
	}
	home, err := config.Home()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// launchd starts agents with a bare PATH; the daemon needs claude and git.
	env := map[string]string{"PATH": os.Getenv("PATH")}
	for _, k := range []string{"GMWD_HOME", "GMWD_SOCKET", "GMWD_CLAUDE"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	a := launchd.Agent{
		Label:   launchd.Label,
		Program: exe,
		Args:    []string{"daemon"},
		Env:     env,
		LogPath: filepath.Join(home, "daemon.log"),
	}
	plist := launchd.PlistPath(userHome, launchd.Label)
	if err := launchd.Install(a, plist, os.Getuid(), launchctl); err != nil {
		return err
	}
	fmt.Fprintf(out, "installed %s\n", plist)
	st, err := waitForDaemon(10 * time.Second)
	if err != nil {
		return fmt.Errorf("the agent is installed but the daemon isn't answering (%v); see %s", err, a.LogPath)
	}
	fmt.Fprintf(out, "daemon running (pid %d); logs in %s\n", st.PID, a.LogPath)
	return nil
}

func cmdUninstall(args []string, out io.Writer) error {
	if len(args) > 0 {
		return errors.New("usage: gmwd uninstall")
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	plist := launchd.PlistPath(userHome, launchd.Label)
	if err := launchd.Uninstall(launchd.Label, plist, os.Getuid(), launchctl); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := dialDaemon()
		if err != nil {
			fmt.Fprintln(out, "daemon stopped and removed from launchd; your topics and data in the gmwd home are kept")
			return nil
		}
		c.Close()
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("removed from launchd, but a daemon is still answering on the socket (started by hand?)")
}
