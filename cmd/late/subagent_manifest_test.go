package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"late/internal/client"
	"late/internal/session"
)

// nowMinus returns time.Now() minus d, second-truncated for stable
// round-trips through the JSON manifest.
func nowMinus(t *testing.T, d time.Duration) time.Time {
	t.Helper()
	return time.Now().Add(-d).Truncate(time.Second)
}

// stubRunnerChild stands in for the *orchestrator.BaseOrchestrator the
// runner classifies: only the surfaces the manifest write path touches.
type stubRunnerChild struct {
	id      string
	history []client.ChatMessage
	sess    *session.Session
}

func (s *stubRunnerChild) ID() string                    { return s.id }
func (s *stubRunnerChild) History() []client.ChatMessage { return s.history }
func (s *stubRunnerChild) Session() *session.Session     { return s.sess }

// runnerTestSession returns a root session whose history path lives in a
// sandboxed sessions dir, so manifest writes are keyed to it without
// touching the real user directory.
func runnerTestSession(t *testing.T, sessionID string) *session.Session {
	t.Helper()
	tmp := t.TempDir()
	original := session.SessionDir
	session.SessionDir = func() (string, error) { return tmp, nil }
	t.Cleanup(func() { session.SessionDir = original })
	return session.New(nil, filepath.Join(tmp, sessionID+".json"), []client.ChatMessage{}, "prompt", false)
}

// TestRunnerTerminalManifestRecords pins the manifest write for each
// termination class through the same session calls the runner makes:
// completed / failed (abnormal incl. idle kill and budget exhaustion) /
// cancelled.
func TestRunnerTerminalManifestRecords(t *testing.T) {
	const sessionID = "session-20250101-120000"
	sess := runnerTestSession(t, sessionID)

	// Spawn records (what agent.NewSubagentOrchestrator writes at spawn).
	for _, rec := range []session.SubagentRecord{
		{ID: "coder-subagent-0", AgentType: "coder", Goal: "g0", Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Minute)},
		{ID: "coder-subagent-1", AgentType: "coder", Goal: "g1", Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Minute)},
		{ID: "coder-subagent-2", AgentType: "coder", Goal: "g2", Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Minute)},
		{ID: "coder-subagent-3", AgentType: "coder", Goal: "g3", Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Minute)},
	} {
		if err := sess.SaveSubagentRecord(rec); err != nil {
			t.Fatalf("SaveSubagentRecord: %v", err)
		}
	}

	// The runner's terminal writes, one per outcome class — the exact calls
	// the runner body makes.
	transcript := filepath.Join(t.TempDir(), "coder-subagent-0-transcript.md")
	if err := sess.MarkSubagentStatus("coder-subagent-0", session.SubagentStatusFailed, "idle: killed by the harness idle watchdog (no stream progress for 1m)", "", transcript); err != nil {
		t.Fatalf("MarkSubagentStatus(failed): %v", err)
	}
	if err := sess.MarkSubagentStatus("coder-subagent-1", session.SubagentStatusFailed, "time budget exhausted (2h)", "", transcript); err != nil {
		t.Fatalf("MarkSubagentStatus(budget): %v", err)
	}
	if err := sess.MarkSubagentStatus("coder-subagent-2", session.SubagentStatusCancelled, "cancelled or killed by the user", "", ""); err != nil {
		t.Fatalf("MarkSubagentStatus(cancelled): %v", err)
	}
	if err := sess.MarkSubagentStatus("coder-subagent-3", session.SubagentStatusCompleted, "", previewText(strings.Repeat("done ", 80), manifestResultPreviewLimit), ""); err != nil {
		t.Fatalf("MarkSubagentStatus(completed): %v", err)
	}

	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}

	rec0, ok := manifest.Get("coder-subagent-0")
	if !ok || rec0.Status != session.SubagentStatusFailed {
		t.Fatalf("idle-kill record = %+v (found=%v), want failed", rec0, ok)
	}
	if rec0.TranscriptPath != transcript {
		t.Errorf("idle-kill TranscriptPath = %q, want %q", rec0.TranscriptPath, transcript)
	}
	if !strings.Contains(rec0.Cause, "idle watchdog") {
		t.Errorf("idle-kill Cause = %q", rec0.Cause)
	}

	rec1, _ := manifest.Get("coder-subagent-1")
	if !ok || !strings.Contains(rec1.Cause, "time budget exhausted") {
		t.Errorf("budget-exhausted Cause = %q", rec1.Cause)
	}
	if rec1.Status != session.SubagentStatusFailed {
		t.Errorf("budget-exhausted Status = %q, want failed", rec1.Status)
	}

	rec2, _ := manifest.Get("coder-subagent-2")
	if rec2.Status != session.SubagentStatusCancelled {
		t.Errorf("cancelled Status = %q, want cancelled", rec2.Status)
	}

	rec3, _ := manifest.Get("coder-subagent-3")
	if rec3.Status != session.SubagentStatusCompleted {
		t.Errorf("completed Status = %q, want completed", rec3.Status)
	}
	if rec3.ResultPreview == "" || len(rec3.ResultPreview) > manifestResultPreviewLimit+len("…") {
		t.Errorf("completed ResultPreview = %d bytes, want clipped to the limit", len(rec3.ResultPreview))
	}
	if rec3.EndedAt.IsZero() {
		t.Errorf("completed EndedAt must be stamped, got zero")
	}
	// Identity fields survive the terminal write.
	if rec3.AgentType != "coder" || rec3.Goal != "g3" {
		t.Errorf("completed record lost identity: %+v", rec3)
	}
}

