# gmwd

Use the Claude subscription quota you'd otherwise lose.

Claude plans have a rolling 5-hour limit and a weekly limit, and most weeks end with quota unused. gmwd keeps a backlog of **topics** (things to research, bugs to fix) and works through them with Claude Code while you're not using it. It stays within a share of your weekly limit that you choose, and always leaves part of each 5-hour window for you.

- **research** topics produce a `report.md` with sources.
- **solve** topics produce commits on a `gmwd/<id>` branch in a git worktree. Nothing is pushed.
- Each morning there's a **digest** of what was done and what to review.

It is a background daemon plus a terminal UI and CLI. It runs the official `claude` CLI on your own subscription and never uses API keys.

## Requirements

- macOS (the daemon installs as a launchd agent; the rest is portable Go)
- Go 1.27+
- [Claude Code](https://claude.com/claude-code), logged in with a Pro or Max subscription (`claude` on your `PATH`)
- `git`

## Install

```sh
go build -o ~/bin/gmwd ./cmd/gmwd
gmwd install      # start the daemon now and at every login (launchd)
```

`gmwd install` points launchd at that exact binary. After rebuilding, run `gmwd install` again to reload it. `gmwd uninstall` stops the daemon and removes it, but keeps your topics.

To try it without launchd, run `gmwd daemon` in a terminal instead.

Or download a prebuilt binary (darwin, linux and windows, arm64 and amd64) from [Releases](https://github.com/riceball-tw/gmwd/releases), check it against `SHA256SUMS`, and put `gmwd` on your `PATH`. The binaries aren't signed, so on macOS clear the download quarantine first: `xattr -d com.apple.quarantine gmwd`.

## Quick start

```sh
gmwd status                          # your 5h and weekly usage, as Claude Code reports it

gmwd add --kind research --title "Pure-Go SQLite drivers" \
  --prompt "Compare pure-Go SQLite drivers: maturity, performance, license."

gmwd add --kind solve --title "Fix flaky auth tests" \
  --prompt "TestLogin fails about 1 in 10 runs. Find the cause and fix it." \
  --repo ~/code/myapp --priority 4

gmwd                                 # open the TUI
```

The daemon starts a topic once you've been idle for 10 minutes and the budget allows it. Later:

```sh
gmwd digest                          # what happened today, what to review
git -C ~/code/myapp log --stat main..gmwd/fix-flaky-auth-tests
```

## How it decides when to work

All three must hold:

1. **You're idle.** By default that means no interactive Claude Code session was written to in the last 10 minutes. gmwd's own runs don't count. With `--idle-mode away` it also waits until you haven't touched the keyboard or mouse for 10 minutes.
2. **There's budget.** The **max burn** slider (default 90%) is how full gmwd may fill your *week*. The headroom below it is spread evenly across the 5-hour windows left before the weekly reset. Near the end of a window, gmwd may use more, because that window's quota expires anyway. It never goes into the **reserve** (default 15% of every 5-hour window) and never past the slider.
3. **It isn't paused**, and a topic is queued. The highest priority runs first, then the oldest.

Usage comes straight from Claude Code (`claude -p /usage`, which costs nothing, plus the rate-limit events from gmwd's own runs). gmwd doesn't estimate it. `gmwd status` and the TUI always show the current reason, e.g. `waiting: user active: Claude Code used 2m ago` or `budget 12% of the 5h window until 14:30`.

A job runs in iterations. Each one is a single `claude -p` call that sees notes from the previous ones. The job stops at the end of an iteration when you come back, when you pause gmwd, when its budget is spent, when the topic reports it's done, or after 3 failures in a row. If you reach your reserve mid-iteration, the iteration is stopped right away and its changes are discarded.

## Using it

### TUI

Run `gmwd` with no arguments. Quitting the TUI leaves the daemon running.

| Screen | Keys |
|---|---|
| 1 Dashboard | usage bars, `←/→` max-burn slider, status, current job |
| 2 Topics | `a` add, `e` edit, `d` delete, `p` pause/resume, `+/-` priority, `enter` details |
| 3 Jobs | recent runs and why each stopped, live log |
| 4 Digest | `←/→` previous/next day |

These keys work on every screen: `tab` or `1`–`4` to switch screens, `space` to pause/resume gmwd, `?` for help, `q` to quit.

### CLI

```
gmwd status [--refresh]          usage, resets, daemon state, calibration
gmwd add --kind research|solve --title T --prompt P [--repo DIR]
         [--priority 1-5] [--model sonnet|opus|haiku] [--done-when TEXT]
gmwd ls | rm ID
gmwd pause [ID] | resume [ID]    the whole daemon, or one topic
gmwd run ID [--iterations N] [--max-tokens N]
                                 run a topic now in the foreground (ignores idle)
gmwd digest [--date YYYY-MM-DD] [--list]
gmwd config [--max-burn N] [--reserve N] [--idle-mode not-using-claude|away]
            [--quiet-minutes N] [--notify on|off]
```

`--prompt -` reads the prompt from stdin. `--quiet-minutes 0` makes gmwd run whenever there's budget, without waiting for you to be idle.

Jobs use **Sonnet** by default. Pick Opus per topic with `--model opus`.

## Safety

- **No permission bypass.** Every run uses `--permission-mode dontAsk`, so anything not pre-approved is refused, not prompted. `--dangerously-skip-permissions` is never used.
- **Research runs** can write only to their own directory. They can read that directory and an optional read-only context dir (`--repo`), and they have the web tools.
- **Solve runs** work in a git worktree on `gmwd/<id>`. Bash runs in Claude Code's sandbox: it can write only in the worktree, its temp dir and a per-topic cache, it has no network, and it has no way to switch the sandbox off.
- **gmwd commits; the model doesn't.** gmwd commits after successful iterations and resets the worktree after failed ones. It refuses to touch any branch that isn't `gmwd/*` and never pushes.
- **Your setup stays out of jobs.** They run without your user-level hooks, plugins or MCP servers (a repo's own `.claude/settings.json` still applies), and with a minimal environment, so variables like `GITHUB_TOKEN` aren't passed to them.
- **Kill switch:** `gmwd pause` (or `space` in the TUI) stops new iterations immediately.

Job sessions are saved like any Claude Code session. `gmwd run` prints a `claude --resume <id>` line, so you can open a job and carry on yourself.

## Files

Everything lives in `~/.gmwd/` (override with `GMWD_HOME`):

| Path | What |
|---|---|
| `gmwd.db` | topics, runs, usage readings, settings (SQLite) |
| `gmwd.sock` | the daemon's socket (CLI and TUI talk to it) |
| `daemon.log` | daemon log when run by launchd |
| `topics/<id>/notes.md` | what each iteration did; fed to the next one |
| `topics/<id>/work/report.md` | research output |
| `topics/<id>/runs/<run>/` | raw stream-json of every iteration |
| `worktrees/<id>/` | solve worktrees |
| `digest/YYYY-MM-DD.md` | daily digests |

## Development

```sh
go test ./...            # no quota used: tests drive a fake claude
go test -race ./...
```

CI (`.github/workflows/ci.yml`) runs gofmt, `go vet` and `go test -race` on macOS and Linux, then cross-compiles the binaries. Pushing a `v*` tag also publishes a GitHub Release with the binaries and `SHA256SUMS`; the tag becomes `gmwd version`. Other builds report `dev-<commit>`.

- `docs/spike.md` records what the current Claude Code version actually does: log fields, stream-json events, the `/usage` output, window anchoring, and how the sandbox and permissions behave.
- `plan.md` is the design.

Code layout: `cmd/gmwd` holds the CLI. Under `internal/` are `usage` (reading quota), `pacer` (budget), `activity` (idle check), `scheduler`, `runner` (the `claude -p` loop and git), `daemon`, `rpc`, `store`, `digest`, `calib`, `tui`, `launchd`.

## Limits

- One job at a time.
- Built for macOS. Linux and Windows binaries run the daemon, TUI and CLI, but `gmwd install` (launchd), away-mode idle detection (`ioreg`) and notifications (`osascript`) are macOS-only; start `gmwd daemon` yourself there.
- Toolchain caches are redirected into the sandbox for Go and npm (and anything using `XDG_CACHE_HOME`). Other toolchains may fail to write their caches.
- Claude's limits and Claude Code's output formats can change. If they do, `gmwd status` is the first thing to check.
