package main

import (
	"encoding/json"
	"fmt"
	"os"

	"late/internal/client"
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
func synthesizeDanglingSpawnResults(sess *session.Session) error {
	effectiveSessionID := deriveEffectiveSessionID(sess.HistoryPath)
	if effectiveSessionID == "" {
		return nil // no session folder: nothing was ever persisted
	}
	manifestPath, err := session.SubagentManifestPath(effectiveSessionID)
	if err != nil {
		// Unsafe IDs can never have produced a folder; nothing to do.
		return nil
	}
	if _, statErr := os.Stat(manifestPath); os.IsNotExist(statErr) {
		return nil // no manifest: no subagent ever spawned in this session
	}
	manifest, err := session.LoadSubagentManifest(effectiveSessionID)
	if err != nil {
		// A corrupt manifest must not make the session unresumable: the
		// request-time sanitizer still closes the dangling calls.
		logSubagentErrorf("subagent-manifest: failed to load manifest for %s: %v", effectiveSessionID, err)
		return nil
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
				return fmt.Errorf("persist synthesized result for %s: %w", tc.ID, err)
			}
			answeredIDs[tc.ID] = true
			synthesized++
		}
	}

	if synthesized > 0 {
		logSubagentErrorf("subagent-manifest: resume synthesized %d tool result(s) for dangling spawn_subagent call(s) in session %s", synthesized, effectiveSessionID)
	}
	return nil
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
	running := func(rec *session.SubagentRecord) bool { return rec.Status == session.SubagentStatusRunning }
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
// previous exit (work preserved at the recorded paths); completed means the
// result preview survived but the parent's copy was lost in the same exit
// (rare race); failed/cancelled reuse the runner's cause classification;
// no record at all falls back to the generic interrupted wording.
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
		return fmt.Sprintf("The %s subagent failed in a previous session (%s) and cannot be resumed.", record.AgentType, record.Cause)
	case session.SubagentStatusCancelled:
		return fmt.Sprintf("The %s subagent was cancelled in a previous session (%s).", record.AgentType, record.Cause)
	default:
		return danglingSubagentInterruptedText(record.ID, record.AgentType, record.HistoryPath, record.TranscriptPath)
	}
}

// danglingSubagentInterruptedText is the model-facing wording for a spawn
// call interrupted by a previous late exit. It points at the preserved work
// (history file, and transcript when the child ended abnormally) so the
// parent can review it and continue or re-spawn as needed.
func danglingSubagentInterruptedText(id, agentType, historyPath, transcriptPath string) string {
	text := fmt.Sprintf("Subagent %s was interrupted by a previous late exit before completing.", id)
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
