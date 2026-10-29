// Package digest summarises a day of gmwd work: what each topic did, what
// it cost, and what the user should look at.
package digest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gmwd/internal/runner"
	"gmwd/internal/store"
	"gmwd/internal/topic"
	"gmwd/internal/usage"
)

// Topic is one topic's part of the day.
type Topic struct {
	ID          string             `json:"id"`
	Title       string             `json:"title"`
	Kind        topic.Kind         `json:"kind"`
	Status      topic.Status       `json:"status"`
	Repo        string             `json:"repo,omitempty"`
	Runs        int                `json:"runs"`
	Iterations  int                `json:"iterations"`
	Tokens      usage.TokenCounts  `json:"tokens"`
	CostUSD     float64            `json:"costUSD"`
	WindowSpent float64            `json:"windowSpent"`
	Stops       []string           `json:"stops"`
	NewCommits  int                `json:"newCommits"`
	Branch      *runner.BranchInfo `json:"branch,omitempty"`
	Report      string             `json:"report,omitempty"` // path, if one exists
	LastNote    string             `json:"lastNote,omitempty"`
}

// Digest is one day.
type Digest struct {
	Day         time.Time         `json:"day"` // local midnight
	Topics      []Topic           `json:"topics"`
	Runs        int               `json:"runs"`
	Iterations  int               `json:"iterations"`
	Tokens      usage.TokenCounts `json:"tokens"`
	CostUSD     float64           `json:"costUSD"`
	WindowSpent float64           `json:"windowSpent"`
	// Ratio is week points per 5h point, to express spend as share of the week.
	Ratio float64 `json:"ratio"`
}

// DayStart is local midnight of t's day.
func DayStart(t time.Time) time.Time {
	y, m, d := t.Local().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

// Build collects the runs that started on day's date.
func Build(ctx context.Context, st *store.Store, home string, day time.Time, ratio float64) (Digest, error) {
	from := DayStart(day)
	to := from.AddDate(0, 0, 1)
	dg := Digest{Day: from, Ratio: ratio}
	runs, err := st.ListRuns(ctx, "", 5000)
	if err != nil {
		return dg, err
	}
	paths := runner.Paths{Home: home}
	byID := map[string]*Topic{}
	var order []string
	// Oldest first, so stop reasons read in order.
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.Before(runs[j].StartedAt) })
	for _, r := range runs {
		if r.StartedAt.Before(from) || !r.StartedAt.Before(to) {
			continue
		}
		t := byID[r.TopicID]
		if t == nil {
			t = &Topic{ID: r.TopicID}
			if tp, err := st.GetTopic(ctx, r.TopicID); err == nil {
				t.Title, t.Kind, t.Status, t.Repo = tp.Title, tp.Kind, tp.Status, tp.Repo
			}
			byID[r.TopicID] = t
			order = append(order, r.TopicID)
		}
		t.Runs++
		t.Iterations += r.Iterations
		addTokens(&t.Tokens, r.Tokens)
		t.CostUSD += r.CostUSD
		t.WindowSpent += r.WindowSpent
		t.NewCommits += len(r.Commits)
		stop := r.StopReason
		if stop == "" {
			stop = "running"
		}
		t.Stops = append(t.Stops, stop)

		dg.Runs++
		dg.Iterations += r.Iterations
		addTokens(&dg.Tokens, r.Tokens)
		dg.CostUSD += r.CostUSD
		dg.WindowSpent += r.WindowSpent
	}
	for _, id := range order {
		t := byID[id]
		if t.Kind == topic.Solve && t.Repo != "" {
			if info, err := runner.BranchStat(ctx, t.Repo, "gmwd/"+id); err == nil && info.Exists {
				t.Branch = &info
			}
		}
		if _, err := os.Stat(paths.Report(id)); err == nil {
			t.Report = paths.Report(id)
		}
		t.LastNote, _ = paths.LastNote(id)
		dg.Topics = append(dg.Topics, *t)
	}
	return dg, nil
}

func addTokens(dst *usage.TokenCounts, c usage.TokenCounts) {
	dst.Input += c.Input
	dst.Output += c.Output
	dst.CacheCreate += c.CacheCreate
	dst.CacheRead += c.CacheRead
	dst.Requests += c.Requests
}

