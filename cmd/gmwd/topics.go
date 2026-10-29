package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"gmwd/internal/config"
	"gmwd/internal/rpc"
	"gmwd/internal/topic"
)

// errNoDaemon means the socket isn't answering.
var errNoDaemon = errors.New("the gmwd daemon isn't running; start it with `gmwd install` (launchd) or `gmwd daemon`")

func dialDaemon() (*rpc.Client, error) {
	home, err := config.Home()
	if err != nil {
		return nil, err
	}
	sock, err := rpc.SocketPath(home)
	if err != nil {
		return nil, err
	}
	c, err := rpc.Dial(sock)
	if err != nil {
		return nil, errNoDaemon
	}
	return c, nil
}

// call dials the daemon, makes one call and hangs up.
func call(method string, params, result any) error {
	c, err := dialDaemon()
	if err != nil {
		return err
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return c.Call(ctx, method, params, result)
}

func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// topicFlags registers the flags that describe a topic.
type topicFlags struct {
	kind *string
	t    topic.Topic
}

func addTopicFlags(fs *flag.FlagSet) *topicFlags {
	tf := &topicFlags{}
	tf.kind = fs.String("kind", "", "research or solve")
	fs.StringVar(&tf.t.ID, "id", "", "topic id (default: from title)")
	fs.StringVar(&tf.t.Title, "title", "", "short title")
	fs.StringVar(&tf.t.Prompt, "prompt", "", "what to research or fix; - reads stdin")
	fs.StringVar(&tf.t.Repo, "repo", "", "solve: git repo; research: optional read-only context dir")
	fs.StringVar(&tf.t.Model, "model", "", "model (default sonnet)")
	fs.StringVar(&tf.t.DoneWhen, "done-when", "", "natural-language stop condition")
	fs.IntVar(&tf.t.Priority, "priority", 3, "1 (low) to 5 (high)")
	fs.Int64Var(&tf.t.MaxTokens, "topic-max-tokens", 0, "token cap for each run of this topic (0 = none)")
	return tf
}

// topic resolves the flags into a topic: stdin prompt, absolute repo.
func (tf *topicFlags) topic(stdin io.Reader) (topic.Topic, error) {
	t := tf.t
	t.Kind = topic.Kind(*tf.kind)
	if t.Prompt == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return t, err
		}
		t.Prompt = strings.TrimSpace(string(b))
	}
	if t.Title == "" {
		t.Title = firstLine(t.Prompt)
	}
	if t.Repo != "" {
		abs, err := filepath.Abs(t.Repo)
		if err != nil {
			return t, err
		}
		t.Repo = abs
	}
	return t, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

func cmdAdd(args []string, out io.Writer) error {
	fs := newFlags("add")
	tf := addTopicFlags(fs)
	asJSON := fs.Bool("json", false, "print the topic as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("add: unexpected argument %q", fs.Arg(0))
	}
	t, err := tf.topic(os.Stdin)
	if err != nil {
		return err
	}
	var added topic.Topic
	if err := call(rpc.MethodTopicAdd, rpc.TopicParams{Topic: t}, &added); err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(out, added)
	}
	fmt.Fprintf(out, "added %s (%s, priority %d)\n", added.ID, added.Kind, added.Priority)
	return nil
}

func cmdList(args []string, out io.Writer) error {
	fs := newFlags("ls")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var res rpc.TopicsResult
	if err := call(rpc.MethodTopicList, nil, &res); err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(out, res.Topics)
	}
	if len(res.Topics) == 0 {
		fmt.Fprintln(out, "no topics; add one with `gmwd add`")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tPRI\tSTATUS\tMODEL\tTITLE")
	for _, t := range res.Topics {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", t.ID, t.Kind, t.Priority, t.Status, t.ModelOrDefault(), t.Title)
	}
	return tw.Flush()
}

func cmdRemove(args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: gmwd rm ID")
	}
	if err := call(rpc.MethodTopicDelete, rpc.IDParams{ID: args[0]}, nil); err != nil {
		return err
	}
	fmt.Fprintf(out, "removed %s (its notes, reports and gmwd/%s branch are kept)\n", args[0], args[0])
	return nil
}

// cmdPause pauses (or resumes) the whole daemon, or one topic.
func cmdPause(args []string, out io.Writer, pause bool) error {
	verb := map[bool]string{true: "paused", false: "resumed"}[pause]
	switch len(args) {
	case 0:
		var s config.Settings
		if err := call(rpc.MethodSettingsUpdate, rpc.SettingsPatch{Paused: &pause}, &s); err != nil {
			return err
		}
		fmt.Fprintf(out, "gmwd %s\n", verb)
		return nil
	case 1:
		c, err := dialDaemon()
		if err != nil {
			return err
		}
		defer c.Close()
		ctx := context.Background()
		var t topic.Topic
		if err := c.Call(ctx, rpc.MethodTopicGet, rpc.IDParams{ID: args[0]}, &t); err != nil {
			return err
		}
		switch {
		case pause && t.Status == topic.Paused, !pause && t.Status != topic.Paused:
			fmt.Fprintf(out, "%s is %s\n", t.ID, t.Status)
			return nil
		case pause:
			t.Status = topic.Paused
		default:
			t.Status = topic.Queued
		}
		if err := c.Call(ctx, rpc.MethodTopicUpdate, rpc.TopicParams{Topic: t}, &t); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s %s\n", t.ID, verb)
		return nil
	}
	return errors.New("usage: gmwd pause|resume [ID]")
}

// waitForDaemon polls the socket until the daemon answers status.
func waitForDaemon(timeout time.Duration) (rpc.StatusResult, error) {
	var st rpc.StatusResult
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := dialDaemon()
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			lastErr = c.Call(ctx, rpc.MethodStatus, nil, &st)
			cancel()
			c.Close()
			if lastErr == nil {
				return st, nil
			}
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return st, lastErr
}
