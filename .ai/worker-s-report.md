# Worker S Report — Stall Detection & Auto-Resume

Branch `local/full`, uncommitted (Phase-1/1b + Worker-T tree preserved; nothing staged/committed; `internal/tui/*` untouched except one additive status override in `update.go`).

## Gates
- `gofmt` touched files: CLEAN (5 pre-existing unformatted files in mcp/plugin, untouched).
- `go build ./...`: OK.
- `go test -race -count=1 ./...`: ALL GREEN except ONE failure — `TestExecuteToolCalls_WatchdogToolKillDoesNotAttachNote` (internal/executor). **Verified PRE-EXISTING at HEAD**: `git stash` → fails 3/3 runs on the clean tree (timing-flaky, environment-related; my diff does not touch internal/executor). Full log: `expected the killed command's result in history, got "Error executing command: context canceled"`.
- `golangci-lint v2.13.2` on cmd/late, internal/orchestrator, internal/common, internal/tui: **0 issues**.

## Root cause chain fixed
`events dropped (consumer stalled)` bursts → TUI consumer wedged → `AddChild`'s **unconditional blocking** `ChildAddedEvent` send froze the subagent runner goroutine → for background serial children the scheduler slot (and queue) held forever → parent idled behind a cycling idle notification. Separately, idle = "no progress AND no in-flight tool AND no nested spawn" meant an API call that never returned read as **busy forever** — notify-only watchdog could never act.

## 1. Idle definition fix (`internal/orchestrator/base.go`)
- The busy check now computes `toolBlockedFor` from the existing `oldestToolStartAt` stamp (kept the Part-1 evaluator intact, extended it): a tool call **blocked ≥ idleTimeout with zero intermediate output** (tool results arrive only at the end) no longer counts as busy. Surgical: same atomic fields, same middleware stamps, no redesign.
- Delegation exemption: `spawn_subagent` "sync" legitimately blocks the parent's tool slot for the child's whole runtime (parent heartbeats; child has its own watchdog), so a blocked tool under `nestedSpawns > 0` is **not** a stall — the parent is never killed mid-delegation.

## 2. Auto-resume on stuck
- **`SetStallPolicy(cb func(id, cause string))`** on `BaseOrchestrator` (new; mu-guarded; nil = notify-only). Watchdog fires the callback exactly once (double-fire guard + `idleKillReason` interlock so stage-2 kill and stall can never both fire), records `idleKillReason` = `stallCause(...)` with the machine-readable **`stalled:`** prefix, emits the stalled event (below), then **cancels the run ctx** so the wedged agent unwinds; the watchdog goroutine returns.
- `stallCause()` format: `stalled: no activity for <idle> (threshold <idle>); blocked in-flight tool call; last transcript entries: <probe>` — the `stalled:` prefix is the contract the cmd/late classifier keys on.
- **Terminal record + notify**: `classifyAndReportSubagentOutcome` and `subagentCompletionStatus` recognize `stalled:` and pass the cause **verbatim** (previously it would have been wrapped as `idle: killed by...(...)`), write `MarkSubagentStatus(failed, stallCause, transcript)`, and append the parent harness note **with the resume directive**: `... resumable via spawn {"resume": "<id>"}` — both the sync result text and the background `[late harness]` notification (notifyParent appends a dedicated sentence so preview clipping can never hide the directive). `subagent_results` lookup answers stalled records with the same directive. Queue drains via the existing `finished()` path — a stalled serial child frees the slot like any finished child.
- Wiring: `buildAndWireChild` installs the callback; the run cancel func travels inside the run ctx (`withStallCancel`/`stallCancelKey`, new in main.go) so the callback cancels the wedged run for **sync, resumed, and background** spawn paths, even when the budget is unlimited. `launchBackgroundSubagent`'s closure calls it before executing the child.

## 3. Event-send stalls (every remaining blocking send audited)
- New **`sendEvent`** (non-blocking + `droppedEvents` counter) and **`unwedgeSend`** (bounded `unwedgeTimeout = 5s` wait, then count + report `terminal status event dropped (consumer still stalled)`). Converted:
  - `AddChild`'s `ChildAddedEvent`: unconditional block → `unwedgeSend` (was the wedge that froze the runner/scheduler; a slow consumer still receives it, a wedged one costs ≤5s).
  - All terminal status sends in `run()`/`Execute` (idle/closed/error/image/context-exceeded/400/StopRequested), both `onEndTurn` `ContentEvent{Completed}` turn boundaries, Execute's AddUserMessage-failure error send → `unwedgeSend`.
  - `RetryEvent` / `RecoveryEvent` raw `select/default` in both loops → `sendEvent` (now counted).
  - `SubagentIdleEvent` notify + stalled event → non-blocking (notify was already).
- A wedged TUI can now never wedge an agent for more than 5s per terminal send, and every loss is counted/reported.

## 4. Stall visibility
- `SubagentIdleEvent` gained `Stalled bool`; on stall detection the orchestrator emits `SubagentIdleEvent{Stalled: true, IdleFor: blockedFor, Probe}`. TUI (ONE additive override in `update.go`'s existing `SubagentIdleEvent` branch — no Worker-T lines moved): `subagent STALLED (no activity for X) — cancelled, state preserved; resumable via spawn {"resume": "id"}`.

## 5. Tests (`-race`, all pass)
- `internal/orchestrator/base_stall_test.go` (8 tests): end-to-end stall via SSE server + hung tool — callback fires once with `stalled:` cause, run unwinds, tool ctx cancelled, `IdleKillReason` recorded; negative: young tool / nested-spawn delegation exemption; notify-only policy unchanged (no kill, no reason); `unwedgeSend` slow-consumer delivery + wedged-consumer give-up (timing + drop count); `sendEvent` drop accounting; `stallCause` format pinned.
- `cmd/late/subagent_stall_test.go` (3): classifier on stalled child → manifest `failed` with verbatim stall cause, transcript written, resume directive in parent text; scheduler-level stalled serial child → `[late harness]` notification **with directive** + queued child drained + terminal record; `subagent_results` stalled branch. Test waits for scheduler-idle before returning (avoids SessionDir-cleanup race, verified `-count=5`).

## Notes for the orchestrator
- `SubagentIdleEvent` field addition and the classifier changes are the only cross-package contracts; the `stalled:` prefix is asserted by tests in both packages.
- The pre-existing executor failure is documented above for the main agent to own (Worker S did not touch it).
