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

	"gmwd/internal/activity"
	"gmwd/internal/config"
	"gmwd/internal/daemon"
	"gmwd/internal/rpc"
	"gmwd/internal/runner"
	"gmwd/internal/scheduler"
	"gmwd/internal/usage"
)

func cmdDaemon(args []string, out io.Writer) error {
	fs := newFlags("daemon")
	poll := fs.Duration("poll", daemon.DefaultPollInterval, "how often to read usage with claude /usage (free)")
	tick := fs.Duration("tick", scheduler.DefaultInterval, "how often to reconsider starting a job")
	noJobs := fs.Bool("no-jobs", false, "serve and poll usage, but never start jobs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := newDaemon(out, *poll, *tick, !*noJobs)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return d.Run(ctx)
}

// newDaemon builds the daemon `gmwd daemon` runs.
func newDaemon(out io.Writer, poll, tick time.Duration, jobs bool) (*daemon.Daemon, error) {
	home, err := config.Home()
	if err != nil {
		return nil, err
	}
	logRoot, err := usage.DefaultLogRoot()
	if err != nil {
		return nil, err
	}
	logf := func(format string, args ...any) {
		fmt.Fprintf(out, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	}
	d := &daemon.Daemon{
		Home:         home,
		Prober:       usage.Prober{Claude: claudeBin, Dir: filepath.Join(home, "probe")},
		PollInterval: poll,
		TickInterval: tick,
		Activity:     &activity.Detector{LogRoot: logRoot},
		Notifier:     daemon.MacNotify,
		Logf:         logf,
	}
	if jobs {
		r := &runner.Runner{Claude: runner.Claude{Bin: claudeBin}, Home: home}
		r.Logf = func(format string, args ...any) {
			line := fmt.Sprintf(format, args...)
			logf("%s", line)
			d.Notify(rpc.Event{Type: rpc.EventLog, Log: line})
		}
		d.Runner = r.Run
	}
	return d, nil
}
