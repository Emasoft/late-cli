package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"late/internal/agent"
	"late/internal/client"
	"late/internal/common"
	"late/internal/orchestrator"
	"late/internal/session"
)

// ids renders a child list for failure messages.
func ids(children []common.Orchestrator) []string {
	out := make([]string, 0, len(children))
	for _, c := range children {
		out = append(out, c.ID())
	}
	return out
}

// childByID indexes a child list by ID.
func childByID(children []common.Orchestrator, id string) common.Orchestrator {
	for _, c := range children {
		if c.ID() == id {
			return c
		}
	}
	return nil
}

// TestRestoreInterruptedSubagentsListsOnlyInterrupted pins the Phase 3b
// resume contract: interrupted (running-at-exit) manifest records re-enter
// the TUI as read-only children of the root with their loaded histories;
// completed records do NOT (their outcome already lives in the parent
// transcript), and neither do failed/cancelled ones.
func TestRestoreInterruptedSubagentsListsOnlyInterrupted(t *testing.T) {
	const sessionID = "session-20250101-170000"
	sess := runnerTestSession(t, sessionID)

	// Two interrupted children with persisted work, one completed, one
	// cancelled — the manifest spread resume can encounter.
	interruptedA := filepath.Join(filepath.Dir(sess.HistoryPath), sessionID, "subagents", "coder-subagent-0.json")
	interruptedB := filepath.Join(filepath.Dir(sess.HistoryPath), sessionID, "subagents", "coder-subagent-1.json")
	for _, rec := range []session.SubagentRecord{
		{ID: "coder-subagent-0", AgentType: "coder", Goal: "first task", Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Hour), HistoryPath: interruptedA},
		{ID: "coder-subagent-1", AgentType: "researcher", Goal: "second task", Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, 2*time.Hour), HistoryPath: interruptedB},
		{ID: "coder-subagent-2", AgentType: "coder", Goal: "done task", Status: session.SubagentStatusCompleted, SpawnedAt: nowMinus(t, 3*time.Hour), ResultPreview: "finished"},
		{ID: "coder-subagent-3", AgentType: "coder", Goal: "killed task", Status: session.SubagentStatusCancelled, SpawnedAt: nowMinus(t, 4*time.Hour), Cause: "cancelled or killed by the user"},
	} {
		if err := sess.SaveSubagentRecord(rec); err != nil {
			t.Fatalf("SaveSubagentRecord(%s): %v", rec.ID, err)
		}
	}

	// Persisted work for the two interrupted children.
	if err := session.SaveHistory(interruptedA, []client.ChatMessage{
		{Role: "user", Content: client.TextContent("Goal: first task")},
		{Role: "assistant", Content: client.TextContent("halfway through the first task")},
	}); err != nil {
		t.Fatalf("SaveHistory(A): %v", err)
	}
	if err := session.SaveHistory(interruptedB, []client.ChatMessage{
		{Role: "user", Content: client.TextContent("Goal: second task")},
	}); err != nil {
		t.Fatalf("SaveHistory(B): %v", err)
	}

	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}

	root := orchestrator.NewBaseOrchestrator(common.MainAgentID, sess, nil, 0)
	restored := make(map[string]bool)
	restoreInterruptedSubagents(root, manifest, restored)

	children := root.Children()
	if len(children) != 2 {
		t.Fatalf("root.Children() holds %d children (%v), want exactly the 2 interrupted records", len(children), ids(children))
	}

	childA := childByID(children, "coder-subagent-0")
	if childA == nil {
		t.Fatalf("interrupted record coder-subagent-0 not listed; children = %v", ids(children))
	}
	if got := len(childA.History()); got != 2 {
		t.Errorf("coder-subagent-0 history holds %d messages, want the 2 persisted ones", got)
	}
	childB := childByID(children, "coder-subagent-1")
	if childB == nil {
		t.Fatalf("interrupted record coder-subagent-1 not listed; children = %v", ids(children))
	}
	if got := len(childB.History()); got != 1 {
		t.Errorf("coder-subagent-1 history holds %d messages, want the 1 persisted one", got)
	}
	for _, terminal := range []string{"coder-subagent-2", "coder-subagent-3"} {
		if childByID(children, terminal) != nil {
			t.Errorf("terminal record %s must NOT be restored into the TUI", terminal)
		}
	}
	if len(restored) != 2 {
		t.Errorf("restored map holds %d ids, want 2", len(restored))
	}
}

