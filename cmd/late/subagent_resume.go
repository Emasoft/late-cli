package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"late/internal/agent"
	"late/internal/client"
	"late/internal/common"
	"late/internal/orchestrator"
	"late/internal/session"
	"late/internal/tool"
	"late/internal/tui"
)

// spawnSubagentToolName is the registry name of the spawn tool whose calls
// the resume synthesis closes. It mirrors tool.SpawnSubagentTool.Name(),
// declared locally so this file needs no tool-package dependency.
const spawnSubagentToolName = "spawn_subagent"

// synthesizeDanglingSpawnResults closes every dangling spawn_subagent tool
// call in the resumed parent history using the session manifest, and
// PERSISTS the synthesized tool results so a second resume finds no dangling
// calls (the synthesis is idempotent by construction, not by chance).
//
// Crash safety model: nothing runs at crash time, so the manifest's
// "running" records are the source of truth at resume — a record still
// running when the next process reads it means the previous late exit
// happened before the runner could write a terminal status ("interrupted").
// The parent model must not see an unanswered spawn_subagent call (strict
// endpoints reject the exchange, and the request-time sanitizer would
// otherwise paper over it with a placeholder), and the caller's in-flight
// spawn (if any) will still append its own result later.
//
// Correlation: manifest records are keyed by child ID, dangling history
// calls by tool-call ID. When the manifest record carries a non-empty
// spawn_call_id (runtime-stamped at launch — the executor hands the tool
// call's ID into the spawn request), a dangling call whose ID matches is
// bound EXACTLY, which is the only correct answer for repeated identical
// (type, goal) spawns. Records with an empty_or_mismatched spawn_call_id —
// legacy manifests, and any record the exact match did not claim — fall
// back to the positional descend heuristic: a dangling call is matched to
// an unconsumed record by its spawn arguments (agent_type, then goal):
// every spawn creates exactly one record, so this is exact except for
// repeated identical (type, goal) pairs, where the wording is equally true
// of either record. Unmatched calls fall back to the generic interrupted
// wording. The synthesis runs only when the manifest exists; one summary
// line is logged to the common errorlog per invocation.
//
// The loaded manifest is returned (nil when none exists or it could not be
// read) so the resume caller can feed the TUI-side restore
// (restoreInterruptedSubagents) without loading the file twice.
func synthesizeDanglingSpawnResults(sess *session.Session) (*session.SubagentManifest, error) {
	effectiveSessionID := deriveEffectiveSessionID(sess.HistoryPath)
	if effectiveSessionID == "" {
		return nil, nil // no session folder: nothing was ever persisted
	}
	manifestPath, err := session.SubagentManifestPath(effectiveSessionID)
	if err != nil {
		// Unsafe IDs can never have produced a folder; nothing to do.
		return nil, nil
	}
	if _, statErr := os.Stat(manifestPath); os.IsNotExist(statErr) {
		return nil, nil // no manifest: no subagent ever spawned in this session
	}
	manifest, err := session.LoadSubagentManifest(effectiveSessionID)
	if err != nil {
		// A corrupt manifest must not make the session unresumable: the
		// request-time sanitizer still closes the dangling calls.
		common.LogErrorf("subagent-manifest", "failed to load manifest for %s: %v", effectiveSessionID, err)
		return nil, nil
	}

	// answeredIDs collects every tool-call ID that already has a tool result
	// message in the restored history.
	answeredIDs := make(map[string]bool)
	for _, m := range sess.History {
		if m.Role == "tool" {
			answeredIDs[m.ToolCallID] = true
		}
	}

	// consumed records are matched exactly once, so repeated identical
	// spawns cannot both resolve to the same record.
	consumed := make(map[*session.SubagentRecord]bool)

	synthesized := 0
	for _, m := range sess.History {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Name != spawnSubagentToolName || answeredIDs[tc.ID] {
				continue
			}
			rec := matchSpawnRecord(manifest, tc, consumed)
			consumed[rec] = true // also fine for nil (generic wording)
			content := manifestInterruptedText(manifest, tc, rec)
			if err := sess.AddToolResultMessage(tc.ID, content); err != nil {
				return nil, fmt.Errorf("persist synthesized result for %s: %w", tc.ID, err)
			}
			answeredIDs[tc.ID] = true
			synthesized++
		}
	}

	if synthesized > 0 {
		common.LogErrorf("subagent-manifest", "resume synthesized %d tool result(s) for dangling spawn_subagent call(s) in session %s", synthesized, effectiveSessionID)
	}

	// Freeze the interrupted: every non-terminal record (running OR queued —
	// queued covers background children that never launched before the
	// previous exit) is by definition interrupted by that exit, whether or
	// not its spawn call was dangling above (a background child's spawn call
	// is answered at launch time with a queue acknowledgement, so its death
	// leaves no dangling call to synthesize). Making "interrupted" an
	// explicit persisted status — frozen — replaces the "still running at
	// read time" idiom and gives the subagent_results tool a status to
	// report. Persisted so later resumes see frozen, not a stale running.
	// PID-liveness bake (Step 6): a "running" record whose owner process is
	// still alive is NOT interrupted — another late process owns that child
	// and will land its terminal status. Its PID file (written at launch)
	// keeps its live/running semantics: freezing it here would let a second
	// resume relaunch it alongside the live original, and stealing the slot
	// would split one conversation across two processes.
	liveOwners := subagentLiveOwnerIDs(effectiveSessionID, manifest)

	frozen := freezeInterruptedRecords(manifest, liveOwners)
	if frozen > 0 {
		if err := manifest.Save(); err != nil {
			// Non-fatal: the in-memory copy still says frozen for the TUI
			// restore below; the next resume would simply re-freeze.
			common.LogErrorf("subagent-manifest", "failed to persist frozen records for %s: %v", effectiveSessionID, err)
		}
		common.LogErrorf("subagent-manifest", "resume marked %d interrupted subagent record(s) frozen in session %s", frozen, effectiveSessionID)
	}
	// Stale PID files of interrupted records are removed best-effort: the
	// interrupted-frozen state is canonical now, and a leftover owner-dead
	// PID file would only ever re-disambiguate a record that no longer says
	// running. Errors are logged, never fatal.
	removeStaleSubagentPIDs(effectiveSessionID, manifest, liveOwners)
	return manifest, nil
}

