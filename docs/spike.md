# Milestone 0 — Spike notes

Checked 2026-10-29 against **Claude Code 2.1.308** (macOS, subscription/OAuth, `apiKeySource: "none"`).
Only keys, usage objects and Claude Code's own error/metadata fields were inspected — no message content.

## TL;DR

1. **There is a direct quota source.** Every `claude -p --output-format stream-json --verbose` call emits a
   `rate_limit_event` with utilization (0..1) and reset time for both the 5h and 7-day windows.
2. **`claude -p "/usage"` is free** (`total_cost_usd: 0`, no model call, no `rate_limit_event`) and prints the
   same numbers as text (integer percent).
3. Usage from the session logs is therefore a **fallback**, not the primary sensor. The logs also miss usage
   from claude.ai web/desktop and other machines, which share the same limits.
4. 5h window: starts at the first request after the previous window expired, **floored to 10 minutes**.
   Weekly window: fixed, hour-aligned anchor.

Consequences for the plan:
- §4.1 source order becomes: (a) `rate_limit_event` from our own runs, (b) `/usage` poll (free), (c) log
  estimation. Calibration (§4.1.3) is mostly unnecessary — utilization is already a fraction of the cap.
- The pacer can work in **percent of cap** rather than tokens. Token budgets are still useful per job
  (per-iteration cost), measured from `result.usage` and converted via observed %/token.

## 1. Session logs `~/.claude/projects/<proj>/<session>.jsonl`

Layout:
- `~/.claude/projects/<cwd-with-slashes-as-dashes>/<sessionId>.jsonl` — main sessions.
- `~/.claude/projects/<proj>/<sessionId>/subagents/agent-<id>.jsonl` — subagent sessions. **Must be included.**

Record `type`s seen: `assistant user attachment system api-request api-request-blob api-request-shape
last-prompt mode atis-latch ai-title permission-mode file-history-snapshot file-history-delta queue-operation
cost-state bridge-session agent-name isolation-latch fork-context-ref continued-in`. Only `assistant` carries usage.

`assistant` record (relevant fields):
```
timestamp        RFC3339 UTC, ms ("2026-10-29T13:33:29.272Z")
version          Claude Code version
entrypoint       "cli" = interactive, "sdk-cli" = claude -p   ← use for activity detection
isSidechain      bool
sessionId, requestId
message.id       "msg_..."
message.model    "claude-opus-5-5" | "claude-sonnet-5-5" | "claude-haiku-5-5" | "<synthetic>"
message.usage {
  input_tokens, output_tokens,
  cache_creation_input_tokens, cache_read_input_tokens,
  cache_creation { ephemeral_5m_input_tokens, ephemeral_1h_input_tokens },
  output_tokens_details { thinking_tokens },
  server_tool_use { web_search_requests, web_fetch_requests },
  service_tier, speed, inference_geo, iterations[], fallback_credit
}
```

**Duplicates:** one `assistant` record is written per content block, each repeating the *same* usage.
Dedupe on `(message.id, requestId)` before summing.

**Rate-limit hits** are logged as synthetic assistant records:
```
message.model = "<synthetic>", isApiErrorMessage = true, error = "rate_limit", apiErrorStatus = 429
quotaLimits = { status:"rejected", resetsAt:<unix s>, rateLimitType:"five_hour",
                overageStatus, overageDisabledReason, isUsingOverage, unifiedRateLimitFallbackAvailable }
apiErrorParams.rate_limit_info = same shape (sometimes null)
```
`quotaLimits` appears **only** on rejected records (34 seen, all `five_hour`). Usage is all zero on these.

`~/.claude/stats-cache.json` holds daily aggregates only — not useful for windows.

## 2. `claude -p --output-format stream-json --verbose`

Event sequence for `claude -p "say hi" --model haiku --tools ""`:
```
system/hook_started, system/hook_response   ← user's SessionStart hooks DO run in -p
system/init        { claude_code_version, model, apiKeySource, permissionMode, tools, session_id, ... }
assistant          { message{model,usage(partial: output_tokens may be low)}, request_id, timestamp }
rate_limit_event   { rate_limit_info }
result/success     { is_error, api_error_status, stop_reason, terminal_reason, num_turns,
                     total_cost_usd, usage (final, same shape as log usage), modelUsage{<model>:{...}} }
```

`rate_limit_event` (status allowed):
```json
{"type":"rate_limit_event","rate_limit_info":{
  "status":"allowed","resetsAt":1793294400,"rateLimitType":"five_hour",
  "overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false,
  "unifiedWindows":{
    "five_hour":{"utilization":0.05,"resetsAt":1793294400},
    "seven_day":{"utilization":0.28,"resetsAt":1793527200}}}}
```
- `utilization` is a fraction of the cap with 2 decimals.
- On a reject, `status` is `"rejected"` and `result.api_error_status` is 429 (inferred from the log records
  above; not provoked on purpose).
- Use `result.usage` / `result.modelUsage` for per-run token accounting; the streamed `assistant` usage is partial.

The `-p` run is written to `~/.claude/projects/` like any session, with `entrypoint: "sdk-cli"`.
`--no-session-persistence` turns that off.

Runner-relevant flags (2.1.308): `--allowedTools`, `--disallowedTools`, `--tools`, `--permission-mode`,
`--model`, `--fallback-model`, `--max-budget-usd` (print only; list-price cost as a hard per-run cap),
`--no-session-persistence`, `--session-id`, `--resume`, `--setting-sources`, `--strict-mcp-config`,
`--append-system-prompt`, `--add-dir`.
**`--bare` is unusable for us**: it never reads OAuth/keychain, so it can't use the subscription.
To avoid the user's hooks/plugins in jobs, try `--setting-sources project,local` + `--strict-mcp-config`
(milestone 2).

