package main

import (
	"encoding/json"
	"fmt"
	"os"

	"late/internal/agent"
	"late/internal/client"
	"late/internal/orchestrator"
	"late/internal/session"
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
// calls by tool-call ID — two different namespaces with no stored link (the
// runner never sees the tool-call ID). A dangling call is therefore matched
// to an unconsumed record by its spawn arguments (agent_type, then goal):
// every spawn creates exactly one record, so this is exact except for
// repeated identical (type, goal) pairs, where the wording is equally true
// of either record. Unmatched calls fall back to the generic interrupted
// wording. The synthesis runs only when the manifest exists; one summary
// line is logged to stderr per invocation.
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
		logSubagentErrorf("subagent-manifest: failed to load manifest for %s: %v", effectiveSessionID, err)
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
		logSubagentErrorf("subagent-manifest: resume synthesized %d tool result(s) for dangling spawn_subagent call(s) in session %s", synthesized, effectiveSessionID)
	}
	return manifest, nil
}

// restoreInterruptedSubagents re-lists every manifest child that was
// interrupted by the previous exit (Phase 3b of subagent persistence) as a
// read-only orchestrator on the root, so the TUI can browse it — tab
// switching, transcript view — like any other historical child. The
// synthesized tool result above already made the parent MODEL aware; this is
// the TUI-facing half of the same resume story.
//
// The rule mirrors the manifest's crash model: a record still "running" when
// this process reads it means the previous exit interrupted that child, so
// EVERY such record is restored — whether or not its spawn call was dangling
// in this history (the persisted synthesis makes later resumes find no
// dangling calls, while the manifest keeps saying "running" until a
// terminal write). Terminal records — completed, failed, cancelled — are
// skipped: their outcome already lives in the parent transcript via the
// persisted tool result or the runner's own summary, and the manifest's
// terminal preview is that same outcome, not a hidden conversation. The
// restored map deduplicates within one resume pass.
//
// Read-only by construction (agent.NewRestoredSubagentOrchestrator): no
// session, empty registry, Execute refuses. Failures are logged and skipped,
// never fatal — a broken history file degrades the TUI listing only, and
// resume must always proceed.
func restoreInterruptedSubagents(root *orchestrator.BaseOrchestrator, manifest *session.SubagentManifest, restored map[string]bool) {
	if root == nil || manifest == nil {
		return
	}
	for i := range manifest.Records {
		rec := &manifest.Records[i]
		if rec.Status != session.SubagentStatusRunning || restored[rec.ID] {
			continue
		}
		restored[rec.ID] = true
		if rec.HistoryPath == "" {
			logSubagentErrorf("subagent-restore: subagent %s was interrupted without a persisted history; not listed in the TUI", rec.ID)
			continue
		}
		child, err := agent.NewRestoredSubagentOrchestrator(rec.ID, rec.AgentType, rec.Goal, rec.HistoryPath, RestoredSubagentStatusText(rec.Cause))
		if err != nil {
			logSubagentErrorf("subagent-restore: failed to restore subagent %s: %v", rec.ID, err)
			continue
		}
		root.AddChild(child)
		logSubagentErrorf("subagent-restore: restored subagent %s (%s) into the TUI with %d preserved message(s)", rec.ID, rec.AgentType, len(child.History()))
	}
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
// return and the parent's result persistence). When no tier matches on
// concrete arguments (all tiers empty or contradictory), the final tier
// falls back to ANY unconsumed record so the parent still gets the
// manifest's real state for its dangling call: the wording is equally true
// of whichever interrupted child it names, and the correlation is
// best-effort by design — spawn arguments and manifest records share no
// stored link. Unmatched calls (empty manifest, everything consumed) fall
// back to the generic interrupted wording — nil return.
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
	running := func(rec *session.SubagentRecord) bool { return rec.Status == session.SubagentStatusRunning }
	matchesType := func(rec *session.SubagentRecord) bool {
		// An empty arg is a wildcard (the model omitted agent_type); a
		// non-empty one must AGREE with the record — a "researcher" spawn
		// call must not be correlated with a "coder" record, whose
		// directive resume wording would point the parent at the wrong
		// child.
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

	if rec := tier(func(rec *session.SubagentRecord) bool { return matchesType(rec) && matchesGoal(rec) }); rec != nil {
		return rec
	}
	if rec := tier(matchesType); rec != nil {
		return rec
	}
	// Goal-only tier still requires the agent type to agree (a typed call
	// must never bind a different type's record — a wrong-type directive
	// resume wording would point the parent at the wrong child); an
	// untyped call matches by goal alone.
	if rec := tier(func(rec *session.SubagentRecord) bool { return matchesType(rec) && args.Goal != "" && matchesGoal(rec) }); rec != nil {
		return rec
	}
	// Last resort: any unconsumed record. The agent-type tiers above stay
	// strict (never cross-type), so the catch-all still cannot bind a spawn
	// call to a record of a DIFFERENT type — it only fills in when the call
	// named no type at all, and the wording remains true of the child it
	// names.
	if rec := tier(func(*session.SubagentRecord) bool { return args.AgentType == "" }); rec != nil {
		return rec
	}
	return nil
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
		return danglingSubagentInterruptedText(tc.ID, "", "", "")
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

// danglingSubagentInterruptedText is the model-facing wording for a spawn
// call interrupted by a previous late exit. It is DIRECTIVE (Phase C live
// resume): the parent is told exactly how to restore the child — the
// spawn_subagent {"resume": "<id>"} call — and warned not to re-state the
// goal or spawn a replacement, because the preserved history is the child's
// full state and resuming it continues the task where it stopped.
func danglingSubagentInterruptedText(id, agentType, historyPath, transcriptPath string) string {
	if id == "" {
		// No manifest record matched the dangling call: the generic
		// fallback keeps the historical review-the-transcript shape (the
		// tool-call ID is not a resume ID).
		text := "This subagent was interrupted by a previous late exit before completing."
		if historyPath != "" {
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
	text := fmt.Sprintf("Subagent %s (%s) was interrupted by a previous late exit. Its full work state is preserved", id, agentType)
	if historyPath != "" {
		text += fmt.Sprintf(" at %s", historyPath)
	} else {
		text += " in the session manifest"
	}
	if transcriptPath != "" {
		text += fmt.Sprintf(" (transcript: %s)", transcriptPath)
	}
	text += fmt.Sprintf(". To resume it exactly where it stopped, call the spawn_subagent tool with {\"resume\": \"%s\"} (all other parameters are ignored; the agent restores with its complete history and continues its task). Do NOT re-state the goal or spawn a new agent for this work unless resume fails.", id)
	return text
}