// subagentLiveOwnerIDs returns the IDs of manifest-"running" records whose
// PID file reports a still-alive owner process (nil when the session has no
// folder). Every other errno condition — missing file, dead owner, EPERM —
// falls on the interrupted side, and a liveness-read error logs and falls
// on the interrupted side too: a corrupt PID file must never orphanize a
// resumable child in the other direction (the PID files are advisory; the
// manifest write is the commit).
//
// The same-exception first: on the WINDOWS build kill(pid, 0) is
// unavailable, so a present PID file reads as alive — the launch/remove
// lifecycle is the signal there (documented degradation in internal/
// session/subagent_pid.go).
func subagentLiveOwnerIDs(sessionID string, manifest *session.SubagentManifest) map[string]bool {
	if sessionID == "" || manifest == nil {
		return nil
	}
	var live map[string]bool
	for i := range manifest.Records {
		rec := &manifest.Records[i]
		if rec.Status != session.SubagentStatusRunning || rec.ID == "" {
			continue
		}
		alive, err := session.IsSubagentAlive(sessionID, rec.ID)
		if err != nil {
			common.LogErrorf("subagent-manifest", "subagent %s liveness check failed: %v", rec.ID, err)
			continue
		}
		if alive {
			if live == nil {
				live = make(map[string]bool)
			}
			live[rec.ID] = true
		}
	}
	return live
}

// removeStaleSubagentPIDs best-effort deletes the PID files of interruptible
// (non-terminal, non-live) records after the freeze synthesis; a queued
// child never had one, so ErrNotExist is the common case. liveOwners holds
// the IDs left running and untouched.
func removeStaleSubagentPIDs(sessionID string, manifest *session.SubagentManifest, liveOwners map[string]bool) {
	if sessionID == "" || manifest == nil {
		return
	}
	for i := range manifest.Records {
		rec := &manifest.Records[i]
		if rec.ID == "" || liveOwners[rec.ID] {
			continue
		}
		if rec.Status != session.SubagentStatusRunning &&
			rec.Status != session.SubagentStatusQueued &&
			rec.Status != session.SubagentStatusInterrupted &&
			rec.Status != session.SubagentStatusFrozen {
			continue // terminal never carried a PID file at this point
		}
		if err := session.RemoveSubagentPID(sessionID, rec.ID); err != nil {
			common.LogErrorf("subagent-manifest", "failed to remove stale pid file for %s: %v", rec.ID, err)
		}
	}
}

