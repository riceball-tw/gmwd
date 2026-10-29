package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"gmwd/internal/config"
	"gmwd/internal/rpc"
	"gmwd/internal/runner"
	"gmwd/internal/store"
	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

// claudeBin lets tests substitute a fake claude.
var claudeBin = os.Getenv("GMWD_CLAUDE")

func cmdRun(args []string, out io.Writer) error {
	fs := newFlags("run")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage:
  gmwd run [options] ID                                                        run a queued topic now
  gmwd run --kind research|solve --title T --prompt P [--repo DIR] [options]   add a topic and run it

Options:
`)
		fs.PrintDefaults()
	}
	tf := addTopicFlags(fs)
	var b runner.Budget
	fs.IntVar(&b.Iterations, "iterations", runner.DefaultIterations, "max iterations this run")
	fs.Int64Var(&b.Tokens, "max-tokens", 0, "max input+output+cache-write tokens this run (0 = no cap)")
	windowPct := fs.Float64("window-pct", 10, "max percentage points of the 5h window this run may use (0 = no cap)")
	reservePct := fs.Float64("reserve", -1, "percent of the 5h window always left for you (default: the daemon setting)")
	asJSON := fs.Bool("json", false, "print the summary as JSON")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	home, err := config.Home()
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(home, "gmwd.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var t topic.Topic
	switch {
	case len(pos) == 1 && *tf.kind == "":
		if t, err = st.GetTopic(ctx, pos[0]); err != nil {
			return err
		}
	case len(pos) == 0 && *tf.kind != "":
		nt, err := tf.topic(os.Stdin)
		if err != nil {
			return err
		}
		if t, err = addTopic(ctx, st, nt); err != nil {
			return err
		}
	default:
		fs.Usage()
		return errors.New("run: give a topic ID or --kind/--title/--prompt")
	}

	settings, err := st.Settings(ctx)
	if err != nil {
		return err
	}
	b.WindowPct = *windowPct / 100
	b.ReservePct = settings.ReservePct / 100
	if *reservePct >= 0 {
		b.ReservePct = *reservePct / 100
	}

	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	}
	if settings.Paused {
		logf("note: gmwd is paused; running %s anyway because you asked for it", t.ID)
	}
	if err := st.ClaimTopic(ctx, t.ID); err != nil {
		return err
	}
	// From here on the topic is ours: always release it.
	final := t.Status
	if final == topic.Running {
		final = topic.Queued
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := st.SetTopicStatus(rctx, t.ID, final); err != nil {
			logf("warning: couldn't set %s back to %s: %v", t.ID, final, err)
		}
	}()

	opts := runner.Options{Budget: b}
	if snap, err := currentUsage(ctx, home); err != nil {
		logf("warning: couldn't read usage (%v); window budget starts from the first call", err)
	} else {
		opts.Start = &snap
		logf("usage before run: 5h %.0f%%, week %.0f%%", snap.FiveHour.Utilization*100, snap.SevenDay.Utilization*100)
	}

	startedAt := time.Now()
	runRow, err := st.StartRun(ctx, store.Run{TopicID: t.ID, StartedAt: startedAt})
	if err != nil {
		return err
	}
	logf("running %s topic %q (Ctrl-C stops after cleaning up)", t.Kind, t.ID)
	r := &runner.Runner{Claude: runner.Claude{Bin: claudeBin}, Home: home, Logf: logf}
	s, runErr := r.Run(ctx, t, opts)
	if s.TopicStatus != "" {
		final = s.TopicStatus
	}
	rec := store.Run{ID: runRow, TopicID: t.ID, RunID: s.RunID, RunDir: s.RunDir, EndedAt: time.Now(),
		StopReason: string(s.StopReason), Iterations: s.Iterations, Succeeded: s.Succeeded, Failed: s.Failed,
		Tokens: s.Tokens, CostUSD: s.CostUSD, WindowSpent: s.WindowSpent, Commits: s.Commits}
	if runErr != nil && rec.StopReason == "" {
		rec.StopReason = "error: " + runErr.Error()
	}
	// Record the run even if ctx was cancelled.
	fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.FinishRun(fctx, rec); err != nil {
		logf("warning: couldn't record the run: %v", err)
	}
	if runErr != nil {
		return runErr
	}
	if *asJSON {
		return writeJSON(out, s)
	}
	printRunSummary(out, s)
	return nil
}

// addTopic adds through the daemon when it runs, so its clients hear about
// it, and straight to the store otherwise.
func addTopic(ctx context.Context, st *store.Store, t topic.Topic) (topic.Topic, error) {
	var added topic.Topic
	err := call(rpc.MethodTopicAdd, rpc.TopicParams{Topic: t}, &added)
	if !errors.Is(err, errNoDaemon) {
		return added, err
	}
	if t.ID == "" {
		t.ID = topic.Slug(t.Title)
	}
	if t.Priority == 0 {
		t.Priority = 3
	}
	return st.AddTopic(ctx, t)
}

// currentUsage asks the daemon (which records the sample) or probes directly.
func currentUsage(ctx context.Context, home string) (usage.Snapshot, error) {
	var snap usage.Snapshot
	err := call(rpc.MethodUsageRefresh, nil, &snap)
	if !errors.Is(err, errNoDaemon) {
		return snap, err
	}
	pctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return usage.Prober{Claude: claudeBin, Dir: filepath.Join(home, "probe")}.Probe(pctx)
}

func printRunSummary(out io.Writer, s runner.Summary) {
	fmt.Fprintf(out, "\nTopic %s: %s (stopped: %s)\n", s.TopicID, s.TopicStatus, s.StopReason)
	fmt.Fprintf(out, "  iterations  %d (%d ok, %d failed)\n", s.Iterations, s.Succeeded, s.Failed)
	fmt.Fprintf(out, "  tokens      %s in+out+cache-write, %s cache read, ~$%.2f at list price\n",
		tokens(s.Tokens.Input+s.Tokens.Output+s.Tokens.CacheCreate), tokens(s.Tokens.CacheRead), s.CostUSD)
	fmt.Fprintf(out, "  5h window   +%.0f points\n", s.WindowSpent*100)
	if s.Report != "" {
		fmt.Fprintf(out, "  report      %s\n", s.Report)
	}
	if s.Branch != "" {
		fmt.Fprintf(out, "  branch      %s (%d new commits) in %s\n", s.Branch, len(s.Commits), s.WorkDir)
	}
	if !s.ResetsAt.IsZero() {
		fmt.Fprintf(out, "  rate limited; resets %s\n", s.ResetsAt.Local().Format("Mon 15:04"))
	}
	fmt.Fprintf(out, "  logs        %s\n", s.RunDir)
	if len(s.Sessions) > 0 {
		fmt.Fprintf(out, "  open in Claude Code: cd %s && claude --resume %s\n", s.WorkDir, s.Sessions[len(s.Sessions)-1])
	}
}
