# gmwd — Plan

Use leftover Claude subscription quota before it resets, without getting in the way of your own work.

## 1. Problem

Claude subscriptions have a rolling **5-hour window** and a **weekly cap**. Most weeks end with quota unused; it expires at reset and is gone. gmwd is a background daemon plus a Bubble Tea TUI that:

- holds a backlog of **topics** (things to research or problems to solve),
- watches subscription usage and reset times,
- spends leftover quota on those topics while the user is idle, up to a user-set limit,
- never blocks the user's own interactive Claude Code work.

## 2. Goals / Non-goals

**Goals**
- Set-and-forget: the daemon runs under launchd, survives reboots and TUI closing.
- A **usage slider** controls the maximum burn (how much of the leftover quota gmwd may use).
- Two topic types: **research** (produces a report) and **solve** (produces a git branch).
- Safe by default: worktrees, scoped tools, never push to main, a reserve kept for the user.
- Morning digest: what was done, tokens spent, what to review.

**Non-goals (v0)**
- Multi-account, team or shared usage (against ToS; never).
- Using API keys / pay-per-token billing (subscription only).
- Agents other than Claude Code (maybe later).
- Windows/Linux service install (macOS launchd first; code stays portable).

## 3. Architecture

```
┌─────────────┐   unix socket (JSON-RPC)  ┌──────────────────────────────┐
│  gmwd (TUI) │ ◄───────────────────────► │        gmwd daemon           │
│ Bubble Tea  │                           │                              │
└─────────────┘                           │  usage   → pacer → scheduler │
                                          │     ▲                 │      │
┌─────────────┐                           │     │                 ▼      │
│ gmwd CLI    │ ◄── same socket ───────►  │  ~/.claude/      runner      │
│ add/ls/...  │                           │  projects/*.jsonl  (claude -p│
└─────────────┘                           │                 in worktree) │
                                          │  store: ~/.gmwd/ (SQLite)    │
                                          └──────────────────────────────┘
```

- **Daemon** (`gmwd daemon`): long-lived, headless, owns all state and processes. Installed via `gmwd install` → `~/Library/LaunchAgents/com.gmwd.daemon.plist`.
- **TUI** (`gmwd`): thin client. Connects to the socket, renders state, sends commands. Quitting it does not affect jobs.
- **CLI** (`gmwd add`, `gmwd ls`, `gmwd pause`, …): scriptable, same socket. Lets a future Claude Code mod (`/backlog add`) work by shelling out.
- **Socket**: `~/.gmwd/gmwd.sock`, newline-delimited JSON-RPC. Server-push events for live TUI updates.
- **Store**: `~/.gmwd/gmwd.db` (SQLite via `modernc.org/sqlite`, pure Go — no cgo). Topics, jobs, usage samples, calibration data.

## 4. Core components

### 4.1 Usage sensor (`internal/usage`)
Answers: *how much has been used in the current 5h window and this week, and when does each reset?*

Sources, in order of trust:
1. **Rate-limit events** from `claude -p --output-format stream-json` (gnhf reads reset times from these). Gives exact reset time when a limit is hit.
2. **Claude Code session logs** `~/.claude/projects/**/*.jsonl`: per-message token usage (input, output, cache read/write) with timestamps and model. Sum per window — ccusage does this; reuse the approach. Watched with fsnotify; includes the user's own sessions, which is what we want.
3. **Calibration**: limits aren't published in tokens. Each time a limit is hit, record the window's weighted usage → estimate the cap. Until calibrated, fall back to a configurable plan default (Pro / Max 5x / Max 20x) and be conservative.

Token weighting: models count differently against limits (Opus > Sonnet; cache reads are cheap). Keep a configurable weight table; refine via calibration.

> **To verify early (spike):** exact fields in the JSONL logs and stream-json rate-limit events in the current Claude Code version; whether Claude Code exposes remaining-quota data anywhere directly (status line input, `/usage`). If a direct source exists, prefer it over estimation.

### 4.2 Pacer (`internal/pacer`)
Turns usage + the slider into a **burn budget** right now.

Inputs: estimated used/remaining for the 5h window and the week, time to each reset, slider setting, reserve, user activity.

Logic (v0):
- **Weekly is the binding constraint.** Target: weekly usage reaches `slider%` of the cap by weekly reset — no more.
- Per 5h window: allowed burn = min(remaining in window − reserve, weekly budget spread over windows left until weekly reset).
- **Use-it-or-lose-it boost**: as a window nears reset with unused headroom, raise its allowance (up to the weekly target), since that quota expires anyway.
- **Reserve**: always leave `reserve%` of the current window for the user (default 15%).
- Output: `{canRun bool, tokenBudget int, reason string}` — the reason is shown in the TUI ("waiting: user active", "budget 120k until 14:30 reset").

### 4.3 Usage slider (UX)
One number: **max burn %** — the share of the weekly cap gmwd may bring total usage up to (e.g. 90% means "fill my week to 90%, leave the last 10% untouched").
- TUI: ←/→ in 5% steps, shows the projected token amount next to it.
- Optional advanced settings: reserve %, per-job token cap, quiet hours, "only run when idle" toggle.

### 4.4 Idle detection (`internal/activity`)
The user's own work comes first.
- Active if: an interactive `claude` process exists that isn't one of ours (track our PIDs), or user-session JSONL files were written in the last N minutes (default 10).
- Optional: macOS HID idle time (`ioreg -c IOHIDSystem` → `HIDIdleTime`) for "only when away from keyboard".
- On user activity: the running job finishes its current iteration and then pauses (configurable: or kill immediately).