// freezeInterruptedRecords flips every non-terminal record (running, queued)
// in the in-memory manifest to frozen and returns how many flipped. Terminal
// in the in-memory manifest to frozen and returns how many flipped. Terminal
// records are untouched. Records in keepAlive (live owners per the PID
// files — see subagentLiveOwnerIDs) are skipped and left "running".
func freezeInterruptedRecords(manifest *session.SubagentManifest, keepAlive map[string]bool) int {
	if manifest == nil {
		return 0
	}
	frozen := 0
	for i := range manifest.Records {
		rec := &manifest.Records[i]
		if keepAlive[rec.ID] {
			continue
		}
		if rec.Status == session.SubagentStatusRunning ||
			rec.Status == session.SubagentStatusQueued ||
			rec.Status == session.SubagentStatusInterrupted {
			rec.Status = session.SubagentStatusFrozen
			frozen++
		}
	}
	return frozen
}

// liveRestoreDeps carries the startup-scope collaborators a live restore
// needs — the same machinery the spawn_subagent {"resume": "<id>"} path uses
// (client routing, tool surface, TUI messenger, the scheduler that runs
// background children). It is built in main() once every collaborator is
// resolved, right after the scheduler is created, and passed to
// restoreInterruptedSubagents. nil (or a nil field) degrades the restore to
// read-only stubs — exactly the pre-live-restore behavior.
type liveRestoreDeps struct {
	// root is the orchestrator the restored children mount on.
	root *orchestrator.BaseOrchestrator
	// sessionID is the effective session ID (manifest namespace).
	sessionID string
	// sess is the parent session (manifest writes, notifications).
	sess *session.Session
	// scheduler runs the restored children in the background with the same
	// parallel/serial machinery, outcome classification, and terminal
	// manifest writes a fresh background spawn gets.
	scheduler *SubagentScheduler
	// clientFor resolves the child's client by agent type (the agent_models
	// routing the fresh-spawn path applies). nil falls back to defaultClient.
	clientFor func(agentType string) *client.Client
	// defaultClient is the subagent client (no agent_models routing).
	defaultClient *client.Client
	// enabledTools, injectCWD, gemmaThinking, maxTurns mirror the fresh-spawn
	// and resume path's prompt/tool switches.
	enabledTools  map[string]bool
	injectCWD     bool
	gemmaThinking bool
	maxTurns      int
	// messenger is the TUI messenger (event delivery, confirmations).
	messenger tui.Messenger
	// runEnv bundles the child executor's startup-scope collaborators (see
	// subagentRunEnv) — launchBackgroundSubagent's run closure reads it.
	runEnv *subagentRunEnv
	// globalBudget is the resolved global subagent budget (0 = unlimited).
	globalBudget time.Duration
}

// isInterruptedSubagentStatus reports whether a manifest status means the
// child was interrupted by a previous late exit: running and queued are the
// raw at-exit states (manifests written before the frozen field existed, or
// writes that never landed), frozen is the explicit post-synthesis status.
// All three are resumable and all three restore into the TUI.
func isInterruptedSubagentStatus(status string) bool {
	return status == session.SubagentStatusRunning ||
		status == session.SubagentStatusQueued ||
		status == session.SubagentStatusFrozen ||
		status == session.SubagentStatusInterrupted
}

