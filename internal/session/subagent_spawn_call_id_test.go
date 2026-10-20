package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPatchSubagentSpawnCallID_StampsAndPersists: the patch binds a record
// to its spawn tool-call ID, persists it under the spawn_call_id json key,
// never invents a record, and keeps the FIRST binding on a later patch.
func TestPatchSubagentSpawnCallID_StampsAndPersists(t *testing.T) {
	tmp := t.TempDir()
	stubSessionDir(t, tmp)
	sessionID := "patch-test"
	sess := manifestTestSession(t, tmp, sessionID)

	// No record yet: a patch may never invent one.
	if err := sess.PatchSubagentSpawnCallID("child-x", "call-x"); err != nil {
		t.Fatalf("patch on unknown id must be a benign no-op, got %v", err)
	}

	manifest, err := LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	manifest.Upsert(SubagentRecord{
		ID:        "child-x",
		AgentType: "coder",
		Goal:      "legacy goal",
		Status:    SubagentStatusRunning,
		SpawnedAt: time.Now(),
	})
	if err := manifest.Save(); err != nil {
		t.Fatalf("seed record: %v", err)
	}

	if err := sess.PatchSubagentSpawnCallID("child-x", "call-x"); err != nil {
		t.Fatalf("patch: %v", err)
	}
	loaded, err := LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("reload manifest: %v", err)
	}
	rec, ok := loaded.Get("child-x")
	if !ok {
		t.Fatal("record vanished")
	}
	if rec.SpawnCallID != "call-x" {
		t.Errorf("SpawnCallID = %q, want call-x", rec.SpawnCallID)
	}

	// First binding wins: a later patch cannot move the correlation.
	if err := sess.PatchSubagentSpawnCallID("child-x", "call-other"); err != nil {
		t.Fatalf("second patch: %v", err)
	}
	loaded, err = LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("reload manifest: %v", err)
	}
	rec, _ = loaded.Get("child-x")
	if rec.SpawnCallID != "call-x" {
		t.Errorf("SpawnCallID after second patch = %q, want unchanged call-x", rec.SpawnCallID)
	}

	// Raw JSON carries the field under its snake_case name.
	raw, err := os.ReadFile(filepath.Join(tmp, sessionID, "subagents", "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(raw), `"spawn_call_id": "call-x"`) {
		t.Errorf("manifest json missing spawn_call_id binding")
	}
}
