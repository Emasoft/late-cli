# Phase 1 Report — Subagent Orchestration: Async Execution Core

Mission: async execution with parallel/serial scheduling (spawn_subagent `execution` param, SubagentScheduler, notifications, `subagent_results`, manifest integration, tests). Branch `local/full`, HEAD at start `59f2594` (0bdd263 lineage, newer as expected). **Committed nothing** (per instructions; next phase builds on the working tree).

## Diff summary

```
 cmd/late/main.go                      |  72 ++++++++++++++-------
 cmd/late/subagent_resume.go           |  73 +++++++++++++++++----
 docs/quickstart.md                    |  12 ++++
 internal/session/paths.go             |  24 +++++++
 internal/session/session.go           |  48 +++++++++++++-
 internal/session/subagent_manifest.go |  67 ++++++++++++++++++++
 internal/tool/subagent.go             | 116 +++++++++++++++++++++++++++++++++-
 7 files changed, 375 insertions(+), 37 deletions(-)
 untracked (new): cmd/late/subagent_scheduler.go (~470 lines), cmd/late/subagent_scheduler_test.go (~640 lines)
```

## What landed (per brief item)

1. **`execution` param** (internal/tool/subagent.go): `SubagentSpawnRequest.Execution` (`""`→sync), constants `SubagentExecutionSync/Parallel/Serial`, `NormalizeSubagentExecution`, schema property (enum sync/parallel/serial, resume-ignored note), validation in `validateFreshSpawnArgs` (error-result with hint, before worktree side effects), `CallString` shows mode for background spawns.
2. **Scheduler** (cmd/late/subagent_scheduler.go): `SubagentScheduler` — `mu sync.Mutex`, `running map[string]string` (id→mode), `serialRunning`, `serialQueue`, `parallelQueue`, parent `*session.Session`, `statusWriter`. Rules: parallel launches unless a serial child is live; serial launches only when `len(running)==0`; drain (under the completion's lock section): serial head first (serial chains one-at-a-time), then the whole parallel queue as one batch once the serial queue is empty and no serial is live. Sync spawns bypass the scheduler entirely; orchestrator unaffected.
3. **Result delivery**: completion notification appended into the PARENT session via `sess.AddUserMessage` (established mid-loop harness-note shape): `[late harness] subagent <id> (<type>) <outcome>. Result preview: <200 chars>. Full result: call subagent_results with {"id": "<id>"}.` Full result written to `<sessionsDir>/<sessionID>/subagents/<id>.result.txt` (new `session.SubagentResultPath`), path recorded in the manifest's new `SubagentRecord.ResultPath` via new locked `Session.MarkSubagentResultPath`.
4. **`subagent_results` tool** (internal/tool/subagent.go, same file): `SubagentResultsTool{Lookup}` (thin shell + injected closure — tool package cannot import session). Registered alongside spawn inside `if resolvedEnableSubagents`. Lookup precedence: scheduler live state (running (parallel|serial) / queued (mode, position)) → manifest (queued/running → live-state text; frozen → interrupted + resume hint; terminal → full result file, preview fallback, cause+transcript fallback; unknown status reported factually; not-found error result). Manifest status set extended: queued, running, frozen, completed, failed, cancelled (+ `IsTerminalSubagentStatus` helper).
5. **Runner refactor**: client selection + child construction stay shared above the branch; sync path byte-for-byte the historical budget/wiring/classify code; background branch extracted to `launchBackgroundSubagent` (reuses `buildAndWireChild` + `classifyAndReportSubagentOutcome`), returns immediate launch/queue acknowledgement. Resume path stays sync-only (execution ignored — documented in schema + runner comment).
6. **Draining**: `finished()` → notify parent → lock: delete from running, clear serialRunning, `drainLocked()` pops everything now allowed, then (unlocked) `writeStatus(running)` per drained entry. Notification intentionally precedes the state cleanup: drained⇒notified (happens-before chain through the mutex).
7. **Manifest integration**: constructor writes `running` (unchanged); scheduler stamps `queued` at enqueue (never for immediate launches) and `running` at actual launch; terminal writes stay in `classifyAndReportSubagentOutcome`. Resume synthesis flips non-terminal records (running OR queued) to **frozen** and persists; `validateResumeRecord`, `matchSpawnRecord` (non-terminal tier preference), and `restoreInterruptedSubagents` all accept running/queued/frozen via `isInterruptedSubagentStatus`.
8. **Tests** (cmd/late/subagent_scheduler_test.go): serial-yields + parallel-batch state machine; parallel-immediate + serial-waits-for-LAST-parallel; queue/launch status stamps; background completion → notification + result file + manifest pointer (end-to-end at unit level); `subagent_results` lookup table (9 cases + in-memory); tool shells (schema/parse/empty-id); `execution` param (schema enum, normalization, typo rejection, pass-through, resume bypasses validation); 8-way concurrent completion race probe; frozen flip at resume + TUI restore; resume validator status table.

## Design decisions where the brief left freedom

- **Background context detachment**: the spawn ctx dies with the parent's run (`BaseOrchestrator.run` cancels its derived ctx on return), so a background child inheriting it would be killed the moment the parent's turn ends — making the feature useless. Background runs derive from `context.WithoutCancel(spawnCtx)` (values preserved, cancellation detached) with the budget re-derived **at actual launch** (so a queued child's budget is never burned while it waits). Process exit still kills everything (crash model unchanged); budget deadline + idle watchdog still bound the child.
- **"frozen" semantics** (brief listed the status without defining it): made the explicit persisted interrupted-by-exit state — the next process flips non-terminal (running/queued) records to frozen during resume synthesis, and resume/restore/results paths accept all three non-terminal statuses for backward compatibility with old manifests. Rationale: background spawns' tool calls are answered at launch time, so their death leaves NO dangling call to synthesize; frozen gives `subagent_results` something honest to report and gives the resume hint a carrier.
- **Notification vs. state-cleanup order**: notify first, then release the machine — makes "scheduler drained" a true synchronization point for the notification (verified by -race).
- **Session concurrency fix (beyond the brief, required by the -race gate)**: the brief assumed `appendMessage` is thread-safe under historyMu; true for appends, but `saveAndNotify → UpdateSessionMetadata → GenerateSessionMeta` read `s.History` UNLOCKED, so two concurrent notifications raced. Fixed minimally: `GenerateSessionMeta`/`UpdateSessionMetadata` snapshot under historyMu; internal `generateSessionMetaLocked`/`updateSessionMetadataLocked` for the compaction walk's tail which holds historyMu by design (naive locking there deadlocked `TestJevCompactContextEndToEnd` — found and fixed during verification). Added `Session.HistorySnapshot()` as the concurrent-safe history reader for the scheduler's tests. This also hardens `UpdateSubagentSeq`/`PopLastUserMessage` metadata writes against the new concurrent appenders.
- **Queue position** in the immediate result is the position at enqueue time (documented as approximate; live positions come from `subagent_results`).
- **No scheduler Stop()/CancelAll this phase**: process death is the only shutdown path today (same as sync children); manifest running records read as interrupted at next resume — same recovery semantics, documented in the scheduler header.
- **Docs**: documented in `docs/quickstart.md` (Subagent Control section, next to the `timeout` doc). `quickstart.zh-CN.md` has no corresponding Subagent Control section (structurally diverged); adding an orphan untranslated section would be inconsistent, so skipped — flag to main agent if a zh-CN translation is wanted.
- **Background children in the TUI**: a queued background child is registered with the parent at construction (identical to today's spawn flow) and shows as an active child before its first turn — cosmetic, unchanged from sync semantics, noted here for awareness.

## Gates (verbatim)

1. `gofmt -l cmd/late internal/session internal/tool` (touched files) → no output (clean). Pre-existing unformatted, untouched: `internal/mcp/config.go`, `internal/plugin/command.go`, `internal/plugin/installer.go`, `internal/plugin/manifest.go`, `internal/plugin/skills_cleanup_test.go`.
2. `go build ./...` → success, no output.
3. `go test ./... -race -count=1` →
```
ok  	late/cmd/late	8.956s
?  	late/cmd/mcp-run	[no test files]
ok  	late/internal/agent	4.757s
ok  	late/internal/assets	3.796s
ok  	late/internal/client	17.248s
ok  	late/internal/common	7.457s
ok  	late/internal/compaction	7.003s
ok  	late/internal/config	4.649s
ok  	late/internal/executor	15.254s
ok  	late/internal/git	6.820s
ok  	late/internal/mcp	6.668s
ok  	late/internal/orchestrator	10.332s
ok  	late/internal/pathutil	7.567s
ok  	late/internal/plugin	32.291s
ok  	late/internal/session	14.178s
ok  	late/internal/skill	6.769s
ok  	late/internal/tool	12.832s
ok  	late/internal/tool/ast	6.514s
ok  	late/internal/tui	18.115s
```
4. `golangci-lint v2.13.2` (installed to temp GOBIN, `run ./...`) → 2 findings, **both pre-existing at HEAD and outside this diff** (also documented in `.ai/audit4-report.md`): `internal/compaction/client.go:755` (S1024) and `internal/tool/diagnostics.go:35` (unused `diagnosticsSink`, zero callers). Scoped run over touched packages (`./cmd/late/... ./internal/session/... ./internal/tool/...`) → **0 issues in touched files**.

## Notes for Phase 2

- `cmd/late/subagent_scheduler.go` + `_test.go` are untracked — first commit of phase 2 should add them.
- `Session.HistorySnapshot()` is new public API (session.go) — usable by TUI/other concurrent readers.
- The parent session's own RunLoop appends remain single-goroutine; with background notifications now appending concurrently, any NEW unlocked `sess.History` reader outside session.go would race — use `HistorySnapshot()`.
