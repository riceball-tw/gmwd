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

Claude Code reports quota directly (see `docs/spike.md`), so usage is read, not estimated. Sources, in order of trust:
1. **`rate_limit_event`** from `claude -p --output-format stream-json --verbose`: emitted on every call, not only on a limit hit. `unifiedWindows.{five_hour,seven_day}.{utilization,resetsAt}` gives the used share of each cap (0..1, two decimals) and the exact reset time. Free byproduct of every job iteration.
2. **`claude -p /usage`**: costs no quota (no model call), ~3 s. Integer percent and minute-precision reset times for both windows (reset rounded to the nearest 10 min). The daemon polls this while no job is running.
3. **Claude Code session logs** `~/.claude/projects/**/*.jsonl` (subagent logs included, deduped on `message.id`+`requestId`): token counts per model. Fallback only — they miss claude.ai and other devices. Also used for idle detection (`entrypoint: "cli"` = interactive) and to read logged rate-limit hits.

Usage is tracked as **percent of cap**. Tokens are still counted per job (from `result.usage`) for per-job caps and the digest, and the observed %-per-token ratio is kept to turn a % budget into a token estimate for display. No calibration of caps in tokens is needed.

Window anchoring: a 5h window starts at the first request after the previous one expired, floored to 10 min; the weekly reset is a fixed, hour-aligned anchor. When no 5h window is active, gmwd's first request opens one.

### 4.2 Pacer (`internal/pacer`)
Turns usage + the slider into a **burn budget** right now.

Inputs: utilization (percent of cap) of the 5h window and the week, time to each reset, slider setting, reserve, user activity.

Logic (v0), all in percent of cap:
- **Weekly is the binding constraint.** Target: weekly utilization reaches `slider%` by weekly reset — no more.
- Per 5h window: allowed burn = min(window headroom − reserve, weekly budget spread over windows left until weekly reset).
- **Use-it-or-lose-it boost**: as a window nears reset with unused headroom, raise its allowance (up to the weekly target), since that quota expires anyway.
- **Reserve**: always leave `reserve%` of the current window for the user (default 15%).
- Live check: during a job, every iteration's `rate_limit_event` updates utilization, so the pacer re-checks between iterations against real numbers.
- Output: `{canRun bool, budgetPct float, tokenEstimate int, reason string}` — the reason is shown in the TUI ("waiting: user active", "budget 12% of window until 14:30 reset").

### 4.3 Usage slider (UX)
One number: **max burn %** — the share of the weekly cap gmwd may bring total usage up to (e.g. 90% means "fill my week to 90%, leave the last 10% untouched").
- TUI: ←/→ in 5% steps, shows the projected token amount next to it.
- Optional advanced settings: reserve %, per-job token cap, quiet hours, "only run when idle" toggle.

### 4.4 Idle detection (`internal/activity`)
The user's own work comes first. Two modes (setting `idleMode`):
- **not-using-claude** (default): idle when no interactive Claude Code session (`entrypoint: "cli"` records in `~/.claude/projects/**/*.jsonl`) was written in the last `quietMinutes` (default 10). gmwd's own runs are `sdk-cli` and don't count. An open but quiet `claude` process doesn't count either: sessions are often left open overnight.
- **away**: additionally requires macOS HID idle time (`ioreg -c IOHIDSystem` → `HIDIdleTime`) of at least `quietMinutes`.
- On user activity or `gmwd pause`: the running job finishes its current iteration and stops; the topic stays queued.

### 4.5 Topics (`internal/topic`)
```
Topic {
  id, title, kind: research | solve
  prompt          // what to research / what to fix
  repo            // solve: path to git repo; research: optional context dir
  priority        // 1..5
  model           // sonnet (default) | opus
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
Rewritten to `~/.gmwd/digest/YYYY-MM-DD.md` after every run (scheduled or `gmwd run`), shown by `gmwd digest` and the TUI's Digest screen:
- totals for the day: runs, iterations, tokens, share of 5h windows and of the week;
- **To review**: new commits on a `gmwd/*` branch (with the `git log` command to see them), finished/updated reports, failed topics;
- per topic: how each run stopped, branch stats vs. the default branch, latest note.
Optional macOS notification (`osascript`, setting `notify`, off by default) when a run leaves something to review.

### 4.9 Calibration (`internal/calib`)
Claude Code reports utilization directly, so caps are never estimated in tokens. What is calibrated is how gmwd's measures relate:
- weekly points per 5h point (pacer ratio) from usage readings in the same windows; rate-limited readings are skipped (they may carry only one window);
- tokens per 5h point per model, from finished runs of the last 14 days, used to show what the slider means in tokens.

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
| 6 | Digest + calibration | Daily digest; ratio and tokens-per-point estimates learned from usage and runs (rate-limited readings can't skew them). |
| 7 | (later) Claude Code mod | `/backlog add` + status line segment that read/write through the CLI. |

## 9. Open questions

- ~~Direct source for remaining quota?~~ Yes: `rate_limit_event` and `/usage` (§4.1).
- ~~5h window boundaries?~~ First request after expiry, floored to 10 min (§4.1).
- ~~Default model?~~ Sonnet; Opus selectable per topic.
- Should research topics be able to spawn solve topics (e.g. "research X" → "now implement")? Probably later; keep a human in the loop for v0.
