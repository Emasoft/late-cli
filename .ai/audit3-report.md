# SYSTEM AUDIT 3/4 — Orchestrator + executor RunLoop/ExecuteToolCalls + subagent runner + event flow

Scope: `internal/orchestrator/base.go`, `internal/executor/executor.go` (RunLoop/ExecuteToolCalls/ConsumeStream), runner closure + `classifyAndReportSubagentOutcome` + `buildAndWireChild` in `cmd/late/main.go`, `cmd/late/subagent_resume.go`, `cmd/late/subagent_transcript.go`, event wiring (`ForwardOrchestratorEvents`, TUI status handling).
Baseline: branch `local/full`, uncommitted Audit-1 + Audit-2 fixes in the working tree — built on top, nothing reverted, nothing committed.

Gates (all green):
- `gofmt` — touched files clean (5 pre-existing unformatted files in mcp/plugin packages, untouched, not mine).
- `go build ./...` — OK.
- `go test -race -count=1 ./...` — full run OK. One flake in the first full run: `internal/plugin` `TestHandleCommand_ConcurrentWithWriters` tripped its own 60s watchdog under parallel-suite load (the test's comment documents this exact load flake; plugin code is untouched by this audit); passed standalone and in the full re-run.
- `golangci-lint v2.13.2` — 0 issues in `internal/orchestrator`, `internal/executor`, `./cmd/...`.
- Commit: nothing.

---

## FIXED (code + tests)

### F1 — `Execute` early return wedges the orchestrator forever (HIGH, hang class)
`BaseOrchestrator.Execute` sets `isRunning=true`, starts the idle watchdog, and only THEN calls `sess.AddUserMessage(text)` (base.go). On persist failure it returned early — **before** the terminal-status defer was registered — leaving `isRunning=true` forever: every later `Execute` failed with "orchestrator is already running", every `Submit` queued into `pendingMsgs` for a run that would never start, no terminal event was ever emitted (TUI stuck in running state). Reachable whenever the first history persist of a run fails (disk full, bad path, permission); production children call `Execute("")` so it has been latent, but any non-empty-text caller hits it.
**Fix** (`internal/orchestrator/base.go`): on the `AddUserMessage` error path, clear `isRunning` under `mu` and emit a blocking terminal `StatusEvent{error}` before returning (the `cancel()` defer still stops the watchdog). Mirrors `Submit`'s own AddMessage-failure cleanup and the run() terminal contract.
**Test** `base_execute_guard_test.go::TestBaseOrchestrator_ExecuteRecoversFromAddUserMessageFailure`: history path under a regular file → persist fails → two consecutive `Execute`s both return the persist error (not "already running") and two terminal error statuses are delivered.

### F2 — `Execute` terminal defer silently discards queued user messages (HIGH, continuity)
`Execute`'s terminal defer did `o.pendingMsgs = nil`. The TUI allows submitting to a **running** child (`m.Focused.Submit`, update.go:2231) — the message lands in `pendingMsgs`, `MessageQueuedEvent` tells the TUI it's queued, then the child's completion **deleted it**. `run()` deliberately preserves queued messages (comment at base.go:834-838); `Execute` contradicted the contract, and the preserved queue is exactly what a resume (`Execute("")` → `onStartTurn` drain) or interrupt-time `DrainQueuedMessages` needs to restore user input.
**Fix** (`internal/orchestrator/base.go`): terminal defer no longer nils `pendingMsgs`; documented parity with `run()`.
**Test** `TestBaseOrchestrator_ExecutePreservesQueuedMessages`: message submitted mid-stream (server-gated) survives after `Execute` returns (`QueuedMessages()` keeps it).

### F3 — Blocking `MessageQueuedEvent` send on the user-input path (MEDIUM, stall/deadlock class)
`Submit` queued a message under `mu`, then sent `MessageQueuedEvent` with a **blocking** channel send — on the TUI's submit goroutine. Under the exact consumer-stall scenario the harness already hardened everything else for (`droppedEvents` counter path, pinned by `TestBaseOrchestrator_ExecuteDoesNotDeadlockWhenEventConsumerStalls`), this was the remaining send that could wedge the user's input path forever. The event is cosmetic: the message is already safely in `pendingMsgs`, and the TUI branch for it only refreshes the viewport (update.go:2007), which the next turn/terminal event does anyway.
**Fix** (`internal/orchestrator/base.go`): route through `trySendProgress` (non-blocking + drop counting), comment explains the cosmetic-loss tradeoff.
**Test** `TestBaseOrchestrator_SubmitDoesNotBlockWhenConsumerStalls`: 150-chunk stream overflows the 100-slot buffer with **no consumer**; `Submit` must return within 5s while the buffer is provably full (previously: blocked forever).

### F4 — Zero panic containment in the tool path (HIGH, crash class)
`ExecuteToolCalls` → `sess.ExecuteTool` → `tool.Execute` had **no recover anywhere in the run path** (repo-wide `recover()` scan: none). Any panicking tool implementation — buggy builtin, plugin inline tool, MCP bridge — unwound the run-loop goroutine and killed the whole process: root agent, every sibling subagent, the TUI, mid-session. The "panic between runner return and AddToolResultMessage / manifest terminal write" lens is real for panics; the correct seam is the single tool call.
**Fix** (`internal/executor/executor.go`): new `runToolCall(runner, ctx, tc)` wraps exactly one tool invocation with a recover that converts the panic into `fmt.Errorf("tool %s panicked: %v", …)`. The existing error branch then formats it as an ordinary tool result (`"Error executing tool X: tool X panicked: …"`) which enters history via `AddToolResultMessage` — the model sees it and can recover, in-flight-tool registration/clear (`SetInFlightToolCancel`/`ClearInFlightToolCancel`), the per-call context, and the remaining calls in the batch all stay outside the panicking stack and behave unchanged. `inFlightKill` classification is unaffected (panic doesn't cancel `toolCtx`).
**Tests** `internal/executor/tool_panic_test.go`: `TestExecuteToolCalls_ToolPanicIsContained` (panicking tool → error-shaped result in history, healthy second call still executed, nil Go error) + direct unit tests of `runToolCall` (recovery, and pass-through of normal results).

### F5 — Dropped-progress-events report lost on error/stop exits (LOW, monitoring gap)
`reportDroppedEvents()` fired only from `onEndTurn`, which only runs for a turn that committed a successful stream. A turn that ended in a stream error, a 429-budget exhaustion, or a stop carried its consumer-stall drops to the grave — the "events dropped (consumer stalled)" signal the harness promises never reached the operator.
**Fix** (`internal/orchestrator/base.go`): `Execute`'s terminal defer and `run()`'s post-loop exit both call `o.reportDroppedEvents()` (no-op when nothing dropped, so no test churn). The diagnostics-sink routing is unchanged.
**Test**: covered by `TestBaseOrchestrator_SubmitDoesNotBlockWhenConsumerStalls` — after the run finishes, the diagnostics sink must contain the "events dropped" line.

---

## REPORT-ONLY (verified sound or deferred, with evidence)

### R1 — Root agent has no max-turns guard (design gap, flag to product)
Root is constructed with `NewBaseOrchestrator(common.MainAgentID, sess, nil, 0)` (main.go:1071); executor's turn loop is `for i := 0; maxTurns <= 0 || i < maxTurns; i++` → **0 = unbounded turns** for the root, while every subagent gets `resolvedSubagentMaxTurns` (default from appconfig). An LLM stuck in a tool-call loop runs forever, burning requests each turn. Existing outer guards: user stop (ctx), and the idle watchdog — root DOES get the idle policy (main.go:1075), but it is config-dependent: `subagent-idle-timeout` resolving to 0 disables the watchdog and leaves NO bound at all. The output-truncation path (`finish_reason=length` → partial save + "continue more concisely" user message → `continue`) also consumes turns, so it is bounded iff maxTurns>0; with maxTurns=0 a persistently-truncating model loops, growing history by 2 messages/round until the ≥95% context discriminator converts it to the capped compaction path (maxContextCompactionRounds=2) — self-limiting but potentially many wasted requests. Recommendation: a root-level turn budget flag (default off) or a hard-bound on consecutive truncation rounds. NOT fixed: product-behavior change, needs a decision.

### R2 — `eventCh` is never closed; per-child forwarder goroutines leak (bounded-by-spawns leak)
`ForwardOrchestratorEvents` does `for event := range o.Events()` (main.go:2714) but nothing ever closes an orchestrator's `eventCh`. Every spawned child leaves one forwarder goroutine alive forever, pinning the child orchestrator + its session against GC. A long session spawning hundreds of subagents accumulates hundreds of goroutines + retained sessions. The `AddChild` design (child events begin buffering at construction; forwarder spawns on ChildAddedEvent receipt) makes closing non-trivial: a later `Submit`/resume on a closed channel would panic. Report-only; needs a lifecycle owner (e.g., close on orchestrator retirement + re-forwarder spawn on resume).

### R3 — Transcript render is O(history bytes) per frame at up to 60fps while streaming
`renderTranscriptCmd` (tui/transcript.go) rebuilds every entry per render pass: `msg.Content.String()` materialization for all history (line 321-324), cache keys that embed **full entry content** via `fmt.Sprintf` (line 451, and `"stream-markdown:"+block` at 437), plus a per-tool-call `hasResult` rescan of the history tail (`for _, h := range history[i+1:]`, O(n²) worst case). Mitigations already present: frame coalescing (60fps, `framePending`), render on a Cmd goroutine (off the UI thread), per-entry styled-row cache keyed by content. Net effect at ~500k tokens (~2MB history): several MB of key-building garbage per frame ≈ 100+ MB/s allocation pressure while streaming, spinning one core. Safe follow-up: key on (index, generation, role, timestamp, content length) — history is append-only between generations — and hoist `hasResult` into a per-pass map. NOT fixed: render-correctness risk (stale rows after in-place compaction rewrites) requires the generation-bump audit across compaction/rewind; too broad for this pass.

### R4 — 5s snapshot ticker marshals the full child history each tick (bounded, monitored)
`startSubagentSnapshotTicker` → `childSession.SnapshotHistory()` every 5s per running child: O(history) marshal + write outside `historyMu` (Audit-2 fixed the race; the cost remains). At 500k tokens that's a multi-MB marshal+write per 5s while a child runs, one per running child. Acceptable (crash-loss window is the explicit goal, Phase 3a), but worth an amortized/incremental persistence design later. Also verified: stop() joins the ticker goroutine before the final flush, and every defer in `buildAndWireChild` (ticker stop, `EndNestedSpawn`, heartbeat `close(done)`, `requestCancel`) runs even if `child.Execute` panics — no ticker/counter leak on the panic path (the panic itself now can't originate in a tool after F4).

### R5 — Orchestrator control surface: stop reaches children; listing exists but is invisible
- **Stop propagates**: child ctx derives `parentCtx ⊇ runCtx(budget)` via `SetContext` (buildAndWireChild), so `Cancel()` on the root cancels in-flight children through `ExecuteToolCalls`'s per-call contexts; a user can focus a child (tab cycles root + non-closed children, update.go:1611) and interrupt it (`m.Focused.Cancel()`, update.go:2536) — user-initiated kill of a hung child EXISTS and reaches mid-Execute.
- **Gap**: there is no overview listing all children with statuses (idle/running/closed/failed). Closed children drop out of the tab cycle; `Children()` + `FindOrchestrator` exist but nothing renders the tree. A `/subagents` view is cheap and would surface stuck children the user could then kill. Report-only (TUI feature).
- **Monitoring gap**: `SubagentIdleEvent` surfaces only as a one-line status text for the focused agent; a child idling in an unfocused tab is invisible unless the user happens to tab there.

### R6 — Dangling tool_call risk if `AddToolResultMessage` fails after a child run (report-only)
The child's outcome string becomes the parent's tool result at executor.go:275. If that history commit fails (persist error), `ExecuteToolCalls` returns an error and the run breaks — leaving the assistant's `tool_calls` in history without a matching `tool` message; every subsequent request 400s (bad-body budget burns, then terminal error). No rollback exists for this shape (`PopLastUserMessage` is user-only). Catastrophic-persist territory; a tool-result rollback primitive would be the fix. Also note `classifyAndReportSubagentOutcome` returns `(result, nil)` for failed children — deliberate: the manifest record + transcript path carry the failure, and the parent gets the resume instructions.

### R7 — Verified sound (no action)
- **run() error branches**: every exit path emits a blocking terminal event — cancel → "idle" (+`StopRequestedEvent` if the stopCh token landed), image-unsupported → "error(image_unsupported)" + persisted rollback, context-exhaustion → typed guidance error, 400-after-retries → rollback + actionable error, all other errors → "error". `isRunning` cleared on every exit (terminal-error hang fix already present).
- **ConsumeStream/errCh ordering**: `errCh` is buffered(1); every producer error-send precedes `close(out)` (defers: `close(errCh)` runs before `close(out)`), so the post-range non-blocking error read can't miss an error; producer chunk sends select on `ctx.Done()` — no goroutine leak on mid-stream cancel.
- **Compaction-round caps**: context-exhaustion rounds capped at `maxContextCompactionRounds`; infra/bad-body/throttle retry budgets are independent and exhausted budgets return immediately; backoff sleeps are ctx-cancelable and route stops to the stop path, not the error box.
- **Retry-event state**: executor's per-attempt accumulator is fresh; the orchestrator's shared accumulator is reset on retry; `RetryEvent`/`RecoveryEvent` are non-blocking drops by design; terminal "error"/"closed" and "thinking" clear the TUI retry verb. (Residual cosmetic: terminal "idle" doesn't clear `RetryVerb` — a stop during backoff can leave a stale verb string until the next turn's thinking event; status line itself already shows "Ready"/"Stopped".)
- **Double-stop / cancel idempotence**: `Cancel()` is idempotent (stopCh one-shot select, ctx cancel idempotent); resume path's `runCancel` fires via both `buildAndWireChild`'s defer and the runner's own `defer runCancel()` — safe.
- **ChildAddedEvent ordering**: `AddChild` happens in the child constructor before `Execute` starts emitting, and the ChildAddedEvent send is deliberately blocking (drop would orphan the child's whole event stream) — ordering is sound.
- **Sequential tool execution** is by design (fail-closed shell guard depends on it); event channel size 100 matches the drop-counting contract; unbounded goroutine creation not found beyond R2's bounded-by-spawns forwarders.

---

## Test additions
- `internal/orchestrator/base_execute_guard_test.go` — F1, F2, F3, F5 (server-gated SSE mock helper `newStallTestServer`, full-buffer stall rig).
- `internal/executor/tool_panic_test.go` — F4 (containment, batch continuation, pass-through).

All existing tests untouched except `base.go`/`executor.go` behavior under test. Full suite green with `-race -count=1`.
