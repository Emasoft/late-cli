package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"late/internal/client"
	"late/internal/session"
)

// TestDanglingSubagentInterruptedTextDirective pins the Phase C directive
// wording for interrupted children: the exact spawn_subagent resume JSON
// snippet, the ignored-parameters note, and the do-not-respawn warning.
func TestDanglingSubagentInterruptedTextDirective(t *testing.T) {
	got := danglingSubagentInterruptedText("coder-subagent-0", "coder", "/s/subagents/coder-subagent-0.json", "/t/transcript.md")
	for _, want := range []string{
		"Subagent coder-subagent-0 (coder) was interrupted by a previous late exit",
		"Its full work state is preserved",
		`{"resume": "coder-subagent-0"}`,
		"all other parameters are ignored",
		"restores with its complete history and continues its task",
		"Do NOT re-state the goal or spawn a new agent for this work unless resume fails",
		"/s/subagents/coder-subagent-0.json",
		"/t/transcript.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("directive wording missing %q:\n%s", want, got)
		}
	}
}

// TestDanglingSubagentInterruptedTextWithoutHistory pins the no-history
// variant: the record ID is still resumable (resume surfaces the
// missing-history error), so the directive call stays present.
func TestDanglingSubagentInterruptedTextWithoutHistory(t *testing.T) {
	got := danglingSubagentInterruptedText("coder-subagent-1", "researcher", "", "")
	if !strings.Contains(got, `{"resume": "coder-subagent-1"}`) {
		t.Errorf("resume call missing from the no-history wording: %s", got)
	}
	if strings.Contains(got, "transcript") {
		t.Errorf("no transcript should be mentioned: %s", got)
	}
}

// TestDanglingSubagentInterruptedTextUnknownCallID pins the generic
// fallback: with no manifest record there is no resume ID to advertise, so
// the wording keeps the historical review-and-continue shape.
func TestDanglingSubagentInterruptedTextUnknownCallID(t *testing.T) {
	got := danglingSubagentInterruptedText("", "", "", "")
	if strings.Contains(got, `"resume"`) {
		t.Errorf("generic fallback must not advertise a resume call: %s", got)
	}
	if !strings.Contains(got, "Review it and continue or re-spawn as needed.") {
		t.Errorf("generic fallback lost the review wording: %s", got)
	}
}

// TestTerminalResumeError pins the model-facing error for terminal records.
func TestTerminalResumeError(t *testing.T) {
	completed := terminalResumeError(&session.SubagentRecord{ID: "a-1", Status: session.SubagentStatusCompleted})
	if !strings.Contains(completed.Error(), "agent a-1 already terminated (completed)") ||
		!strings.Contains(completed.Error(), "spawn a fresh agent instead") {
		t.Errorf("completed error = %v", completed)
	}

	failed := terminalResumeError(&session.SubagentRecord{ID: "a-2", Status: session.SubagentStatusFailed, Cause: "crashed: boom"})
	if !strings.Contains(failed.Error(), "agent a-2 already terminated (crashed: boom)") {
		t.Errorf("failed error = %v", failed)
	}

	// A failed record without a cause falls back to the status name.
	noCause := terminalResumeError(&session.SubagentRecord{ID: "a-3", Status: session.SubagentStatusFailed})
	if !strings.Contains(noCause.Error(), "agent a-3 already terminated (failed)") {
		t.Errorf("no-cause error = %v", noCause)
	}
}

// TestValidateResumeRecord pins the resumability gate: only a
// running-at-read (interrupted) record WITH a persisted history passes.
func TestValidateResumeRecord(t *testing.T) {
	interrupted := &session.SubagentRecord{ID: "a-1", Status: session.SubagentStatusRunning, HistoryPath: "/s/a-1.json"}
	if err := validateResumeRecord(interrupted); err != nil {
		t.Errorf("interrupted record with history = %v, want nil", err)
	}

	noHistory := &session.SubagentRecord{ID: "a-2", Status: session.SubagentStatusRunning}
	if err := validateResumeRecord(noHistory); err == nil || !strings.Contains(err.Error(), "no persisted history") {
		t.Errorf("interrupted record without history = %v, want the no-history error", err)
	}

	for status, name := range map[string]string{
		session.SubagentStatusCompleted: "completed",
		session.SubagentStatusFailed:    "failed",
		session.SubagentStatusCancelled: "cancelled",
	} {
		rec := &session.SubagentRecord{ID: "a-x", Status: status, HistoryPath: "/s/a-x.json"}
		if err := validateResumeRecord(rec); err == nil || !strings.Contains(err.Error(), "already terminated") {
			t.Errorf("%s record = %v, want the terminated error", name, err)
		}
	}

	unknown := &session.SubagentRecord{ID: "a-9", Status: "banana", HistoryPath: "/s/a-9.json"}
	if err := validateResumeRecord(unknown); err == nil || !strings.Contains(err.Error(), "unknown manifest status") {
		t.Errorf("unknown status = %v, want the unknown-status error", err)
	}
}