// TestPreviewText pins the rune-safe clipping of the manifest result preview.
func TestPreviewText(t *testing.T) {
	if got := previewText("short", 10); got != "short" {
		t.Errorf("previewText(short) = %q", got)
	}
	long := strings.Repeat("é", 300)
	got := previewText(long, manifestResultPreviewLimit)
	if want := strings.Repeat("é", manifestResultPreviewLimit) + "…"; got != want {
		t.Errorf("previewText clipped to %d runes, want %d", len([]rune(got))-1, manifestResultPreviewLimit)
	}
}

// TestSynthesizeDanglingSpawnResults_PersistedAndIdempotent: a parent
// history with a dangling spawn + a running manifest record gets a
// synthesized tool result PERSISTED into its history file; a second run
// finds no dangling calls and changes nothing.
func TestSynthesizeDanglingSpawnResults_PersistedAndIdempotent(t *testing.T) {
	const sessionID = "session-20250101-130000"
	sess := runnerTestSession(t, sessionID)

	// Seed the parent history: user prompt, assistant spawn call (dangling).
	historyPath := sess.HistoryPath
	if err := sess.AddUserMessage("do the thing"); err != nil {
		t.Fatalf("AddUserMessage: %v", err)
	}
	if err := sess.AddAssistantMessageWithTools("", "", []client.ToolCall{{
		Index: 0, ID: "call_spawn_1", Type: "function",
		Function: client.FunctionCall{Name: spawnSubagentToolName, Arguments: `{"goal":"sub work","agent_type":"coder"}`},
	}}); err != nil {
		t.Fatalf("AddAssistantMessageWithTools: %v", err)
	}

	// The manifest knows the child as running (interrupted at exit).
	childHistory := filepath.Join(filepath.Dir(historyPath), sessionID, "subagents", "coder-subagent-0.json")
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID:          "coder-subagent-0",
		AgentType:   "coder",
		Goal:        "sub work",
		Status:      session.SubagentStatusRunning,
		SpawnedAt:   nowMinus(t, time.Hour),
		HistoryPath: childHistory,
	}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}

	// Reload the session the way resume does (fresh session, history from
	// disk) and run the synthesis.
	reloaded, err := session.LoadHistory(historyPath)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	resumed := session.New(nil, historyPath, reloaded, "prompt", false)
	if _, err := synthesizeDanglingSpawnResults(resumed); err != nil {
		t.Fatalf("synthesizeDanglingSpawnResults: %v", err)
	}

	var toolResult *client.ChatMessage
	for i := range resumed.History {
		m := &resumed.History[i]
		if m.Role == "tool" && m.ToolCallID == "call_spawn_1" {
			toolResult = m
		}
	}
	if toolResult == nil {
		t.Fatal("no synthesized tool result in history")
	}
	text := toolResult.Content.String()
	if !strings.Contains(text, "interrupted by a previous late exit") {
		t.Errorf("synthesized text missing the interrupted wording: %q", text)
	}
	if !strings.Contains(text, childHistory) {
		t.Errorf("synthesized text must point at the preserved history %q, got %q", childHistory, text)
	}

	// Persisted: the on-disk history now contains the tool result.
	saved, err := session.LoadHistory(historyPath)
	if err != nil {
		t.Fatalf("reload persisted history: %v", err)
	}
	found := false
	for _, m := range saved {
		if m.Role == "tool" && m.ToolCallID == "call_spawn_1" {
			found = true
		}
	}
	if !found {
		t.Fatal("synthesized tool result was not persisted to the history file")
	}

	// Idempotent: a second synthesis run over the persisted history is a
	// no-op (no new messages, count unchanged).
	before := len(saved)
	resumed2 := session.New(nil, historyPath, saved, "prompt", false)
	if _, err := synthesizeDanglingSpawnResults(resumed2); err != nil {
		t.Fatalf("second synthesizeDanglingSpawnResults: %v", err)
	}
	if len(resumed2.History) != before {
		t.Fatalf("second run appended %d messages, want 0", len(resumed2.History)-before)
	}
}

