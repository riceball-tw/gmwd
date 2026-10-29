package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"gmwd/internal/config"
	"gmwd/internal/daemon"
	"gmwd/internal/usage"
)

func cmdDaemon(args []string, out io.Writer) error {
	fs := newFlags("daemon")
	poll := fs.Duration("poll", daemon.DefaultPollInterval, "how often to read usage with claude /usage (free)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	home, err := config.Home()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d := &daemon.Daemon{
		Home:         home,
		Prober:       usage.Prober{Claude: claudeBin, Dir: filepath.Join(home, "probe")},
		PollInterval: *poll,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(out, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
		},
	}
	return d.Run(ctx)
}