// TestResumeWorktreeWordingThroughSynthesis runs a worktree child's
// interrupted wording through the full synthesis path: the resumed record
// (carrying the Phase C fields) produces the directive resume call for the
// preserved child.
func TestResumeWorktreeWordingThroughSynthesis(t *testing.T) {
	const sessionID = "session-phasec-worktree-wording"
	sess := runnerTestSession(t, sessionID)

	if err := sess.AddAssistantMessageWithTools("", "", []client.ToolCall{{
		Index: 0, ID: "call_wt", Type: "function",
		Function: client.FunctionCall{Name: spawnSubagentToolName, Arguments: `{"goal":"port","agent_type":"coder"}`},
	}}); err != nil {
		t.Fatalf("AddAssistantMessageWithTools: %v", err)
	}

	worktree := filepath.Join(t.TempDir(), "repo-worktrees", "feature-x")
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: "coder-subagent-12", AgentType: "coder", Goal: "port",
		Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Hour),
		HistoryPath: "/s/coder-subagent-12.json", WorktreePath: worktree, WorkingDir: "/repo",
		ResumeCount: 1,
	}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}

	saved, err := session.LoadHistory(sess.HistoryPath)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	resumed := session.New(nil, sess.HistoryPath, saved, "prompt", false)
	if _, err := synthesizeDanglingSpawnResults(resumed); err != nil {
		t.Fatalf("synthesizeDanglingSpawnResults: %v", err)
	}
	var text string
	for _, m := range resumed.History {
		if m.Role == "tool" && m.ToolCallID == "call_wt" {
			text = m.Content.String()
		}
	}
	if text == "" {
		t.Fatal("no synthesized tool result for the dangling call")
	}
	for _, want := range []string{
		`{"resume": "coder-subagent-12"}`,
		"all other parameters are ignored",
		"Do NOT re-state the goal",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("synthesized wording missing %q:\n%s", want, text)
		}
	}
}

// TestSynthesisUnmatchedCallNeverAdvertisesToolCallID runs two dangling
// spawn calls through the full synthesis path with only ONE manifest record:
// the first call consumes it; the second is unmatched (a typed call with no
// same-type record falls through every tier). The unmatched wording must be
// the id-less generic one — the record-less branch used to pass the
// TOOL-CALL ID into the directive, advertising
// spawn_subagent {"resume": "<tc.ID>"} with an id that lives in a different
// namespace than the manifest's child IDs and could never resolve.
func TestSynthesisUnmatchedCallNeverAdvertisesToolCallID(t *testing.T) {
	const sessionID = "session-phasec-unmatched-wording"
	sess := runnerTestSession(t, sessionID)

	if err := sess.AddAssistantMessageWithTools("", "", []client.ToolCall{
		{Index: 0, ID: "call_first", Type: "function",
			Function: client.FunctionCall{Name: spawnSubagentToolName, Arguments: `{"goal":"port the module","agent_type":"coder"}`}},
		{Index: 1, ID: "call_second", Type: "function",
			Function: client.FunctionCall{Name: spawnSubagentToolName, Arguments: `{"goal":"fix the flaky test","agent_type":"coder"}`}},
	}); err != nil {
		t.Fatalf("AddAssistantMessageWithTools: %v", err)
	}

	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: "coder-subagent-7", AgentType: "coder", Goal: "port the module",
		Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Hour),
		HistoryPath: "/s/coder-subagent-7.json",
	}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}

	saved, err := session.LoadHistory(sess.HistoryPath)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	resumed := session.New(nil, sess.HistoryPath, saved, "prompt", false)
	if _, err := synthesizeDanglingSpawnResults(resumed); err != nil {
		t.Fatalf("synthesizeDanglingSpawnResults: %v", err)
	}
	results := map[string]string{}
	for _, m := range resumed.History {
		if m.Role == "tool" {
			results[m.ToolCallID] = m.Content.String()
		}
	}
	if results["call_first"] == "" || results["call_second"] == "" {
		t.Fatalf("both dangling calls must be synthesized: %q / %q", results["call_first"], results["call_second"])
	}
	// The matched call carries the directive; the unmatched call must not
	// advertise ANY resume id — least of all its tool-call ID.
	if !strings.Contains(results["call_first"], `{"resume": "coder-subagent-7"}`) {
		t.Errorf("matched call lost the resume directive:\n%s", results["call_first"])
	}
	if strings.Contains(results["call_second"], `"resume"`) {
		t.Errorf("unmatched call must not advertise a resume call (its tool-call ID can never resolve):\n%s", results["call_second"])
	}
	if !strings.Contains(results["call_second"], "Review it and continue or re-spawn as needed.") {
		t.Errorf("unmatched call lost the generic review wording:\n%s", results["call_second"])
	}
}

