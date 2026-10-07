# Worker T Report — TUI transcript visibility + restored-subagent submission fix

Branch: `local/full` · Commit: NOTHING (as instructed) · Gates below.

## Part 1 — Investigation findings (user directive: thinking + tool output shown by default)

**(a) Assistant THINKING/reasoning in the live TUI:** already rendered **expanded, full content, muted
style** — `internal/tui/transcript.go` assistant case renders `entry.reasoning` under the `· thinking`
header via `thoughtBodyStyle` (italic slate, left border). History messages carry `ReasoningContent`
(session.AddAssistantMessageWithTools persists it; history_sanitize.go keeps it; only the *pruned
on-disk* transcript `cmd/late/subagent_transcript.go` drops it — that's tool-side and out of scope).
The earlier research note ("reasoning dropped in pruned transcripts") refers to the pruned snapshot,
NOT the live TUI. No change needed for reasoning; verified by test.

**(b) TOOL OUTPUT blocks:** `client.ChatMessage{Role:"tool"}` results were **dropped entirely** from
the live transcript — the render switch had no `"tool"` case (only the one-line `↳ tool` call badge
on the assistant message). This was the real visibility bug.

**Truncation-in-rendering:** none existed. Only per-row width clamps (`ansi.Truncate` to viewport
width) and glamour word-wrap. No `maxChars` content clamps in rendering (the `clip()`/`previewText()`
clamps live in the pruned on-disk transcript writer — separate surface, untouched). Added full
tool-output rendering rides the existing block cache + viewport pagination → no O(n) regression.

## Part 2 — Implemented

1. **Expanded-default tool output** (`internal/tui/transcript.go`): new `case "tool"` renders a
   `↳ tool output for <call-id>` header + full body (one muted row per line, `attachmentStyle`),
   cached in the block cache like every other role; paginated by the existing viewport.
2. **Cheap collapse affordance (global, slash command):** `/collapse` folds tool blocks to a
   one-line `↳ tool output for <id> (N bytes) — /expand to show` summary; `/expand` restores.
   Default state = **expanded**. Runtime-only (never persisted; fresh state always starts expanded).
   Per-block ctrl+o toggle rejected — ctrl+o is the file picker. Thinking/reasoning is unaffected by
   collapse (directive: reasoning always shown). Implementation: `toolOutputsCollapsed` flag on
   `transcriptState` + `transcriptRenderedMsg` (worker-safe immutable render input), cache key now
   carries the flag so toggling invalidates; `ToggleToolOutputCollapse(collapsed)` on Model; commands
   wired in update.go next to `/timestamps` (toast + dirty, no persistence); listed in
   AvailableCommands alphabetically; help view gained a "Transcript Visibility" section.
3. **Reasoning rendering:** no code change needed (was already expanded) — pinned by tests.
4. **Live child indicator (parent status bar):** when the focused agent is NOT the busy one, a
   compact muted segment `coder #0: thinking` / `coder #0: tool bash` / `…: working` appears in the
   status bar for the first busy background child (`StateThinking`/`StateStreaming`, not closed, not
   focused). Tool-kind detection: streaming state with a pending ToolCall and no streamed prose →
   `tool <name>`. No mirroring; one line; hidden while the child itself is focused.
5. **Tests** (`internal/tui/transcript_visibility_test.go`, 10 tests): reasoning expanded by default;
   tool output expanded by default (full lines present); `/collapse`→summary+hides body, reasoning
   still visible; `/expand`→restores; zero-value state expanded; toggle no-op/cache-invalidation
   semantics; command listing; live child indicator (thinking/tool/focused-hidden/idle-hidden);
   focused agent never summarized as a child.
   Plus **regression tests for the user-reported bug** (see Part 3): submit-on-restored-tab routes
   to parent; live focused agent unchanged.

## Part 3 — User-reported bug: "Error: subagent coder-subagent-200 is restored and read-only"

**Root cause.** At session load, `restoreInterruptedSubagents` (`cmd/late/subagent_resume.go:197`,
wired at `cmd/late/main.go:1089`) mounts `agent.RestoredSubagentOrchestrator` stubs on the root so
interrupted children are browsable tabs. The stub intentionally refuses Submit/Execute
(`internal/agent/restored.go:87/93`). But the TUI submit path called `m.Focused.Submit(...)` directly
(`internal/tui/update.go` submitMessage + the messageHookResultMsg path), so pressing Enter while a
restored tab was focused surfaced the refusal — and nothing routed the work to the sanctioned resume
flow. The restored session CAN continue: the explicit path
`resumeSubagent` → `agent.NewResumedSubagentOrchestrator` (same ID, full persisted history, live
session; `-r1` suffix if the root already lists the ID) → `buildAndWireChild` runs it exactly like a
fresh spawn. The parent model is instructed by the resume synthesis to call
`spawn_subagent {"resume": "<id>"}` — the only missing link was TUI-side routing.

**Fix (tui/update.go only — my owned file).** `submitMessage` now resolves a submission TARGET:
if the focused orchestrator is a restored record (structural detection `interface{ StatusText() string }` —
same probe the tab handler already uses at update.go:1675; `RestoredSubagentOrchestrator` is the only
production type with it; the tui package cannot import internal/agent), the target becomes its
parent (fallback: root; stubs' own `Parent()` returns nil). Preflight context check now evaluates the
TARGET's window (unchanged behavior when target == focused; for a rerouted submission the parent's
context is what can overflow, and the focused tab still gets a status note if the parent trips the
warning). Attachment re-validation uses the target (prevents silently dropping images because the
stub has SupportsVision()==false). Both submit paths (sync and onMessageSend-hook) Submit to the
target; `finishSubmit(target)` books history/thinking on the target state (it already took an
explicit target for exactly this reason). The user's message now reaches the parent model, which
continues the child via the sanctioned resume flow.

Not changed (would collide with the owner worker): `internal/agent/restored.go`,
`cmd/late/subagent_resume.go`, `cmd/late/main.go`. Suggested follow-up for that worker: consider
softening the stub's Submit refusal to a pointer at resume, and/or auto-queueing resume
notifications when a restored tab receives input.

## Files touched

- `internal/tui/transcript.go` — tool role rendering (expanded default + collapsed summary),
  collapse flag plumbing, `padRow` helper, cache-key flag.
- `internal/tui/update.go` — /collapse + /expand commands; submission-target reroute for restored
  tabs (bug fix); target-based preflight.
- `internal/tui/state.go` — /collapse + /expand in AvailableCommands.
- `internal/tui/view.go` — live child-activity segment in statusBarView; help section.
- `internal/tui/transcript_visibility_test.go` — NEW, 10 tests (incl. bug regression tests).

NOT touched (per constraints): orchestrator/base.go, executor, main.go, quickstart docs,
internal/agent, cmd/late.

## Gates

- `gofmt -l internal/tui/` → clean (all touched files formatted).
- `go build ./...` → OK.
- `go test -race -count=1 ./...` → ALL PASS except `internal/executor`
  `TestExecuteToolCalls_WatchdogToolKillDoesNotAttachNote` — **pre-existing flake in a package I
  never touched** (`git diff HEAD -- internal/executor/` empty; owned by another worker): fails
  intermittently ONLY under `-race` ("context canceled" races the kill path), passes without it,
  passes `-count=1` repeatedly, failed `-count=3` under `-race`. Recommend the executor owner fix
  the race window.
- `internal/tui` under `-race -count=2` → ok (15.6s / 19.3s).
- `~/go/bin/golangci-lint v2.13.2` (not on PATH; lives in ~/go/bin): `run ./internal/tui/...` →
  **0 issues**. Full-repo run shows 2 **pre-existing** issues in files I never touched:
  `internal/compaction/client.go:755` (staticcheck S1024) and `internal/tool/diagnostics.go:35`
  (unused `diagnosticsSink`; internal/tool carries another worker's uncommitted changes) — out of
  my scope, reported for the main agent.
- quicksilver: not installed on this machine (graceful skip). tldr-code + jgrep activated; bash
  was heavily OTP-gated so native tools (search_content/find_files/read_file/target_edit) carried
  the investigation.
