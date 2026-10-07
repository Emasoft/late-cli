# SYSTEM AUDIT 4/4 — TUI + agent lifecycle + spawn/resume tool surface + prompts

Scope: `internal/tui/{update,view,state,model,transcript}.go`, `internal/tool/subagent.go`, `internal/agent/{agent,restored}.go`, `internal/assets` prompts; cross-checked `cmd/late/main.go` (runner/resume/restore wiring), `cmd/late/subagent_resume.go`, `internal/session` (LoadHistory/CompactContext), `internal/executor` (tool-error surfacing).
Baseline: branch `local/full`, uncommitted Audit-1 + Audit-2 + Audit-3 fixes in the working tree — built on top, nothing reverted, nothing committed.

Gates (all green):
- `gofmt` — all touched files clean.
- `go build ./...` — OK.
- `go test -race -count=1 ./...` — full run OK (all 19 packages; no flakes this run). A 4-package concurrent sub-run tripped ONE failure in `internal/tool` that never reproduced: standalone, in the 4-package combo (re-run), and in two full-suite runs — all green; the failing test's name was lost to output truncation. My added tool tests are deterministic (temp dirs, no chdir, no git); the tool package's worktree tests chdir via `restoreCWD`, the historical load-flake suspect. Documented as un-reproduced.
- `golangci-lint v2.13.2` — 0 issues in touched files/packages. Two pre-existing findings exist in COMMITTED, untouched code (report-only, see R9).
- Commit: nothing.

---

## FIXED (code + tests)

### F1 — spawn_subagent fresh-spawn argument validation (HIGH, silent-misbehavior class) — `internal/tool/subagent.go`
The tool trusted the JSON surface completely: `required: ["goal","agent_type"]` in the schema is only a steer (a provider need not enforce it, and an empty string satisfies every JSON type). Consequences on the runner path:
- **Empty/whitespace goal** spawned a child whose only instruction is `Goal: ` — nothing to do, yet the manifest records it as running work (blocks the ID sequence, pollutes resume correlation).
- **Empty/unknown `agent_type`** failed deep in the runner with a bare Go error ("unknown agent type: ") instead of a retryable hint.
- **Nonexistent / directory `ctx_files`** were **silently dropped** when the goal message was built (`os.ReadFile` err → skipped in `newSubagentOrchestrator`), so the model believed context was attached when it was not.

Fix: new `validateFreshSpawnArgs` runs in `Execute` on the fresh-spawn path, **before** the worktree check (worktree creation has side effects: a branch + a directory). Every failure is an error RESULT (nil Go error, `Error: …` prefix — the same convention the timeout and worktree validators use) so the model reads the hint and retries; the runner is never invoked. Validated: goal non-empty after trim; agent_type non-empty and one of the configured types (`assets.GetSubagents()`, the same source the schema enum is built from; a broken embedded registry disables the type check rather than bricking spawns); every `ctx_files` entry must exist and be a file, with all offending entries named in one result. Resume requests (`{"resume": …}`) bypass the validation deliberately — all other fields are ignored there, and a goal-less call is exactly the shape a resume takes when the provider doesn't enforce `required`.
Tests: `TestSpawnSubagentTool_FreshSpawnArgumentValidation` (7 rejection cases + pass-through case + resume-bypass case).

### F2 — unreadable ctx_files are annotated, never silently dropped — `internal/agent/agent.go`
Defense in depth behind F1 (the validation is stat-based; a file can vanish or become unreadable between validation and the spawn, and `NewSubagentOrchestrator` is also called directly by tests/plugins). The goal-message builder now writes `- <path>: (could not be read: <err>)` for every unreadable entry instead of skipping it, so the child knows the promised context is absent instead of rediscovering it (or reporting work "done" without it).
Tests: `TestNewSubagentOrchestrator_UnreadableCtxFileAnnotated` (missing file + directory, both annotated; goal preserved).

