package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"late/internal/client"
	"late/internal/orchestrator"
	"late/internal/session"
)

// TestNewSubagentOrchestrator_WritesRunningManifestRecord verifies that a
// spawn registers a running record in the PARENT session's manifest — the
// manifest is keyed by the parent session folder and written even though the
// child session itself skips metadata.
func TestNewSubagentOrchestrator_WritesRunningManifestRecord(t *testing.T) {
	tmp := t.TempDir()
	setSessionDirForTest(t, tmp)

	cfg := client.Config{BaseURL: "http://localhost:8080"}
	c := client.NewClient(cfg)

	mockSession := session.New(c, filepath.Join(tmp, "session-test.json"), []client.ChatMessage{}, "mock system prompt", true)
	parent := orchestrator.NewBaseOrchestrator("parent", mockSession, nil, 10)

	goal := "research the options"
	ctxFiles := []string{"notes.md"}
	_, err := NewSubagentOrchestrator(
		c,
		goal,
		ctxFiles,
		"researcher",
		map[string]bool{},
		false, // injectCWD
		false, // gemmaThinking
		10,    // maxTurns
		"session-test",
		true, // saveSubagentHistory
		parent,
		nil, // messenger
	)
	if err != nil {
		t.Fatalf("NewSubagentOrchestrator: %v", err)
	}

	manifest, err := session.LoadSubagentManifest("session-test")
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	rec, ok := manifest.Get("researcher-subagent-0")
	if !ok {
		t.Fatalf("no manifest record for researcher-subagent-0, records = %+v", manifest.Records)
	}
	if rec.AgentType != "researcher" {
		t.Errorf("AgentType = %q, want researcher", rec.AgentType)
	}
	if rec.Goal != goal {
		t.Errorf("Goal = %q, want %q", rec.Goal, goal)
	}
	if len(rec.CtxFiles) != 1 || rec.CtxFiles[0] != "notes.md" {
		t.Errorf("CtxFiles = %v, want [notes.md]", rec.CtxFiles)
	}
	if rec.Status != session.SubagentStatusRunning {
		t.Errorf("Status = %q, want running at spawn time", rec.Status)
	}
	if rec.SpawnedAt.IsZero() || rec.SpawnedAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("SpawnedAt not stamped: %v", rec.SpawnedAt)
	}
	if !rec.EndedAt.IsZero() {
		t.Errorf("EndedAt must be zero while running, got %v", rec.EndedAt)
	}

	// The recorded history path must already exist on disk (the initial
	// goal message is persisted before the manifest record is written).
	if rec.HistoryPath == "" {
		t.Fatal("HistoryPath empty in the spawn record")
	}
	if _, err := os.Stat(rec.HistoryPath); err != nil {
		t.Errorf("recorded history path missing: %v", err)
	}
	if want := filepath.Join(tmp, "session-test", "subagents", "researcher-subagent-0.json"); rec.HistoryPath != want {
		t.Errorf("HistoryPath = %q, want %q", rec.HistoryPath, want)
	}
}

// TestNewSubagentOrchestrator_ManifestRecordWithoutHistoryPersistence
// verifies the manifest is still written when subagent history persistence
// is disabled for the run: the record then carries an empty HistoryPath and
// resume knows the child existed (and where its transcript would be) even
// though its conversation was in-memory only.
func TestNewSubagentOrchestrator_ManifestRecordWithoutHistoryPersistence(t *testing.T) {
	tmp := t.TempDir()
	setSessionDirForTest(t, tmp)

	cfg := client.Config{BaseURL: "http://localhost:8080"}
	c := client.NewClient(cfg)

	mockSession := session.New(c, filepath.Join(tmp, "session-test.json"), []client.ChatMessage{}, "mock system prompt", true)
	parent := orchestrator.NewBaseOrchestrator("parent", mockSession, nil, 10)

	_, err := NewSubagentOrchestrator(
		c,
		"goal",
		nil,
		"coder",
		map[string]bool{},
		false,
		false,
		10,
		"session-test",
		false, // saveSubagentHistory
		parent,
		nil,
	)
	if err != nil {
		t.Fatalf("NewSubagentOrchestrator: %v", err)
	}

	manifest, err := session.LoadSubagentManifest("session-test")
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	rec, ok := manifest.Get("coder-subagent-0")
	if !ok {
		t.Fatalf("no manifest record for coder-subagent-0, records = %+v", manifest.Records)
	}
	if rec.HistoryPath != "" {
		t.Errorf("HistoryPath = %q, want empty when history persistence is off", rec.HistoryPath)
	}
	if rec.Status != session.SubagentStatusRunning {
		t.Errorf("Status = %q, want running", rec.Status)
	}
}

// TestNewSubagentOrchestrator_NoManifestWithoutSessionFolder verifies that
// an in-memory parent session (empty parentSessionID → no session folder)
// spawns without writing any manifest anywhere.
func TestNewSubagentOrchestrator_NoManifestWithoutSessionFolder(t *testing.T) {
	tmp := t.TempDir()
	setSessionDirForTest(t, tmp)

	cfg := client.Config{BaseURL: "http://localhost:8080"}
	c := client.NewClient(cfg)

	// Root session with NO history path: nothing can be keyed.
	mockSession := session.New(c, "", []client.ChatMessage{}, "mock system prompt", true)
	parent := orchestrator.NewBaseOrchestrator("parent", mockSession, nil, 10)

	_, err := NewSubagentOrchestrator(
		c,
		"goal",
		nil,
		"researcher",
		map[string]bool{},
		false,
		false,
		10,
		"", // parentSessionID: no session folder
		true,
		parent,
		nil,
	)
	if err != nil {
		t.Fatalf("NewSubagentOrchestrator: %v", err)
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("no manifest may be written without a session folder, found: %v", entries)
	}
}