## 3. Direct quota sources

| Source | Cost | Shape | Notes |
|---|---|---|---|
| `rate_limit_event` in stream-json | rides on a real call | structured, 2-decimal fraction, both windows | free byproduct of every job iteration |
| `claude -p "/usage"` | **0** (no model call) | text, integer %, both windows | daemon can poll this while idle |
| statusline stdin | — | not checked | relevant for milestone 7 only |
| session logs | 0 | tokens, local machine only | fallback + activity detection |

`/usage` output (2.1.308):
```
Current session: 6% used · resets Oct 30 at 1:20am (Asia/Taipei)
Current week (all models): 29% used · resets Nov 1 at 6pm (Asia/Taipei)
```
"Current session" = `five_hour`. The reset string has no year and uses the user's TZ; parse with care.
It prints a minute early (1:19am / 5:59pm for exact 01:20:00 / 18:00:00 from `rate_limit_event`), so
`internal/usage` rounds it to the nearest 10 minutes. ~2–3 s per call with
`--no-session-persistence --setting-sources project,local --strict-mcp-config` (OAuth still works, no hooks).
Other plans may show extra lines (e.g. a per-model weekly cap) — parse all `Current week (...)` lines and
treat the max as binding. When no 5h window is active the format is unverified.

Numbers agreed: `rate_limit_event` 0.05 / 0.28 vs `/usage` 6% / 29% a minute later (another session was active).

## 4. Window anchoring

5h `resetsAt` minus 5h vs. the first logged request after the previous window:

| first request (UTC) | implied window start | 
|---|---|
| 2026-10-06 01:11:53 | 01:10 |
| 2026-10-15 06:11:00 | 06:10 |
| 2026-10-29 07:02:45 | 07:00 (prev window) |
| 2026-10-29 12:27   | 12:20 (reset 17:20Z = 01:20 Taipei) |

→ The 5h window starts at the first request after the previous one expired, floored to 10 min. Caveat:
requests from claude.ai/other machines are invisible here, so this could be off in some cases — always prefer
`resetsAt` from a direct source.

Weekly: `resetsAt` 2026-11-01 10:00Z, hour-aligned; a fixed weekly anchor per account.

Pacer implication: when no 5h window is active, the first gmwd request *opens* one. Starting a job right
after expiry anchors a full fresh window.

## Open
- `/usage` text when no 5h window is active / on other plans.
- Whether `--setting-sources project,local` keeps OAuth working and skips user hooks.
- Statusline stdin fields (milestone 7).

## 5. Unattended permissions (checked for milestone 2, haiku, scrubbed env)

All runs: `--permission-mode dontAsk` (anything not pre-approved is denied, nobody is asked),
`--setting-sources project,local --strict-mcp-config --no-session-persistence`, and a minimal env
(`HOME USER LOGNAME PATH TMPDIR LANG`) — OAuth still works with that env.

| Check | Result |
|---|---|
| `--allowedTools 'Write(./**)' 'Edit(./**)'`: write in cwd / outside | inside written; outside denied, listed in `result.permission_denials` |
| Read / Glob / Grep with no rules: in cwd / outside | inside allowed; outside denied (cwd and `--add-dir` are readable by default) |
| `--json-schema '{...}'` | validated object in `result.structured_output` |
| Bash, `--settings '{"sandbox":{"enabled":true,"autoAllowBashIfSandboxed":true}}'`: write outside cwd | `operation not permitted` (writes allowed only in cwd and `$TMPDIR`) |
| same, network (`curl https://example.com`) | blocked: `deny network-outbound example.com:443` |
| same + `"allowUnsandboxedCommands":false`, no `Bash` in `--allowedTools`, model told to use `dangerouslyDisableSandbox` | sandboxed Bash auto-allowed; escape not possible, write still denied |

Follow-up from the first real solve run (milestone 2 e2e):

| Check | Result |
|---|---|
| Bash *not* pre-approved, command with `$VAR` expansion (`GOCACHE=$TMPDIR/x go test`) | **denied** under dontAsk — sandbox auto-allow doesn't cover it |
| `--allowedTools Bash` + `allowUnsandboxedCommands:false`: `$`-expansion, write outside, `dangerouslyDisableSandbox` | expansion runs; outside write still `operation not permitted`; no escape |
| write to `/tmp/x` in the sandbox | denied |
| `$TMPDIR` in the sandbox | a sandbox-private temp dir (not the parent's `$TMPDIR`); writable |
| `"filesystem":{"allowWrite":["<dir>"]}` in sandbox settings | `<dir>` writable |
| `--allowedTools 'Edit(<dir>/**)'` | does **not** make `<dir>` writable for Bash |

So the runner pre-approves `Bash` (only ever together with `allowUnsandboxedCommands:false`) and gives each
solve topic a cache dir `~/.gmwd/cache/<id>` via `filesystem.allowWrite`, with `GOCACHE`, `XDG_CACHE_HOME`
and `npm_config_cache` pointing into it.

Consequences for solve jobs:
- In a worktree, `.git` points into the main repo, which is outside cwd → the model can't commit; gmwd commits.
  Network is blocked, so `git push` can't work either.
- Toolchains whose caches aren't covered by those env vars need more entries (later: per-topic config).
- Sandboxed Bash can still *read* most of the disk; with network off it can't send it anywhere.