// restoreInterruptedSubagents brings every manifest child that was
// interrupted by the previous exit back into the TUI — LIVE, not read-only.
// The synthesized tool result above already made the parent MODEL aware of
// the interruption; this is the TUI-facing half of the same resume story,
// and since the live-resume machinery (agent.NewResumedSubagentOrchestrator,
// the same constructor the spawn_subagent {"resume": "<id>"} path uses) is
// fully available at startup — the client routing, tool surface, messenger,
// and scheduler are all resolved before this runs — the restore no longer
// stops at a read-only projection: a message to the restored child continues
// its task, and the read-only "restored and read-only; re-spawn it" refusal
// is gone for interrupted (non-terminal) records.
//
// The rule mirrors the manifest's crash model: a record still "running"
// (or "queued") when this process reads it — or already flipped to
// "frozen" by the synthesis above — means the previous exit interrupted
// that child, so EVERY such record is restored — whether or not its spawn
// call was dangling in this history (the persisted synthesis makes later
// resumes find no dangling calls, while the manifest keeps the interrupted
// status until a terminal write). Terminal records — completed, failed,
// cancelled — are skipped: their outcome already lives in the parent
// transcript via the persisted tool result or the runner's own summary,
// and the manifest's terminal preview is that same outcome, not a hidden
// conversation. The restored map deduplicates within one resume pass.
//
// Degradation: without the live machinery (a nil deps — only possible in
// tests, since main always builds it when subagents are enabled) or when an
// individual record's history cannot be reloaded, the pre-live-restore
// read-only stub takes over, preserving the Phase 3b visibility guarantee.
// Failures are logged and skipped, never fatal — resume must always
// proceed.
func restoreInterruptedSubagents(root *orchestrator.BaseOrchestrator, manifest *session.SubagentManifest, restored map[string]bool, deps *liveRestoreDeps) {
	if root == nil || manifest == nil {
		return
	}
	// Collect FIRST, feed the scheduler SECOND: the persisted scheduler
	// queue snapshot (Step 4) reorders the relaunch batch so the children
	// resume in the launch order the interrupted run had — serial queue
	// order, then parallel batch membership — instead of manifest order.
	// Manifest order is the manifest write order, which bears no relation
	// to the queue; without the reorder a resume could launch serial child
	// B before serial child A and invert a deliberate sequencing.
	batch := make([]*session.SubagentRecord, 0)
	for i := range manifest.Records {
		rec := &manifest.Records[i]
		if !isInterruptedSubagentStatus(rec.Status) || restored[rec.ID] {
			continue
		}
		batch = append(batch, rec)
	}
	var modesOf map[string]string
	if deps != nil && deps.sessionID != "" {
		batch, modesOf, _ = consumeSchedulerSnapshotForRestore(deps.sessionID, batch)
	}
	for _, rec := range batch {
		if restored[rec.ID] {
			continue // unreachable today; guards future batch builds
		}
		// Slot ownership (Step 6): a "running" record whose PID file still
		// reports an alive owner is a LIVE child of another late process.
		// Resume must not steal its slot — skip live-resume entirely (the
		// owning process lands the terminal status and removes the PID
		// file); a dead/absent owner falls through to the unchanged
		// interrupted path. Frozen/queued records never carried a live
		// owner and always take the interrupted path.
		if deps != nil && deps.sessionID != "" && rec.Status == session.SubagentStatusRunning {
			alive, aliveErr := session.IsSubagentAlive(deps.sessionID, rec.ID)
			if aliveErr != nil {
				common.LogErrorf("subagent-restore", "subagent %s liveness check failed: %v", rec.ID, aliveErr)
			} else if alive {
				common.LogErrorf("subagent-restore", "subagent %s is still owned by a live late process (pid file); leaving its slot untouched", rec.ID)
				restored[rec.ID] = true
				continue
			}
		}
		restored[rec.ID] = true
		if rec.HistoryPath == "" {
			common.LogErrorf("subagent-restore", "subagent %s was interrupted without a persisted history; not listed in the TUI", rec.ID)
			continue
		}
		if deps != nil && deps.root != nil && deps.scheduler != nil && deps.defaultClient != nil && deps.runEnv != nil {
			if restoreInterruptedSubagentLive(deps, rec, restoreLaunchMode(modesOf, rec.ID)) {
				continue
			}
			// Live restore failed for this record: fall through to the
			// read-only projection so the interrupted work stays browsable.
		}
		child, err := agent.NewRestoredSubagentOrchestrator(rec.ID, rec.AgentType, rec.Goal, rec.HistoryPath, RestoredSubagentStatusText(rec.Cause))
		if err != nil {
			common.LogErrorf("subagent-restore", "failed to restore subagent %s: %v", rec.ID, err)
			continue
		}
		root.AddChild(child)
		common.LogErrorf("subagent-restore", "restored subagent %s (%s) into the TUI read-only with %d preserved message(s) (live restore unavailable)", rec.ID, rec.AgentType, len(child.History()))
	}
}

// restoreUnorderedSlot is the sort slot for children absent from the
// scheduler snapshot: they launch after every snapshot-ordered child, in
// manifest order (best-effort ordering, never a drop).
const restoreUnorderedSlot = 1 << 32