// TestSynthesizeDanglingSpawnResults_StatusWordings pins the model-facing
// wording for each manifest status.
func TestSynthesizeDanglingSpawnResults_StatusWordings(t *testing.T) {
	const sessionID = "session-20250101-140000"
	sess := runnerTestSession(t, sessionID)

	spawnCall := func(id string) client.ToolCall {
		return client.ToolCall{
			Index: 0, ID: id, Type: "function",
			Function: client.FunctionCall{Name: spawnSubagentToolName, Arguments: `{}`},
		}
	}
	if err := sess.AddAssistantMessageWithTools("", "", []client.ToolCall{
		spawnCall("call_run"), spawnCall("call_done"), spawnCall("call_fail"), spawnCall("call_cancel"), spawnCall("call_unknown"),
	}); err != nil {
		t.Fatalf("AddAssistantMessageWithTools: %v", err)
	}

	transcript := "/t/coder-subagent-7-transcript.md"
	childHistory := "/s/coder-subagent-7.json"
	records := []session.SubagentRecord{
		{ID: "coder-subagent-7", AgentType: "coder", Goal: "g", Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Hour), HistoryPath: childHistory, TranscriptPath: transcript},
		{ID: "coder-subagent-8", AgentType: "coder", Goal: "g", Status: session.SubagentStatusCompleted, SpawnedAt: nowMinus(t, time.Hour), ResultPreview: "the finished thing"},
		{ID: "coder-subagent-9", AgentType: "coder", Goal: "g", Status: session.SubagentStatusFailed, SpawnedAt: nowMinus(t, time.Hour), Cause: "crashed: boom"},
		{ID: "coder-subagent-10", AgentType: "coder", Goal: "g", Status: session.SubagentStatusCancelled, SpawnedAt: nowMinus(t, time.Hour), Cause: "cancelled or killed by the user"},
	}
	for _, rec := range records {
		if err := sess.SaveSubagentRecord(rec); err != nil {
			t.Fatalf("SaveSubagentRecord(%s): %v", rec.ID, err)
		}
	}

	// The history on disk carries the dangling calls; reload like resume.
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

	// Running → interrupted wording with preserved-work pointers.
	if got := results["call_run"]; !strings.Contains(got, "coder-subagent-7 was interrupted") ||
		!strings.Contains(got, childHistory) || !strings.Contains(got, transcript) {
		t.Errorf("running record wording mismatch: %q", got)
	}
	// Completed → rare race wording + preview.
	if got := results["call_done"]; !strings.Contains(got, "completed its task") || !strings.Contains(got, "the finished thing") {
		t.Errorf("completed record wording mismatch: %q", got)
	}
	// Failed → cause preserved.
	if got := results["call_fail"]; !strings.Contains(got, "failed in a previous session") || !strings.Contains(got, "crashed: boom") {
		t.Errorf("failed record wording mismatch: %q", got)
	}
	// Cancelled → cancelled wording.
	if got := results["call_cancel"]; !strings.Contains(got, "cancelled in a previous session") {
		t.Errorf("cancelled record wording mismatch: %q", got)
	}
	// Unknown call ID → generic interrupted fallback.
	if got := results["call_unknown"]; !strings.Contains(got, "interrupted by a previous late exit") {
		t.Errorf("unknown-record fallback wording mismatch: %q", got)
	}
}

