// Command gmwd spends leftover Claude subscription quota on a topic backlog.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

const usageText = `gmwd — use leftover Claude quota on your backlog

Usage:
  gmwd                                open the TUI (needs the daemon)
  gmwd daemon                         run the daemon in the foreground
  gmwd install | uninstall            start/stop the daemon with launchd at login
  gmwd status [--json] [--refresh]    usage, reset times and daemon state
  gmwd add --kind K --title T --prompt P [--repo DIR] [--priority 1-5] ...
                                      queue a topic (research or solve)
  gmwd ls [--json]                    list topics
  gmwd rm ID                          delete a topic (keeps its files and branch)
  gmwd pause [ID] | resume [ID]       pause/resume the daemon, or one topic
  gmwd digest [--date YYYY-MM-DD]     what gmwd did that day and what to review
  gmwd config [--max-burn N] [--reserve N] [--idle-mode M] [--quiet-minutes N] [--notify on|off]
                                      show or change settings
  gmwd run ID                         run a topic now, in the foreground
  gmwd run --kind K --title T --prompt P [--repo DIR]
                                      add a topic and run it now
  gmwd version                        print the version
`

var commands = map[string]func(args []string, out io.Writer) error{
	"tui":    cmdTUI,
	"daemon": cmdDaemon,
	"status": cmdStatus,
	"add":    cmdAdd,
	"ls":     cmdList,
	"rm":     cmdRemove,
	"pause":  func(args []string, out io.Writer) error { return cmdPause(args, out, true) },
	"resume": func(args []string, out io.Writer) error { return cmdPause(args, out, false) },
	"run":    cmdRun,
	"config": cmdConfig,
	"digest": cmdDigest,
	"version": func(_ []string, out io.Writer) error {
		_, err := fmt.Fprintln(out, "gmwd", version)
		return err
	},
	"install":   cmdInstall,
	"uninstall": cmdUninstall,
}

func main() {
	name := "tui"
	if len(os.Args) >= 2 {
		name = os.Args[1]
	}
	if name == "-h" || name == "--help" || name == "help" {
		fmt.Print(usageText)
		return
	}
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "gmwd: unknown command %q\n\n%s", name, usageText)
		os.Exit(2)
	}
	var args []string
	if len(os.Args) > 2 {
		args = os.Args[2:]
	}
	if err := cmd(args, os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "gmwd:", err)
		os.Exit(1)
	}
}

// newFlags returns a FlagSet that reports errors instead of exiting.
func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// parseArgs parses flags that may come before or after positional
// arguments (`gmwd run ID --iterations 3`) and returns the positionals.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}