// consumeSchedulerSnapshotForRestore folds the persisted scheduler queue
// snapshot into launch order for an interrupted batch and CONSUMES the
// snapshot: the file is removed as soon as its ordering has been captured in
// memory (before any relaunch) so a second restore pass reads no stale
// snapshot, and the scheduler's own persistence hooks immediately write a
// fresh snapshot describing the resumed queue. The reorder is purely
// additive: unknown children keep manifest order at the back and no child is
// ever dropped. A missing snapshot changes nothing; a corrupt one is logged
// and treated as absent. Returns the reordered batch and each child's
// restored scheduling mode ("" when the snapshot says nothing — callers use
// the historical parallel default).
func consumeSchedulerSnapshotForRestore(sessionID string, batch []*session.SubagentRecord) ([]*session.SubagentRecord, map[string]string, error) {
	state, err := session.LoadSchedulerState(sessionID)
	if err != nil {
		// Corrupt snapshot: log-and-treat-as-empty (same degrade rule the
		// scheduler's own persistence follows) — but do NOT delete a file we
		// could not read, so the broken state stays inspectable.
		common.LogErrorf("subagent-restore", "scheduler queue snapshot unreadable; relaunching the batch without the persisted order: %v", err)
		return batch, nil, err
	}
	rendezvous := len(state.SerialQueue) + len(state.ParallelQueue) + len(state.Running)
	orders := make(map[string]int, rendezvous)
	modes := make(map[string]string, rendezvous)
	register := func(id string, order int, mode string) {
		if _, seen := orders[id]; seen || id == "" {
			return // first slot wins; also guarded by SaveSchedulerState's dedupe
		}
		orders[id] = order
		modes[id] = mode
	}
	for _, id := range state.Running {
		// A snapshot-running child keeps the historical live-resume launch
		// (parallel) and is the first relaunch priority: it was mid-work.
		register(id, -1, tool.SubagentExecutionParallel)
	}
	serialSlot := 0
	for _, id := range state.SerialQueue {
		register(id, serialSlot, tool.SubagentExecutionSerial)
		serialSlot++
	}
	parallelSlot := serialSlot // serial queue order always precedes the batch
	for _, id := range state.ParallelQueue {
		register(id, parallelSlot, tool.SubagentExecutionParallel)
		parallelSlot++
	}
	// Stable sort: preserved relative order inside a slot group (manifest
	// order for the unordered tail), preserving the serialized FIFO order.
	sort.SliceStable(batch, func(a, b int) bool {
		slotA, okA := orders[batch[a].ID]
		if !okA {
			slotA = restoreUnorderedSlot
		}
		slotB, okB := orders[batch[b].ID]
		if !okB {
			slotB = restoreUnorderedSlot
		}
		return slotA < slotB
	})
	modesFor := modes
	// Consume: the snapshot's whole purpose is this relaunch batch's order;
	// delete it (best-effort, atomic-delete style — remove and tolerate a
	// missing file) so later passes never reorder against a stale snapshot.
	if path, pathErr := session.SchedulerStatePath(sessionID); pathErr != nil {
		common.LogErrorf("subagent-restore", "scheduler queue snapshot cannot be resolved for removal: %v", pathErr)
	} else if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		common.LogErrorf("subagent-restore", "failed to remove consumed scheduler queue snapshot: %v", rmErr)
	}
	return batch, modesFor, nil
}

// restoreLaunchMode maps a child's snapshot slot to the scheduling mode the
// restore relaunch uses: serial-queued children relaunch serial (their
// persisted position in the serial FIFO is only meaningful under serial
// scheduling — parallel would shatter the sequencing the snapshot
// preserves); everything else (snapshot running, snapshot-parallel,
// absent-from-snapshot) keeps the historical parallel relaunch.
func restoreLaunchMode(modes map[string]string, childID string) string {
	if mode, ok := modes[childID]; ok && mode != "" {
		return mode
	}
	return tool.SubagentExecutionParallel
}