// TestSynthesizeDanglingSpawnResults_NonInterruptedHistoryUntouched: no
// manifest → nothing is synthesized, history untouched (the fast path every
// normal session takes).
func TestSynthesizeDanglingSpawnResults_NonInterruptedHistoryUntouched(t *testing.T) {
	const sessionID = "session-20250101-150000"
	sess := runnerTestSession(t, sessionID)

	if err := sess.AddUserMessage("hello"); err != nil {
		t.Fatalf("AddUserMessage: %v", err)
	}
	before := len(sess.History)

	// No manifest file exists at all.
	if _, err := synthesizeDanglingSpawnResults(sess); err != nil {
		t.Fatalf("synthesizeDanglingSpawnResults: %v", err)
	}
	if len(sess.History) != before {
		t.Fatalf("history changed without a manifest: %d → %d", before, len(sess.History))
	}
}

// TestSynthesizeDanglingSpawnResults_InMemorySessionNoop: a session without
// a history path (no folder) is a silent no-op.
func TestSynthesizeDanglingSpawnResults_InMemorySessionNoop(t *testing.T) {
	sess := session.New(nil, "", []client.ChatMessage{}, "prompt", false)
	if _, err := synthesizeDanglingSpawnResults(sess); err != nil {
		t.Fatalf("synthesizeDanglingSpawnResults on in-memory session = %v, want nil", err)
	}
}

// TestSynthesizedResultsAreValidToolExchange proves the synthesized message
// shape is the persisted, request-valid form: Role tool with matching
// ToolCallID — the exact shape AddToolResultMessage writes and strict
// endpoints accept.
func TestSynthesizedResultsAreValidToolExchange(t *testing.T) {
	const sessionID = "session-20250101-160000"
	sess := runnerTestSession(t, sessionID)

	if err := sess.AddAssistantMessageWithTools("", "", []client.ToolCall{{
		Index: 0, ID: "call_x", Type: "function",
		Function: client.FunctionCall{Name: spawnSubagentToolName, Arguments: `{}`},
	}}); err != nil {
		t.Fatalf("AddAssistantMessageWithTools: %v", err)
	}
	if err := sess.SaveSubagentRecord(session.SubagentRecord{ID: "coder-subagent-0", AgentType: "coder", Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Minute)}); err != nil {
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

	// The persisted JSON history must decode with the tool message last.
	data, err := os.ReadFile(sess.HistoryPath)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	var msgs []client.ChatMessage
	if err := json.Unmarshal(data, &msgs); err != nil {
		t.Fatalf("history is not valid JSON: %v", err)
	}
	last := msgs[len(msgs)-1]
	if last.Role != "tool" || last.ToolCallID != "call_x" {
		t.Fatalf("last persisted message = %+v, want the synthesized tool result", last)
	}
}
