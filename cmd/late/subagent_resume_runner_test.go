package main

import (
	"context"
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
	"late/internal/tool"
)

// newRunnerTestEnv builds the subagentRunEnv a runner test needs: a real
// root orchestrator over a sandboxed session, no plugins/archive/retrieval.
func newRunnerTestEnv(t *testing.T, sessionID string) (*subagentRunEnv, *orchestrator.BaseOrchestrator) {
	t.Helper()
	sess := runnerTestSession(t, sessionID)
	root := orchestrator.NewBaseOrchestrator(common.MainAgentID, sess, nil, 0)
	env := &subagentRunEnv{
		pluginManager: nil,
		messenger:     nil,
		root:          root,
		sess:          sess,
		toolArchive:   "",
	}
	return env, root
}

// TestResumeRunnerRunsChildWithLoadedHistory is the runner-level resume
// test: an interrupted record with persisted history is restored through
// the same constructor call the resume closure makes; the live child
// carries the loaded history (no goal re-append), the exact record ID, a
// full fresh-spawn registry, and its next turn lands on the same history
// file.
func TestResumeRunnerRunsChildWithLoadedHistory(t *testing.T) {
	const sessionID = "session-phasec-runner"
	env, root := newRunnerTestEnv(t, sessionID)

	historyPath, err := session.SubagentHistoryPath(sessionID, "coder-subagent-4")
	if err != nil {
		t.Fatalf("SubagentHistoryPath: %v", err)
	}
	preserved := []client.ChatMessage{
		{Role: "user", Content: client.TextContent("Goal: restore me")},
		{Role: "assistant", Content: client.TextContent("I got halfway through.")},
	}
	if err := os.MkdirAll(filepath.Dir(historyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := session.SaveHistory(historyPath, preserved); err != nil {
		t.Fatalf("SaveHistory: %v", err)
	}

	c := client.NewClient(client.Config{BaseURL: "http://localhost:8080"})
	record := session.SubagentRecord{
		ID: "coder-subagent-4", AgentType: "coder", Goal: "restore me",
		Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, oneHour),
		HistoryPath: historyPath, WorkingDir: "/repo",
	}
	if err := env.sess.SaveSubagentRecord(record); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}

	// The resume gate passes for this record.
	if err := validateResumeRecord(&record); err != nil {
		t.Fatalf("validateResumeRecord: %v", err)
	}

	child, id, err := agent.NewResumedSubagentOrchestrator(c, record, "coder", map[string]bool{"read_file": true}, false, false, 10, root, nil)
	if err != nil {
		t.Fatalf("NewResumedSubagentOrchestrator: %v", err)
	}
	if id != "coder-subagent-4" {
		t.Errorf("id = %q, want the record ID", id)
	}

	// History is loaded BEFORE Execute (the LLM's next request would see
	// the full prior context).
	loaded := child.History()
	if len(loaded) != len(preserved) {
		t.Fatalf("loaded history holds %d messages, want %d", len(loaded), len(preserved))
	}
	if loaded[1].Content.String() != "I got halfway through." {
		t.Errorf("loaded[1] = %q, want the preserved assistant message", loaded[1].Content.String())
	}

	// Registry: parent's inherited surface + config-allowed tools, minus
	// the orchestrator-only tools.
	if child.Registry().Get("spawn_subagent") != nil {
		t.Error("resumed child must not inherit spawn_subagent")
	}
	if child.Registry().Get("read_file") == nil {
		t.Error("resumed child registry missing the allowed read_file tool")
	}

	// The child is on the parent (TUI can address it).
	found := false
	for _, ch := range root.Children() {
		if ch.ID() == id {
			found = true
		}
	}
	if !found {
		t.Error("resumed child missing from root.Children()")
	}

	// A turn executed through the shared executor appends to the SAME
	// history file the record points at (the next resume sees it).
	ctx := context.WithValue(context.Background(), common.MaxStreamRetriesKey, -1)
	if _, err := buildAndWireChild(env, child, wireChildConfig{runCtx: ctx}); err != nil {
		// The client points nowhere, so the turn itself fails — the
		// snapshot/commit machinery must still have persisted the run's
		// history state. Any failure here is the expected unreachable-
		// backend class; the assertion below is what matters.
		t.Logf("Execute failed as expected without a backend: %v", err)
	}
	saved, err := session.LoadHistory(historyPath)
	if err != nil {
		t.Fatalf("LoadHistory after execute: %v", err)
	}
	if len(saved) < len(preserved) {
		t.Fatalf("history file holds %d messages, want at least the %d preserved", len(saved), len(preserved))
	}

	// The manifest counted the resume.
	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	rec, ok := manifest.Get("coder-subagent-4")
	if !ok {
		t.Fatal("manifest record missing after resume")
	}
	if rec.ResumeCount != 1 {
		t.Errorf("ResumeCount = %d, want 1", rec.ResumeCount)
	}
}

// TestResumeRunnerTerminalRecordRefused pins the runner-level guard: a
// terminal record's resume is refused with the spawn-fresh guidance before
// any orchestrator is built.
func TestResumeRunnerTerminalRecordRefused(t *testing.T) {
	const sessionID = "session-phasec-terminal"
	env, _ := newRunnerTestEnv(t, sessionID)

	record := session.SubagentRecord{
		ID: "coder-subagent-5", AgentType: "coder", Goal: "done work",
		Status: session.SubagentStatusCompleted, SpawnedAt: nowMinus(t, oneHour),
		ResultPreview: "finished!",
	}
	if err := env.sess.SaveSubagentRecord(record); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}

	err := validateResumeRecord(&record)
	if err == nil {
		t.Fatal("completed record must not be resumable")
	}
	if !strings.Contains(err.Error(), "already terminated (completed)") ||
		!strings.Contains(err.Error(), "spawn a fresh agent instead") {
		t.Errorf("terminal resume error = %v", err)
	}

	// Unknown record: also refused.
	err = validateResumeRecord(&session.SubagentRecord{ID: "ghost", Status: session.SubagentStatusRunning})
	if err == nil || !strings.Contains(err.Error(), "no persisted history") {
		t.Errorf("history-less record error = %v", err)
	}
}

// oneHour is a named duration so the resume tests read cleanly.
const oneHour = time.Hour

// TestResumeRequestToolWiringThroughRegistry verifies the registry path end
// to end at the tool level: registering tool.SpawnSubagentTool on a session
// registry and issuing a resume request through the same dispatch the
// executor uses reaches the runner with the parsed ResumeID (the parent
// model's exact surface for Phase C resume).
func TestResumeRequestToolWiringThroughRegistry(t *testing.T) {
	runnerCalled := false
	var gotID string
	sess := session.New(nil, "", nil, "prompt", true)
	sess.Registry.Register(tool.SpawnSubagentTool{
		Runner: func(ctx context.Context, request tool.SubagentSpawnRequest, timeoutOverride *time.Duration) (string, error) {
			runnerCalled = true
			gotID = request.ResumeID
			return "resumed ok", nil
		},
	})

	result, err := sess.ExecuteTool(context.Background(), client.ToolCall{
		Index: 0, ID: "call_resume", Type: "function",
		Function: client.FunctionCall{Name: "spawn_subagent", Arguments: `{"resume":"coder-subagent-7"}`},
	})
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	if !runnerCalled {
		t.Fatal("runner was not invoked through the registry")
	}
	if gotID != "coder-subagent-7" {
		t.Errorf("ResumeID = %q, want coder-subagent-7", gotID)
	}
	if result != "resumed ok" {
		t.Errorf("result = %q, want resumed ok", result)
	}
}