// TestRestoreInterruptedSubagentsStatusAndReadOnly pins the surfaced state:
// the restored child is the concrete agent.RestoredSubagentOrchestrator, it
// carries the manifest cause in its status text, and it refuses execution
// (read-only).
func TestRestoreInterruptedSubagentsStatusAndReadOnly(t *testing.T) {
	const sessionID = "session-20250101-170001"
	sess := runnerTestSession(t, sessionID)

	childHistory := filepath.Join(filepath.Dir(sess.HistoryPath), sessionID, "subagents", "coder-subagent-5.json")
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: "coder-subagent-5", AgentType: "coder", Goal: "g",
		Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Hour),
		HistoryPath: childHistory, Cause: "time budget exhausted (2h)",
	}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}
	if err := session.SaveHistory(childHistory, []client.ChatMessage{
		{Role: "user", Content: client.TextContent("Goal: g")},
	}); err != nil {
		t.Fatalf("SaveHistory: %v", err)
	}

	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}

	root := orchestrator.NewBaseOrchestrator(common.MainAgentID, sess, nil, 0)
	restoreInterruptedSubagents(root, manifest, make(map[string]bool))

	children := root.Children()
	if len(children) != 1 {
		t.Fatalf("root.Children() holds %d children, want 1", len(children))
	}
	ro, ok := children[0].(*agent.RestoredSubagentOrchestrator)
	if !ok {
		t.Fatalf("restored child type = %T, want *agent.RestoredSubagentOrchestrator", children[0])
	}
	if got := ro.StatusText(); !strings.Contains(got, "restored") || !strings.Contains(got, "time budget exhausted") {
		t.Errorf("StatusText() = %q, want the restored wording with the manifest cause", got)
	}
	if got := ro.AgentType(); got != "coder" {
		t.Errorf("AgentType() = %q, want coder", got)
	}
	if got := ro.Goal(); got != "g" {
		t.Errorf("Goal() = %q, want g", got)
	}
	if _, err := ro.Execute(""); err == nil {
		t.Error("restored child Execute() must fail (read-only)")
	}
	if ro.Registry() != nil {
		t.Error("restored child Registry() must be nil (no tools)")
	}
}

// TestRestoreInterruptedSubagentsIdempotentAndTolerant pins the guards:
// an already-restored ID is skipped, a record without a persisted history is
// skipped (logged, never fatal), a corrupt history file is skipped, and a
// nil manifest/root is a silent no-op.
func TestRestoreInterruptedSubagentsIdempotentAndTolerant(t *testing.T) {
	const sessionID = "session-20250101-170002"
	sess := runnerTestSession(t, sessionID)

	// A running record with NO history file (persistence was disabled).
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: "coder-subagent-7", AgentType: "coder", Goal: "g",
		Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Hour),
	}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}
	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}

	root := orchestrator.NewBaseOrchestrator(common.MainAgentID, sess, nil, 0)

	restored := make(map[string]bool)
	restoreInterruptedSubagents(root, manifest, restored)
	if got := len(root.Children()); got != 0 {
		t.Fatalf("record without a history path must not be listed, got %d children", got)
	}

	// A corrupt history file is skipped, never fatal.
	corrupt := filepath.Join(filepath.Dir(sess.HistoryPath), sessionID, "subagents", "coder-subagent-8.json")
	if err := os.MkdirAll(filepath.Dir(corrupt), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: "coder-subagent-8", AgentType: "coder", Goal: "g",
		Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Hour),
		HistoryPath: corrupt,
	}); err != nil {
		t.Fatalf("SaveSubagentRecord(8): %v", err)
	}
	manifest, err = session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	restoreInterruptedSubagents(root, manifest, restored)
	if got := len(root.Children()); got != 0 {
		t.Fatalf("corrupt-history record must not be listed, got %d children", got)
	}

	// nil manifest / nil root are silent no-ops.
	restoreInterruptedSubagents(root, nil, restored)
	restoreInterruptedSubagents(nil, manifest, restored)
}

// TestRestoredSubagentStatusText pins the status-line rendering: the
// manifest cause, when present, is appended to the restored wording.
func TestRestoredSubagentStatusText(t *testing.T) {
	if got := RestoredSubagentStatusText(""); got != agent.RestoredSubagentStatus {
		t.Errorf("RestoredSubagentStatusText(\"\") = %q, want the default wording", got)
	}
	got := RestoredSubagentStatusText("crashed: boom")
	if !strings.Contains(got, "restored") || !strings.Contains(got, "crashed: boom") {
		t.Errorf("RestoredSubagentStatusText(cause) = %q, want wording + cause", got)
	}
}