// TestMatchSpawnRecordNeverCrossesAgentType pins the correlation guard: a
// spawn call that names an agent_type must never be matched to a record of
// a DIFFERENT type — the directive wording names the record's resume ID,
// and a wrong-type correlation would advertise resuming the wrong child.
// An untyped call may still bind any record (the args are a wildcard).
func TestMatchSpawnRecordNeverCrossesAgentType(t *testing.T) {
	records := []session.SubagentRecord{
		{ID: "coder-subagent-20", AgentType: "coder", Goal: "port the parser", Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Hour)},
		{ID: "researcher-subagent-21", AgentType: "researcher", Goal: "survey options", Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Hour)},
	}
	manifest := &session.SubagentManifest{Records: records}

	// A call naming a type that has NO record (e.g. the spawn used a type
	// that was never persisted, or all its records are consumed) must NOT
	// grab a record of a DIFFERENT type — the old catch-all tier bound
	// exactly that wrong record and advertised resuming the wrong child.
	tc := client.ToolCall{ID: "call_r", Function: client.FunctionCall{
		Name: spawnSubagentToolName, Arguments: `{"goal":"entirely unrelated","agent_type":"tester"}`,
	}}
	if rec := matchSpawnRecord(manifest, tc, map[*session.SubagentRecord]bool{}); rec != nil {
		t.Errorf("typed call with no matching record bound %s (%s), want nil (generic wording)", rec.ID, rec.AgentType)
	}

	// A call whose goal matches nothing still binds its OWN type's record
	// (the type-only tier: spawn args may paraphrase the goal, but the
	// type is a strong signal).
	tcDrift := client.ToolCall{ID: "call_d", Function: client.FunctionCall{
		Name: spawnSubagentToolName, Arguments: `{"goal":"entirely unrelated","agent_type":"researcher"}`,
	}}
	if rec := matchSpawnRecord(manifest, tcDrift, map[*session.SubagentRecord]bool{}); rec == nil || rec.AgentType != "researcher" {
		t.Errorf("goal-drifted call bound %+v, want its own researcher record", rec)
	}

	// An untyped call still falls back to any record (backward compat).
	tcUntyped := client.ToolCall{ID: "call_u", Function: client.FunctionCall{
		Name: spawnSubagentToolName, Arguments: `{}`,
	}}
	if rec := matchSpawnRecord(manifest, tcUntyped, map[*session.SubagentRecord]bool{}); rec == nil {
		t.Error("untyped call with no matching record must still bind a record (wildcard args)")
	}

	// The typed call whose type matches binds its own record.
	tcCoder := client.ToolCall{ID: "call_c", Function: client.FunctionCall{
		Name: spawnSubagentToolName, Arguments: `{"goal":"port the parser","agent_type":"coder"}`,
	}}
	if rec := matchSpawnRecord(manifest, tcCoder, map[*session.SubagentRecord]bool{}); rec == nil || rec.ID != "coder-subagent-20" {
		t.Errorf("typed call bound %+v, want coder-subagent-20", rec)
	}
}