### F3 — live resume refuses a missing/empty preserved history (HIGH, silent task-less child) — `internal/agent/agent.go`
`validateResumeRecord` requires `HistoryPath != ""`, but nothing verified the file still exists. `LoadHistory` returns `([]ChatMessage{}, nil)` for a missing or zero-length file, and `NewResumedSubagentOrchestrator` deliberately does **no goal re-append** (the loaded history is the child's only task statement). Manually deleting a child's history file and resuming therefore produced a LIVE child with an empty conversation and no goal — the run would either fail opaquely at the provider or, worse, "continue" from nothing while the manifest said the work was running.
Fix: after `LoadHistory`, `len(history) == 0` → error `resume <id>: the preserved history at <path> is empty or missing — its conversation cannot be restored; spawn a fresh agent instead`, child == nil, nothing wired into the parent, manifest untouched. Mirrors the UI-stub rule in `restored.go` (a missing file lists fine as an empty historical record — only the LIVE resume is refused).
Tests: `TestNewResumedSubagentOrchestrator_EmptyHistoryRefused` (missing file + emptied file; asserts error wording, nil child, empty id, parent holds 0 children).

### F4 — restored child's status line surfaces in the TUI (was invisible) — `internal/tui/update.go`
`agent.RestoredSubagentOrchestrator.StatusText()` carries "restored — was interrupted" + the manifest cause, and its doc says "surfaced through the interface-exposed state text … so the resume layer can carry the record's cause into the UI" — but **nothing in the TUI ever read it**. Tab-switching to a restored child created a fresh AppState with the default `StatusText: "Ready"`, hiding the one fact the user needs when browsing a historical child (its transcript is the record of an interrupted run, not a live agent).
Fix: the `tab` handler now seeds the freshly created state from `interface{ StatusText() string }` — but **only when the state still carries the "Ready" default**, so live statuses (error lines, "Working...", confirm prompts) are never overwritten. The tui package cannot import internal/agent (import cycle: agent wires tui.Messenger), hence the anonymous-interface probe.
Tests: `internal/tui/restored_child_test.go` — `TestTabFocusSeedsRestoredStatusText` (seed lands, survives a root→child round trip, root keeps "Ready") and `TestTabFocusKeepsLiveStatusOverSeed` (pre-existing text wins).

### F5 — `/new` during an in-flight compaction freezes the TUI (HIGH, hang-class UX) — `internal/tui/update.go`
`session.CompactContext` holds `historyMu` for the **whole walk** (scorer network round trips included), and `/new` → `Root.Reset()` → `StartNewConversation` takes the same lock. `/new` mid-walk therefore blocked the TUI update goroutine with **no feedback for the walk's entire duration**, and the finished run's "compacted: saved ~N tokens" report would then land on the fresh conversation as if it had compacted it. (The in-memory slice itself is race-safe — the lock ordering is correct — so `-race` never catches this; it's a freeze + misleading-report bug, not corruption.)
Fix: `/new` refuses while `Model.CompactionRunning` with a visible status-line message (not `Model.Err` — that field has no renderer, see R1); `/new` works again once `compactionResultMsg` clears the guard.
Tests: `TestNewCommandRefusedDuringCompaction` (guard set → no `Root.Reset`, visible refusal naming the compaction; guard cleared → exactly one reset). `mockOrchestrator` gained a `resetCount` field (optional, invisible to existing users).

---

## VERIFIED SOUND on the current tree (no action)

### V1 — Restored children in the TUI (hunt item 2)
`restoreInterruptedSubagents` (cmd/late/subagent_resume.go:129) adds each interrupted record to the root via `AddChild` before the TUI exists; tab cycling iterates `Root.Children()` and lazily creates `AgentStates` via `GetAgentState`, so restored children are first-class tabs. The transcript renders straight from `m.Focused.History()` → the loaded preserved history renders like any other history (badges render from recorded tool-call names with a nil-registry guard — restored stubs have `Registry() == nil`). `RestoredSubagentOrchestrator.Events()` returns a **closed** channel (restored.go:119) — re-verified on the current tree: `ForwardOrchestratorEvents`'s `range` terminates immediately, no goroutine leak per restored child. `MaxTokens() == 0` (context-unknown convention): every consumer guards — `renderContextBar` renders count + `∞` for 0 and `?` for <0 (division only in the positive branch), `maybeJevAutoCompact` skips on `maxTokens <= 0`, `submitMessage` preflight requires `maxTokens > 0`, welcome/info-bar render "unlimited"/"auto". No division by zero anywhere.

### V2 — TUI update loop message coverage (hunt item 3)
All 8 `common.Event` types are handled in the `OrchestratorEventMsg` switch (Content, Status, Retry, Recovery, ChildAdded, StopRequested, MessageQueued, SubagentIdle) — no silent drops. Toast expiry races remain fixed: every toast-set rewrites `ToastExpireTime` and the `clearToastMsg` handler ignores a tick that arrives while "now" is still before the current toast's expiry (update.go:183-199, pinned by `diagnostics_test.go`). Typed 413/ContextExceeded surfaces verified end-to-end: `StatusEvent error` → `errors.Is(ErrPayloadTooLarge)` → guidance toast + one-shot root recovery compaction (`maybePayloadRecoveryCompaction`, `CompactionApplies`-gated, `PayloadRecoveryUsed` one-shot reset by /new); `errors.Is(ErrContextExceeded)` → 8s guidance toast, no TUI-side compaction; `transcriptError` renders the dedicated `**Context Limit Exceeded**` card from the typed sentinel plus the legacy text match (pinned by `context_exceeded_surface_test.go`, `payload_recovery_test.go`).

### V3 — Archived tool-output reference forms in the transcript (hunt item 3, stale-reference rendering)
The compact reference form (`<head>\n…[full output archived: <path>]`) lives only in `tool`-role messages; the transcript's entry switch has **no `case "tool"`** — tool results are collapsed into the assistant entry's tool badges (name + `CallString`), so the reference line is never fed through the markdown renderer and cannot break block parsing. User content uses `UIString()`; only assistant/notice/error content goes through glamour, and `[text]` without a target is inert there anyway.

### V4 — Prompts: embed, resilience block, `${{CWD}}` (hunt item 4)
`go:embed prompts/*.md subagents/*.json` covers all three instruction files; `TestEveryDefaultPromptCarriesResilienceRules` pins the block in the root orchestrator prompt **and** every config-referenced subagent prompt. The `${{CWD}}` replacement is a plain `strings.ReplaceAll` over the whole prompt; the resilience blocks contain no placeholder-like text, so the replacement cannot corrupt them (and coding.md's `${{CWD}}` is the only one in subagent prompts, asserted by the worktree prompt tests). Sizes: coding 3.5 KB, researcher 3.2 KB, orchestrator 9.0 KB — all sensible. `${{NOTICE}}` bash-disabled substitution and the `<|think|>` prefix compose after the block without touching it. Resume wording consistency: the tool description, `manifestInterruptedText`'s directive ("call spawn_subagent with {"resume": "<id>"} … Do NOT re-state the goal"), and the resilience block coexist — the in-context interruption result is directive and wins over the block's generic delta-resume rule (see R6).

### V5 — Config surfacing (hunt item 5)
Strict-parse errors **exit the process before the TUI** (`shouldExitOnConfigLoadError`: parse/read error with nil config → exit; that is the audited design — nothing to surface in-TUI). `ResolveSaveSubagentHistories` implements the documented tri-state (CLI explicit > saved session preference > config `*FlexBool` > default ON); `resolveSubagentTimeout`/`effectiveSubagentBudget` surface invalid per-spawn timeouts as retryable error results. `context-size-tokens`: a non-numeric value fails JSON strict-parse → ConfigParseError → exit; a value ≤ 0 is silently treated as "unknown" (consistent with `MaxTokens() == 0` rendering; noted R7).

### V6 — Spawn/resume input-safety edges (hunt item 1 & 6)
- **Resume id with path chars / traversal**: `manifest.Get(id)` is a map lookup — no path is ever derived from the caller-supplied ID; `LoadHistory` reads the **manifest-recorded** path. Safe.
- **Resume of a terminal record**: `validateResumeRecord` refuses completed/failed/cancelled with cause-carrying wording; unknown status refused; empty `HistoryPath` refused (F3 now covers the "recorded but deleted/empty" sibling one level deeper).
- **Worktree removed while a child runs in it**: the shell tool sets `cmd.Dir = <worktree>` (resolved per call from `common.GetWorktreeDir(ctx)`); a vanished dir fails `CombinedOutput` with the raw `chdir …: no such file or directory` → normal tool-error result → the model sees it and can act. Ugly but functional (see R8).
- **Quit during compaction**: `SaveHistory`/walk persistence are `writeAtomic` (rename of a complete document), so a mid-walk exit can never leave a partial history file; the goroutine simply dies with the process.

---

## REPORT-ONLY (no code changes)

### R1 — `Model.Err` is a dead-end field (cross-cutting error-surfacing gap)
`Model.Err` is SET by at least 8 handlers (`/new` reset failure, `/log` git failure, `/rewind` load failure, `/model` picker config-save failure, compose read/write/exec failures, attachment read failure, message-hook Submit failure) but **never rendered by any view** and never cleared — the user sees nothing when these fail. This is why F5 routes its refusal through the status line instead. A proper fix (render-once error box or a diagnostic-style toast sink with a per-cause clear policy) touches many handlers and every view mode — it deserves its own mission. Note the pattern is inconsistent today: handler-local errors that matter (config-save failures in /infobar //timestamps) already use `StatusText`, others write into the dead end.

### R2 — Restored children flood the event buffer at startup (theoretical startup hang)
`AddChild` sends `ChildAddedEvent` **blocking** on the parent's buffered(100) `eventCh` ("MUST be sent BLOCKING"). `restoreInterruptedSubagents` runs at main.go:1085, before `ForwardOrchestratorEvents` (main.go:1388) starts the consumer. With ≤100 restored children the buffer absorbs the events and the forwarder drains them fine; with **>100** interrupted children in one manifest, startup blocks forever inside `AddChild`. Unreachable in practice (100+ simultaneously-interrupted children in one session), but the shape is real: either start the forwarder before the restore, or make AddChild's send bounded + drain-on-overflow.
Related cosmetic: those drained ChildAddedEvents hit the TUI's `case common.ChildAddedEvent: s.StatusText = "Subagent spawned"` with the ROOT's id, so a resumed session with restored children boots with "Subagent spawned" in the status bar until the next root status event.

### R3 — `spawn_subagent` schema `required: ["goal","agent_type"]` vs resume mode
When a provider DOES enforce `required`, a resume call forces the model to fabricate a goal (harmless — ignored) and an `agent_type` (mildly dangerous: a fabricated type that disagrees with the record fails the resume with "resume id X belongs to a coder subagent, not Y"). The in-context interruption directive tells the parent the exact resume call shape, and the type-mismatch error is explicit and recoverable, so this is left as-is; the long-term fix is provider-conditional schema support (`anyOf`/`oneOf` over fresh-spawn vs resume), which not all backends accept.

### R4 — No size limit on `goal` (and `ctx_files` count)
The goal becomes a user message with no cap (the 100k CharLimit only guards the human textarea). A model looping on huge goals could balloon the child's context instantly — self-limiting via the context guard, but a generous cap (e.g. 64 KB) on the goal + a count cap on `ctx_files` would fail fast with a clear result. Judgment call left to the maintainer.

### R5 — `validateResumeRecord` + `LoadHistory` TOCTOU window
Between the runner's `LoadSubagentManifest`/`validateResumeRecord` and `NewResumedSubagentOrchestrator`'s `LoadHistory`, the file could be deleted externally. F3 converts the worst outcome (goal-less live child) into a clean error; the window itself is unavoidable without a single-reader design. No action.

### R6 — Prompt rule 4 vs resume guidance (consistency note)
The resilience block's "Delta resume: … Spawn the next mission for ONLY the remaining delta" reads as "always spawn fresh after an interruption", while the interruption tool result and tool description say "prefer {"resume": "<id>"}; do NOT spawn a new agent unless resume fails". In practice the in-context directive wins, but tightening rule 4 to "resume the interrupted child via {"resume": "<id>"} when the interruption report names one; otherwise spawn the delta mission" would remove the ambiguity for models that weight the system prompt heavily. Prompt edit deliberately not made in an audit (it is behavior-steering content with its own test contract).

### R7 — `context-size-tokens: <negative>` silently means "unknown"
`ModelSetting.ContextSizeOverride` only honors `> 0`; a negative entry is ignored without a warning while every other malformed config value warns (strict-parse for types) or exits. A `reportWarning` for `!= 0 && < 0` would match the config-reference honesty. Trivial follow-up.

### R8 — Vanished-worktree bash errors carry no recovery hint
The child sees the raw `chdir …: no such file or directory`. Appending "— the working directory no longer exists; it may have been pruned; continue from the repository root or ask for a new worktree" would let the child self-recover instead of retrying the same cwd. Cosmetic; the failure itself already surfaces.

### R9 — Pre-existing golangci-lint findings (committed code, untouched by audits 1-4)
- `internal/compaction/client.go:755` — S1024 `t.Sub(time.Now())` → `time.Until(t)` (staticcheck).
- `internal/tool/diagnostics.go:35` — `diagnosticsSink` is unused (leftover of commit ccc8f33 "route mid-session diagnostics through the TUI").
Both verified present at HEAD with no working-tree diff on those files; fixing them touches audit-1/2 territory and was left out per scope discipline.

### R10 — Verified-sound extras
- `GetAgentState` lazy creation is safe for restored children (`Closed` false → tab-listed; never receives "closed" events → never auto-removed).
- `agentTypeForID`/breadcrumb handle the `-r1` resume-twin ids (label derivation and model lookup only need the type prefix).
- `AddChild` blocking send is safe in the live-resume path (the parent's forwarder is already draining; buffer headroom ≥ 1).
- `synthesizeDanglingSpawnResults` is idempotent-by-persistence; corrupt manifest degrades to no-restore (logged), never blocks resume.

---

## Files touched by THIS audit
- `internal/tool/subagent.go` — F1 (validation helper + hook in Execute).
- `internal/tool/subagent_test.go` — F1 tests (+`fmt` import).
- `internal/agent/agent.go` — F2 (ctx-file annotation), F3 (empty-history refusal).
- `internal/agent/agent_resume_test.go` — F2, F3 tests.
- `internal/tui/update.go` — F4 (status seeding on tab), F5 (/new guard).
- `internal/tui/paste_test.go` — mockOrchestrator `resetCount` (test helper only).
- `internal/tui/restored_child_test.go` — NEW: F4, F5 tests + restored-child stubs.
- `.ai/audit4-report.md` — this report.

**Summary**: 5 real fixes (spawn-arg validation incl. ctx_files, ctx-file annotation in goal msg, empty-history resume refusal, restored-status seeding in TUI, /new-during-compaction guard) + 10 verified/report-only items; all gates green.
**Full report**: `.ai/audit4-report.md`