// restoreInterruptedSubagentLive relaunches ONE interrupted child in the
// background through the exact {"resume": "<id>"} machinery: the resume
// constructor (same ID, full persisted history, complete tool surface, no
// goal re-append — the loaded history already opens with the original goal),
// then the scheduler-backed launch (stall watchdog, outcome classification,
// terminal manifest write, parent notification, result artifact). The child
// appears in the TUI via the constructor's own AddChild and continues its
// task from where the interruption stopped; the synthesized tool result the
// parent already received points at the same {"resume": "<id>"} flow, which
// now finds the record terminal-or-running and degrades to the friendly
// no-op / live-status wording.
//
// The record's status flips back to "running" through the constructor's
// MarkResumed write — the same crash-safe semantics as an explicit resume:
// a crash mid-relaunch reads as interrupted again. Returns false when the
// live relaunch is impossible for this record (validation failure, empty
// history, constructor error) and the read-only fallback should take over.
// mode is the EFFECTIVE scheduling mode this relaunch uses: the persisted
// scheduler snapshot slots serial-queued children as serial so their FIFO
// position means something; every other slot relaunches parallel (the
// historical restore behavior). See restoreLaunchMode.
func restoreInterruptedSubagentLive(deps *liveRestoreDeps, rec *session.SubagentRecord, mode string) bool {
	if rec.ID == "" {
		return false
	}
	if err := validateResumeRecord(rec); err != nil {
		common.LogErrorf("subagent-restore", "subagent %s cannot be live-restored: %v", rec.ID, err)
		return false
	}

	childClient := deps.defaultClient
	if deps.clientFor != nil {
		if routed := deps.clientFor(rec.AgentType); routed != nil {
			childClient = routed
		}
	}

	child, _, err := agent.NewResumedSubagentOrchestrator(
		childClient, *rec, rec.AgentType, deps.enabledTools, deps.injectCWD,
		deps.gemmaThinking, deps.maxTurns, deps.root, deps.messenger)
	if err != nil {
		common.LogErrorf("subagent-restore", "live restore of subagent %s failed: %v", rec.ID, err)
		return false
	}

	// Perfect resume (tool-call spool): surface the shell-call transcripts
	// the previous exit left in the shared tool-outputs dir as harness notes
	// in the child's LOADED history, so its first resumed turn reads the
	// partial output of its interrupted calls instead of blindly retrying.
	// Runs before the background relaunch, i.e. before the child's next
	// request is ever rendered.
	synthesizeSpoolTranscriptNotes(child, spoolDirForHistoryPath(rec.HistoryPath))

	// Status surface: the resumed child's TUI tab seeds with "resumed from
	// interruption" (agent.RestoredResumedStatus) when first focused, and
	// the run's own status takes over from the first turn.
	effective := tool.SubagentExecutionParallel
	if mode == tool.SubagentExecutionSerial {
		effective = tool.SubagentExecutionSerial
	}
	scheduling := subagentScheduling{
		Requested: effective,
		Effective: effective,
		ModelRef:  "",
	}
	if _, err := launchBackgroundSubagent(deps.scheduler, deps.runEnv, deps.sess, deps.sessionID, child,
		rec.AgentType, rec.Goal, "", context.Background(), nil, deps.globalBudget, scheduling); err != nil {
		common.LogErrorf("subagent-restore", "background relaunch of restored subagent %s failed: %v", rec.ID, err)
		return false
	}
	common.LogErrorf("subagent-restore", "live-restored subagent %s (%s) — resumed in the background with %d preserved message(s)", rec.ID, rec.AgentType, len(child.History()))
	return true
}

// RestoredSubagentStatusText renders the status line for a restored record:
// the generic interrupted wording, with the cause appended when the manifest
// recorded one.
func RestoredSubagentStatusText(cause string) string {
	if cause == "" {
		return agent.RestoredSubagentStatus
	}
	return agent.RestoredSubagentStatus + " (" + cause + ")"
}

// spawnArgs is the slice of the spawn_subagent arguments the correlation
// needs. Argument parsing is best-effort: a malformed body (the model wrote
// it, after all) simply matches nothing.
type spawnArgs struct {
	Goal      string `json:"goal"`
	AgentType string `json:"agent_type"`
}

// matchSpawnRecord picks the manifest record for one dangling spawn call.
// Within each precision tier a still-running record is preferred (the
// interrupted-spawn case); a terminal record matches only when no running
// one fits (the result-lost race: the crash landed between the runner's
// return and the parent's result persistence). Unmatched calls fall back to
// the generic wording — nil return.
func matchSpawnRecord(manifest *session.SubagentManifest, tc client.ToolCall, consumed map[*session.SubagentRecord]bool) *session.SubagentRecord {
	var args spawnArgs
	_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)

	isFree := func(rec *session.SubagentRecord) bool {
		return rec != nil && !consumed[rec]
	}
	// best returns the first free record satisfying pred, or nil.
	best := func(pred func(*session.SubagentRecord) bool) *session.SubagentRecord {
		for i := range manifest.Records {
			rec := &manifest.Records[i]
			if isFree(rec) && pred(rec) {
				return rec
			}
		}
		return nil
	}
	running := func(rec *session.SubagentRecord) bool { return isInterruptedSubagentStatus(rec.Status) }
	matchesType := func(rec *session.SubagentRecord) bool {
		return args.AgentType == "" || rec.AgentType == args.AgentType
	}
	matchesGoal := func(rec *session.SubagentRecord) bool {
		return args.Goal == "" || rec.Goal == args.Goal
	}
	// tier applies pred with running records preferred over terminal ones.
	tier := func(core func(*session.SubagentRecord) bool) *session.SubagentRecord {
		if rec := best(func(rec *session.SubagentRecord) bool { return core(rec) && running(rec) }); rec != nil {
			return rec
		}
		return best(core)
	}

	// Exact tier (Step 8): a record explicitly bound to THIS tool call wins
	// before any argument matching. A record bound to a DIFFERENT call never
	// matches here — it stays eligible for the descend heuristic only, which
	// is today's behavior (two same-goal spawns whose ids match different
	// records resolve by arguments exactly as they did before the field).
	if rec := tier(func(rec *session.SubagentRecord) bool { return rec.SpawnCallID != "" && rec.SpawnCallID == tc.ID }); rec != nil {
		return rec
	}

	if rec := tier(func(rec *session.SubagentRecord) bool { return matchesType(rec) && matchesGoal(rec) }); rec != nil {
		return rec
	}
	if rec := tier(matchesType); rec != nil {
		return rec
	}
	if rec := tier(matchesGoal); rec != nil {
		return rec
	}
	return tier(func(*session.SubagentRecord) bool { return true })
}