### 4.5 Topics (`internal/topic`)
```
Topic {
  id, title, kind: research | solve
  prompt          // what to research / what to fix
  repo            // solve: path to git repo; research: optional context dir
  priority        // 1..5
  maxTokens       // per-topic cap (optional)
  doneWhen        // optional natural-language stop condition
  status          // queued | running | paused | done | failed
}
```
- **Research**: runs in a scratch dir `~/.gmwd/research/<id>/`, with WebSearch/WebFetch allowed. Output: `report.md` with sources. Can resume and extend over several runs (notes.md carried between iterations, like gnhf).
- **Solve**: creates a git worktree on branch `gmwd/<id>-<slug>` from the repo's default branch. Iterates: make a change → run checks → commit. Never pushes unless the topic opts in, and never to the default branch.

### 4.6 Runner (`internal/runner`)
- Spawns `claude -p "<prompt>" --output-format stream-json --allowedTools <scoped list> [--model ...]` in the job dir.
- Parses the stream: token usage (feeds the budget), rate-limit events (feeds usage sensor), final result.
- Iteration loop (gnhf-style): build prompt from topic + notes.md → run → commit on success / `git reset --hard` on failure → append notes. Stop on: budget exhausted, `doneWhen` met, max consecutive failures (3), pacer says stop, user activity.
- Stops at an iteration boundary once the budget is spent, so the overshoot is at most one iteration.
- Concurrency: 1 job at a time in v0 (keeps burn predictable).

### 4.7 Scheduler (`internal/scheduler`)
Ticks every 60s (and on usage/activity events):
1. Ask the pacer → can run? budget?
2. Pick the highest-priority queued/resumable topic.
3. Hand it to the runner with the budget.

### 4.8 Digest (`internal/digest`)
Written to `~/.gmwd/digest/YYYY-MM-DD.md` and shown in the TUI:
- per topic: what was done, tokens spent, branch / report link, check status;
- total burned vs. quota that would have expired unused.
Optional macOS notification (`osascript`) when a digest is ready.

## 5. TUI (Bubble Tea)

Libraries: `bubbletea`, `bubbles` (table, list, progress, viewport, textinput), `lipgloss`, `huh` (forms).

Screens (tab to switch):
1. **Dashboard**
   - 5h window bar: used / reserve / gmwd budget, reset countdown.
   - Weekly bar: used / slider target line, reset countdown.
   - **Slider**: `Max burn [■■■■■■■■■□] 90%  (~1.2M tokens)`.
   - Status line: pacer reason ("idle — running 'flaky auth tests' 42k/120k").
2. **Topics**: table; `a` add (huh form), `e` edit, `d` delete, `p` pause, `+/-` priority, `enter` details.
3. **Jobs**: running/history; `enter` → live log viewport (streamed from the daemon).
4. **Digest**: list of finished work; open a report, show branch diff stat.

Global keys: `space` pause/resume the daemon, `?` help, `q` quit the TUI (daemon keeps running).

## 6. Layout

```
gmwd/
  cmd/gmwd/main.go          // subcommands: (tui default), daemon, add, ls, pause, resume, install, uninstall, status
  internal/
    config/                 // ~/.gmwd/config.yml
    store/                  // SQLite, migrations
    usage/                  // JSONL parser, rate-limit events, calibration
    pacer/
    activity/
    topic/
    runner/                 // claude -p, stream-json parsing, git worktree ops
    scheduler/
    digest/
    rpc/                    // socket server + client, shared types
    tui/                    // bubble tea models, one per screen
  plan.md
```

## 7. Safety

- Solve jobs run only in worktrees on `gmwd/*` branches; no push by default; never force-push.
- Scoped `--allowedTools` per kind (research: read + web + write in its own dir; solve: edit + bash in the worktree). No `--dangerously-skip-permissions`.
- No secrets in prompts; env is scrubbed to a minimal allowlist for child processes.
- Hard stops: per-job token cap, per-day cap, 3 consecutive failures → topic marked failed.
- Kill switch: `gmwd pause` / `space` in the TUI stops new iterations immediately.
- ToS: own account, own work, subscription via the official Claude Code CLI only. No account sharing, no proxying quota to other apps. Limits are configurable, not hard-coded, since Anthropic may change them.

## 8. Milestones

| # | Milestone | Done when |
|---|-----------|-----------|
| 0 | **Spike** | Confirmed JSONL fields, stream-json usage/rate-limit event format, any direct quota source. Notes written in `docs/spike.md`. |
| 1 | Usage sensor + `gmwd status` | CLI prints current 5h/weekly usage and reset times that match what Claude Code reports. |
| 2 | Runner (foreground) | `gmwd run <topic>` runs one research and one solve topic end-to-end with a token budget. |
| 3 | Daemon + store + socket | Daemon runs, CLI `add/ls/pause` work over the socket, launchd install works. |
| 4 | Pacer + activity + scheduler | Daemon auto-runs topics only when idle and within budget; logs pacer reasons. |
| 5 | TUI | Dashboard with slider, topics CRUD, live job log. |
| 6 | Digest + calibration | Daily digest; cap estimates improve after limit hits. |
| 7 | (later) Claude Code mod | `/backlog add` + status line segment that read/write through the CLI. |

## 9. Open questions

- Is there a reliable direct source for remaining quota, or is estimation from logs the only option? (Spike.)
- How do the 5h window boundaries work exactly — from the first message or fixed? Affects reset prediction.
- Default model for gmwd jobs: Sonnet (cheaper against limits, more iterations) vs. Opus (better quality)? Probably configurable per topic, default Sonnet.
- Should research topics be able to spawn solve topics (e.g. "research X" → "now implement")? Probably later; keep a human in the loop for v0.