// needsReview says why the user should look at a topic, or "".
func (t Topic) needsReview() string {
	switch {
	case t.Status == topic.Failed:
		return "failed — needs attention"
	case t.NewCommits > 0 && t.Branch != nil:
		s := fmt.Sprintf("%d new commit%s on `%s`", t.NewCommits, plural(t.NewCommits), t.Branch.Branch)
		if t.Branch.ShortStat != "" {
			s += " (" + t.Branch.ShortStat + ")"
		}
		return s
	case t.Kind == topic.Research && t.Report != "":
		if t.Status == topic.Done {
			return "report finished"
		}
		return "report updated"
	}
	return ""
}

// Markdown renders the digest.
func (d Digest) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# gmwd digest — %s\n\n", d.Day.Format("Mon 2 Jan 2006"))
	if d.Runs == 0 {
		b.WriteString("No gmwd runs on this day.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "gmwd ran %d job%s on %d topic%s: %d iteration%s, %s tokens (+%s cache reads), %s of 5h windows",
		d.Runs, plural(d.Runs), len(d.Topics), plural(len(d.Topics)), d.Iterations, plural(d.Iterations),
		tokens(jobTokens(d.Tokens)), tokens(d.Tokens.CacheRead), pct(d.WindowSpent))
	if d.Ratio > 0 {
		fmt.Fprintf(&b, " (≈%s of the week)", pct(d.WindowSpent*d.Ratio))
	}
	b.WriteString(".\n\n")

	var review []string
	for _, t := range d.Topics {
		if why := t.needsReview(); why != "" {
			line := fmt.Sprintf("- **%s** (%s) — %s", t.title(), t.Kind, why)
			switch {
			case t.Branch != nil && t.NewCommits > 0:
				line += fmt.Sprintf("  \n  `git -C %s log --stat %s..%s`", t.Repo, t.Branch.Base, t.Branch.Branch)
			case t.Report != "":
				line += "  \n  " + t.Report
			}
			review = append(review, line)
		}
	}
	b.WriteString("## To review\n\n")
	if len(review) == 0 {
		b.WriteString("Nothing new to review.\n")
	} else {
		b.WriteString(strings.Join(review, "\n") + "\n")
	}

	b.WriteString("\n## Topics\n")
	for _, t := range d.Topics {
		fmt.Fprintf(&b, "\n### %s · `%s` · %s\n\n", t.title(), t.ID, t.Status)
		fmt.Fprintf(&b, "- %d run%s, %d iteration%s, %s tokens, %s of 5h windows\n",
			t.Runs, plural(t.Runs), t.Iterations, plural(t.Iterations), tokens(jobTokens(t.Tokens)), pct(t.WindowSpent))
		fmt.Fprintf(&b, "- stopped: %s\n", strings.Join(t.Stops, ", "))
		if br := t.Branch; br != nil {
			fmt.Fprintf(&b, "- branch `%s`: %d commit%s ahead of %s", br.Branch, br.Ahead, plural(br.Ahead), br.Base)
			if br.ShortStat != "" {
				fmt.Fprintf(&b, " (%s)", br.ShortStat)
			}
			b.WriteString("\n")
		}
		if t.Report != "" {
			fmt.Fprintf(&b, "- report: %s\n", t.Report)
		}
		if t.LastNote != "" {
			b.WriteString("- latest note:\n\n")
			for _, l := range strings.Split(t.LastNote, "\n") {
				b.WriteString("  > " + l + "\n")
			}
		}
	}
	return b.String()
}

func (t Topic) title() string {
	if t.Title != "" {
		return t.Title
	}
	return t.ID
}

// Dir is where digests are written.
func Dir(home string) string { return filepath.Join(home, "digest") }

// Path is the file for a day's digest.
func Path(home string, day time.Time) string {
	return filepath.Join(Dir(home), DayStart(day).Format("2006-01-02")+".md")
}

// Write saves the digest and returns its path.
func Write(home string, d Digest) (string, error) {
	if err := os.MkdirAll(Dir(home), 0o700); err != nil {
		return "", err
	}
	p := Path(home, d.Day)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(d.Markdown()), 0o600); err != nil {
		return "", err
	}
	return p, os.Rename(tmp, p)
}

// Days lists the dates that have a digest, newest first.
func Days(home string) ([]string, error) {
	entries, err := os.ReadDir(Dir(home))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var days []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".md") {
			if _, err := time.Parse("2006-01-02", strings.TrimSuffix(name, ".md")); err == nil {
				days = append(days, strings.TrimSuffix(name, ".md"))
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	return days, nil
}

func jobTokens(c usage.TokenCounts) int64 { return c.Input + c.Output + c.CacheCreate }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
