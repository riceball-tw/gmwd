package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gmwd/internal/topic"
)

// Tool scoping (verified in docs/spike.md §5). Under dontAsk, reads are
// allowed in the working dir and --add-dir only; writes need the ./** rules.
var (
	researchTools   = []string{"Read", "Write", "Edit", "Glob", "Grep", "WebSearch", "WebFetch"}
	researchAllowed = []string{"Write(./**)", "Edit(./**)", "WebSearch", "WebFetch"}

	// Bash is deliberately not in solveAllowed: pre-approving it would also
	// approve unsandboxed runs. The sandbox auto-allows sandboxed commands.
	solveTools    = []string{"Read", "Write", "Edit", "Glob", "Grep", "Bash"}
	solveAllowed  = []string{"Write(./**)", "Edit(./**)"}
	solveSettings = `{"sandbox":{"enabled":true,"autoAllowBashIfSandboxed":true,"allowUnsandboxedCommands":false}}`
)

func (r *Runner) invocation(t topic.Topic, ws workspace, n int, notes string) Invocation {
	inv := Invocation{
		Dir:        ws.dir,
		Prompt:     buildPrompt(t, ws, n, notes),
		Model:      t.ModelOrDefault(),
		SessionID:  newUUID(),
		AddDirs:    ws.addDir,
		JSONSchema: outcomeSchema,
	}
	switch t.Kind {
	case topic.Research:
		inv.Tools, inv.AllowedTools = researchTools, researchAllowed
	case topic.Solve:
		inv.Tools, inv.AllowedTools, inv.Settings = solveTools, solveAllowed, solveSettings
		// Toolchain caches outside the worktree aren't writable in the
		// sandbox; keep them in temp.
		cache := filepath.Join(os.TempDir(), "gmwd", t.ID, "cache")
		inv.ExtraEnv = []string{
			"XDG_CACHE_HOME=" + cache,
			"GOCACHE=" + filepath.Join(cache, "go-build"),
			"npm_config_cache=" + filepath.Join(cache, "npm"),
		}
	}
	return inv
}

func buildPrompt(t topic.Topic, ws workspace, n int, notes string) string {
	var b strings.Builder
	b.WriteString("You are gmwd, working unattended on the user's backlog while they are away. " +
		"Nobody will answer questions: make reasonable decisions and record them in your summary.\n\n")
	fmt.Fprintf(&b, "Topic: %s\n", firstNonEmpty(t.Title, t.ID))
	fmt.Fprintf(&b, "Task:\n%s\n", strings.TrimSpace(t.Prompt))
	if t.DoneWhen != "" {
		fmt.Fprintf(&b, "Done when: %s\n", t.DoneWhen)
	}
	b.WriteString("\n")

	switch t.Kind {
	case topic.Research:
		fmt.Fprintf(&b, "Working directory: %s\n", ws.dir)
		b.WriteString("Keep the deliverable in report.md in the working directory: findings organised by subtopic, " +
			"every claim backed by a source URL, and an \"Open questions\" section at the end. " +
			"Extend and correct the existing report; don't start over. " +
			"You may keep raw notes in other files in this directory. You can't write anywhere else.\n")
		if len(ws.addDir) > 0 {
			fmt.Fprintf(&b, "Read-only context: %s\n", strings.Join(ws.addDir, ", "))
		}
	case topic.Solve:
		fmt.Fprintf(&b, "Working directory: a git worktree at %s on branch %s.\n", ws.dir, ws.branch)
		b.WriteString("Make one focused change toward the task, then run the project's relevant checks " +
			"(build, tests, lint) and fix anything you broke. " +
			"Don't run git commit, push or branch commands: gmwd commits your changes when you report " +
			"progress or done, and discards them when you report failed. " +
			"Bash runs in a sandbox: writes outside the worktree and temp dirs fail, and there is no network.\n")
	}

	b.WriteString("\nNotes from earlier iterations (oldest first):\n")
	if strings.TrimSpace(notes) == "" {
		b.WriteString("(none — this is the first iteration)\n")
	} else {
		b.WriteString(notes)
		if !strings.HasSuffix(notes, "\n") {
			b.WriteString("\n")
		}
	}

	fmt.Fprintf(&b, "\nThis is iteration %d. Make one solid step of progress, then stop.\n", n)
	b.WriteString("End with the structured result:\n" +
		"- status: \"done\" if the task is fully complete (including any done-when condition), " +
		"\"progress\" if you made progress and more remains, \"failed\" if you could not make progress " +
		"or left things broken.\n" +
		"- summary: 2-5 sentences on what you did, what you found or changed, and what the next iteration should do.\n")
	return b.String()
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