// manifestInterruptedText builds the tool-result string for one dangling
// spawn call from the record's status: running means interrupted by the
// previous exit — the directive live-resume wording, pointing the parent at
// the exact spawn_subagent {"resume": "<id>"} call; completed means the
// result preview survived but the parent's copy was lost in the same exit
// (rare race); failed/cancelled reuse the runner's cause classification
// with a review-the-transcript pointer (failed work may optionally be
// resumed too, now that live resume exists); no record at all falls back to
// the generic interrupted wording.
func manifestInterruptedText(manifest *session.SubagentManifest, tc client.ToolCall, record *session.SubagentRecord) string {
	if record == nil {
		// No manifest record matched the dangling call: there is no child
		// ID or history to vouch for, so use the generic fallback (the
		// tool-call ID is not a resume ID, and nothing here may claim
		// preserved work state).
		return danglingSubagentInterruptedText("", "", "", "")
	}
	switch record.Status {
	case session.SubagentStatusRunning:
		return danglingSubagentInterruptedText(record.ID, record.AgentType, record.HistoryPath, record.TranscriptPath)
	case session.SubagentStatusCompleted:
		text := fmt.Sprintf("The %s subagent completed its task, but its result was lost from this session's history by the same late exit.", record.AgentType)
		if record.ResultPreview != "" {
			text += fmt.Sprintf(" Preserved result preview:\n%s", record.ResultPreview)
		}
		return text
	case session.SubagentStatusFailed:
		text := fmt.Sprintf("The %s subagent failed in a previous session (%s).", record.AgentType, record.Cause)
		if record.TranscriptPath != "" {
			text += fmt.Sprintf(" Review the transcript at %s before deciding.", record.TranscriptPath)
		}
		text += " If the failure was transient or the work is close to done, you may call the spawn_subagent tool with {\"resume\": \"" + record.ID + "\"} to continue it; otherwise spawn a fresh agent."
		return text
	case session.SubagentStatusCancelled:
		return fmt.Sprintf("The %s subagent was cancelled in a previous session (%s). Its work state is preserved at %s; call the spawn_subagent tool with {\"resume\": \"%s\"} if it should continue, or spawn a fresh agent for a clean restart.", record.AgentType, record.Cause, record.HistoryPath, record.ID)
	default:
		return danglingSubagentInterruptedText(record.ID, record.AgentType, record.HistoryPath, record.TranscriptPath)
	}
}

// subagentHistoryPersisted reports whether a child's persisted history file
// exists on disk at notify time. An empty path (history persistence was
// disabled for that run) or a missing file means the child's in-flight work
// was NOT preserved — the manifest record alone is not work state.
func subagentHistoryPersisted(historyPath string) bool {
	if historyPath == "" {
		return false
	}
	info, err := os.Stat(historyPath)
	return err == nil && info.Mode().IsRegular()
}

// danglingSubagentInterruptedText is the model-facing wording for a spawn
// call interrupted by a previous late exit. It is DIRECTIVE (Phase C live
// resume): the parent is told exactly how to restore the child — the
// spawn_subagent {"resume": "<id>"} call — and warned not to re-state the
// goal or spawn a replacement, because the preserved history is the child's
// full state and resuming it continues the task where it stopped.
//
// The directive wording is conditional on the child's history file existing
// at notify time: when persistence was disabled for that run (or the file
// vanished), advertising a resume can only fail with "no persisted
// history", so the wording tells the parent to re-dispatch instead.
func danglingSubagentInterruptedText(id, agentType, historyPath, transcriptPath string) string {
	if id == "" {
		// No manifest record matched the dangling call: the generic
		// fallback keeps the historical review-the-transcript shape (the
		// tool-call ID is not a resume ID).
		text := "This subagent was interrupted by a previous late exit before completing."
		if subagentHistoryPersisted(historyPath) {
			text += fmt.Sprintf(" Its work up to the interruption is preserved at %s", historyPath)
		} else {
			text += " Its work up to the interruption could not be preserved"
		}
		if transcriptPath != "" {
			text += fmt.Sprintf(" (and transcript %s)", transcriptPath)
		}
		text += ". Review it and continue or re-spawn as needed."
		return text
	}
	if !subagentHistoryPersisted(historyPath) {
		// Honesty fix (resume incident): a child whose history file does
		// not exist cannot be resumed — resume fails with "no persisted
		// history". Name the real cause and re-dispatch instead of
		// advertising preserved work that is not there.
		return fmt.Sprintf("Subagent %s (%s) was interrupted; subagent history persistence was DISABLED for this session (enable save-subagent-histories or update late) — its in-flight work could not be preserved; re-dispatch the mission.", id, agentType)
	}
	text := fmt.Sprintf("Subagent %s (%s) was interrupted by a previous late exit. Its full work state is preserved at %s", id, agentType, historyPath)
	if transcriptPath != "" {
		text += fmt.Sprintf(" (transcript: %s)", transcriptPath)
	}
	text += fmt.Sprintf(". To resume it exactly where it stopped, call the spawn_subagent tool with {\"resume\": \"%s\"} (all other parameters are ignored; the agent restores with its complete history and continues its task). Do NOT re-state the goal or spawn a new agent for this work unless resume fails.", id)
	return text
}

