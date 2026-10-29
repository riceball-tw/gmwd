package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"gmwd/internal/config"
	"gmwd/internal/runner"
	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage:
  gmwd run --kind research|solve --title T --prompt P [--repo DIR] [options]   new topic
  gmwd run [options] ID                                                        continue a topic

Options:
`)
		fs.PrintDefaults()
	}
	var t topic.Topic
	kind := fs.String("kind", "", "research or solve")
	fs.StringVar(&t.ID, "id", "", "topic id (default: from title)")
	fs.StringVar(&t.Title, "title", "", "short title")
	fs.StringVar(&t.Prompt, "prompt", "", "what to research or fix")
	fs.StringVar(&t.Repo, "repo", "", "solve: git repo; research: optional read-only context dir")
	fs.StringVar(&t.Model, "model", "", "model (default sonnet)")
	fs.StringVar(&t.DoneWhen, "done-when", "", "natural-language stop condition")
	fs.Int64Var(&t.MaxTokens, "topic-max-tokens", 0, "token cap stored on the topic")
	var b runner.Budget
	fs.IntVar(&b.Iterations, "iterations", runner.DefaultIterations, "max iterations this run")
	fs.Int64Var(&b.Tokens, "max-tokens", 0, "max input+output+cache-write tokens this run (0 = no cap)")
	windowPct := fs.Float64("window-pct", 10, "max percentage points of the 5h window this run may use (0 = no cap)")
	reservePct := fs.Float64("reserve", 15, "percent of the 5h window always left for you")
	asJSON := fs.Bool("json", false, "print the summary as JSON")
	fs.Parse(args)
	b.WindowPct, b.ReservePct = *windowPct/100, *reservePct/100

	home, err := config.Home()
	if err != nil {
		return err
	}
	switch {
	case fs.NArg() == 1 && *kind == "":
		if t, err = loadTopic(home, fs.Arg(0)); err != nil {
			return err
		}
	case fs.NArg() == 0 && *kind != "":
		t.Kind = topic.Kind(*kind)
		if t.ID == "" {
			t.ID = topic.Slug(firstNonEmpty(t.Title, t.Prompt))
		}
		if t.Repo != "" {
			if t.Repo, err = filepath.Abs(t.Repo); err != nil {
				return err
			}
		}
		t.Status, t.CreatedAt = topic.Queued, time.Now()
		if err := t.Validate(); err != nil {
			return err
		}
		if old, err := loadTopic(home, t.ID); err == nil {
			return fmt.Errorf("topic %q already exists (%s); continue it with `gmwd run %s` or pick another --id", t.ID, old.Kind, t.ID)
		}
		if err := saveTopic(home, t); err != nil {
			return err
		}
	default:
		fs.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	}
	var opts runner.Options
	opts.Budget = b
	pctx, cancel := context.WithTimeout(ctx, time.Minute)
	snap, perr := usage.Prober{Dir: filepath.Join(home, "probe")}.Probe(pctx)
	cancel()
	if perr != nil {
		logf("warning: couldn't read usage (%v); window budget starts from the first call", perr)
	} else {
		opts.Start = &snap
		logf("usage before run: 5h %.0f%%, week %.0f%%", snap.FiveHour.Utilization*100, snap.SevenDay.Utilization*100)
	}

	t.Status = topic.Running
	if err := saveTopic(home, t); err != nil {
		return err
	}
	logf("running %s topic %q (Ctrl-C stops after cleaning up)", t.Kind, t.ID)
	r := &runner.Runner{Home: home, Logf: logf}
	s, runErr := r.Run(ctx, t, opts)
	t.Status = s.TopicStatus
	if t.Status == "" {
		t.Status = topic.Queued
	}
	if err := saveTopic(home, t); err != nil {
		return errors.Join(runErr, err)
	}
	if runErr != nil {
		return runErr
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	printRunSummary(s)
	return nil
}

func printRunSummary(s runner.Summary) {
	fmt.Printf("\nTopic %s: %s (stopped: %s)\n", s.TopicID, s.TopicStatus, s.StopReason)
	fmt.Printf("  iterations  %d (%d ok, %d failed)\n", s.Iterations, s.Succeeded, s.Failed)
	fmt.Printf("  tokens      %s in+out+cache-write, %s cache read, ~$%.2f at list price\n",
		tokens(s.Tokens.Input+s.Tokens.Output+s.Tokens.CacheCreate), tokens(s.Tokens.CacheRead), s.CostUSD)
	fmt.Printf("  5h window   +%.0f points\n", s.WindowSpent*100)
	if s.Report != "" {
		fmt.Printf("  report      %s\n", s.Report)
	}
	if s.Branch != "" {
		fmt.Printf("  branch      %s (%d new commits) in %s\n", s.Branch, len(s.Commits), s.WorkDir)
	}
	if !s.ResetsAt.IsZero() {
		fmt.Printf("  rate limited; resets %s\n", s.ResetsAt.Local().Format("Mon 15:04"))
	}
	fmt.Printf("  logs        %s\n", s.RunDir)
	if len(s.Sessions) > 0 {
		fmt.Printf("  open in Claude Code: cd %s && claude --resume %s\n", s.WorkDir, s.Sessions[len(s.Sessions)-1])
	}
}

func topicPath(home, id string) string { return filepath.Join(home, "topics", id, "topic.json") }

func loadTopic(home, id string) (topic.Topic, error) {
	var t topic.Topic
	data, err := os.ReadFile(topicPath(home, id))
	if errors.Is(err, os.ErrNotExist) {
		return t, fmt.Errorf("no topic %q", id)
	}
	if err != nil {
		return t, err
	}
	return t, json.Unmarshal(data, &t)
}

func saveTopic(home string, t topic.Topic) error {
	p := topicPath(home, t.ID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