// spoolResumeNote is the model-facing wording injected into a restored
// child's history for every shell-call transcript the previous exit left
// behind. It reuses the live failure wording (the same sentence a still-
// running session sees from ShellTool.Execute) anchored to this process's
// exit: the call was interrupted, its partial output survived on disk, and
// reading it is how the child decides between continuing and retrying.
const spoolResumeNote = "tool call failed: interrupted by a previous late exit. " +
	"The transcript of its execution was preserved here — read it to decide whether to retry the tool call: %s"

// orphanedSpoolTranscripts lists the shell-call transcripts the previous
// exit left in dir: partial-*.txt files, ordered by name (call IDs embed
// Unix-nano start times, so name order is chronological). A missing or
// unreadable dir yields nil — spooling is fail-open end to end.
func orphanedSpoolTranscripts(dir string) []string {
	if dir == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, "partial-*.txt"))
	if err != nil || len(matches) == 0 {
		return nil
	}
	sort.Strings(matches)
	return matches
}

// consumeSpoolTranscript marks one transcript consumed by renaming it to
// <name>.consumed — same-directory, same filesystem, atomic. A benign
// no-op on failure: a consumed marker that failed to stick only means the
// next resume would re-surface the same transcript.
func consumeSpoolTranscript(path string) {
	_ = os.Rename(path, path+".consumed")
}

// synthesizeSpoolTranscriptNotes injects one note per orphaned shell-call
// transcript into the child's LOADED history, before its first turn runs:
// the restored model reads what its interrupted call actually printed and
// decides whether to retry it. Files are consumed by renaming to
// .consumed, so exactly one resume ever surfaces a given transcript.
//
// Message shape: a plain USER message carrying the [late harness] prefix —
// the same attribution every other harness-injected history note uses (see
// the executor's coder failure note). NOT a role-"tool" message: without a
// ToolCallID matching an assistant tool_call in the loaded history,
// SanitizeForRequest would drop it as a dangling tool result before the
// request ever left the process.
func synthesizeSpoolTranscriptNotes(child common.Orchestrator, spoolDir string) int {
	if child == nil || spoolDir == "" {
		return 0
	}
	base, ok := child.(*orchestrator.BaseOrchestrator)
	if !ok || base == nil {
		return 0
	}
	sess := base.Session()
	if sess == nil {
		return 0
	}
	orphans := orphanedSpoolTranscripts(spoolDir)
	if len(orphans) == 0 {
		return 0
	}
	noted := 0
	for _, path := range orphans {
		note := "[late harness] " + fmt.Sprintf(spoolResumeNote, path)
		if err := sess.AddMessage(client.ChatMessage{Role: "user", Content: client.TextContent(note)}); err != nil {
			common.LogErrorf("subagent-restore", "failed to append spool note for %s to child %s history: %v", path, child.ID(), err)
			continue
		}
		consumeSpoolTranscript(path)
		noted++
	}
	if noted > 0 {
		common.LogErrorf("subagent-restore", "resume injected %d preserved shell-call transcript(s) into subagent %s history", noted, child.ID())
	}
	return noted
}

// spoolDirForHistoryPath maps a child's persisted history path to the
// shared spool directory: every artifact of one session lives in one
// tool-outputs folder — <dir-of-history>/../tool-outputs. Empty (in-memory
// sessions, disabled persistence) disables the synthesis.
func spoolDirForHistoryPath(historyPath string) string {
	if strings.TrimSpace(historyPath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(historyPath), "..", "tool-outputs")
}
