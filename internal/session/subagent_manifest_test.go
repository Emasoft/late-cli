package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// manifestTestSession returns a root session whose HistoryPath lives in the
// temp sessions dir, so its manifest record key is the session ID.
func manifestTestSession(t *testing.T, sessionsDir, sessionID string) *Session {
	t.Helper()
	return New(nil, filepath.Join(sessionsDir, sessionID+".json"), nil, "prompt", false)
}

// stubSessionDir points SessionDir at dir for the test's duration — the
// manifest path helpers resolve through it, exactly like the other session
// persistence tests.
func stubSessionDir(t *testing.T, dir string) {
	t.Helper()
	original := SetSessionDirOverrideForTest(func() (string, error) { return dir, nil })
	t.Cleanup(func() { SetSessionDirOverrideForTest(original) })
}

func TestSubagentManifestSaveLoadRoundTrip(t *testing.T) {
	tmp := t.TempDir()

	stubSessionDir(t, tmp)

	manifest, err := LoadSubagentManifest("session-x")
	if err != nil {
		t.Fatalf("LoadSubagentManifest on missing file: %v", err)
	}
	if len(manifest.Records) != 0 {
		t.Fatalf("missing file must yield an empty manifest, got %d records", len(manifest.Records))
	}

	spawned := time.Now().Add(-time.Minute).Truncate(time.Second)
	manifest.Upsert(SubagentRecord{
		ID:          "coder-subagent-0",
		AgentType:   "coder",
		Goal:        "write the thing",
		CtxFiles:    []string{"a.go", "b.go"},
		Status:      SubagentStatusRunning,
		SpawnedAt:   spawned,
		HistoryPath: filepath.Join(tmp, "session-x", "subagents", "coder-subagent-0.json"),
	})
	if err := manifest.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadSubagentManifest("session-x")
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	if loaded.SessionID != "session-x" {
		t.Errorf("SessionID = %q, want session-x", loaded.SessionID)
	}
	if len(loaded.Records) != 1 {
		t.Fatalf("loaded %d records, want 1", len(loaded.Records))
	}
	rec := loaded.Records[0]
	if rec.ID != "coder-subagent-0" || rec.AgentType != "coder" || rec.Goal != "write the thing" {
		t.Errorf("record identity mismatch: %+v", rec)
	}
	if len(rec.CtxFiles) != 2 || rec.CtxFiles[0] != "a.go" {
		t.Errorf("CtxFiles round-trip mismatch: %v", rec.CtxFiles)
	}
	if rec.Status != SubagentStatusRunning {
		t.Errorf("Status = %q, want running", rec.Status)
	}
	if !rec.SpawnedAt.Equal(spawned) {
		t.Errorf("SpawnedAt = %v, want %v", rec.SpawnedAt, spawned)
	}
	if !rec.EndedAt.IsZero() {
		t.Errorf("EndedAt = %v, want zero while running", rec.EndedAt)
	}
	if rec.HistoryPath == "" {
		t.Error("HistoryPath lost in round-trip")
	}

	// The file must live at the documented path.
	wantPath := filepath.Join(tmp, "session-x", "subagents", "manifest.json")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("manifest file missing at %s: %v", wantPath, err)
	}
}

func TestSubagentManifestFilePermissionsAndAtomicity(t *testing.T) {
	tmp := t.TempDir()

	stubSessionDir(t, tmp)

	manifest, err := LoadSubagentManifest("session-x")
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	manifest.Upsert(SubagentRecord{ID: "r0", AgentType: "coder", Goal: "g", Status: SubagentStatusRunning, SpawnedAt: time.Now()})
	if err := manifest.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	dir := filepath.Join(tmp, "session-x", "subagents")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0700 {
		t.Errorf("dir perms = %o, want 700", got)
	}
	fileInfo, err := os.Stat(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("stat manifest: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0600 {
		t.Errorf("manifest perms = %o, want 600", got)
	}

	// The atomic write must not leave temp files behind after a successful
	// save: the directory holds exactly the manifest.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "manifest.json" {
		t.Fatalf("unexpected directory contents after save: %v", entries)
	}

	// Overwrite: the rename replaces the file whole, no leftovers either.
	manifest.MarkStatus("r0", SubagentStatusCompleted, "", "done", "")
	if err := manifest.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir after overwrite: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "manifest.json" {
		t.Fatalf("temp file leaked by overwrite: %v", entries)
	}
}

func TestSubagentManifestUpsertGetMarkStatus(t *testing.T) {
	manifest := &SubagentManifest{SessionID: "session-x"}
	manifest.Upsert(SubagentRecord{ID: "a", AgentType: "coder", Goal: "g", Status: SubagentStatusRunning, SpawnedAt: time.Now()})
	manifest.Upsert(SubagentRecord{ID: "b", AgentType: "researcher", Goal: "g", Status: SubagentStatusRunning, SpawnedAt: time.Now()})

	// Upsert with the same ID replaces, not appends.
	manifest.Upsert(SubagentRecord{ID: "a", AgentType: "coder", Goal: "g2", Status: SubagentStatusRunning, SpawnedAt: time.Now()})
	if len(manifest.Records) != 2 {
		t.Fatalf("after duplicate upsert len = %d, want 2", len(manifest.Records))
	}

	rec, ok := manifest.Get("a")
	if !ok {
		t.Fatal("Get(a) not found")
	}
	if rec.Goal != "g2" {
		t.Errorf("re-upserted record Goal = %q, want g2", rec.Goal)
	}
	if _, ok := manifest.Get("missing"); ok {
		t.Error("Get(missing) reported found")
	}
	if got, ok := manifest.Get("missing"); got != nil || ok {
		t.Errorf("Get(missing) must return (nil, false), got (%v, %v)", got, ok)
	}

	// MarkStatus stamps EndedAt and clears-or-sets its fields, preserving
	// identity fields.
	before := time.Now()
	manifest.MarkStatus("a", SubagentStatusCompleted, "", "result preview", "")
	after := time.Now()
	rec, _ = manifest.Get("a")
	if rec.Status != SubagentStatusCompleted {
		t.Errorf("Status = %q, want completed", rec.Status)
	}
	if rec.EndedAt.Before(before) || rec.EndedAt.After(after) {
		t.Errorf("EndedAt = %v, want within [%v, %v]", rec.EndedAt, before, after)
	}
	if rec.ResultPreview != "result preview" {
		t.Errorf("ResultPreview = %q, want the marked value", rec.ResultPreview)
	}
	if rec.AgentType != "coder" || rec.Goal != "g2" {
		t.Errorf("MarkStatus must preserve identity fields, got %+v", rec)
	}
	if !rec.SpawnedAt.IsZero() && rec.EndedAt.Before(rec.SpawnedAt) {
		t.Errorf("EndedAt %v before SpawnedAt %v", rec.EndedAt, rec.SpawnedAt)
	}

	// Marking an unknown ID is a silent no-op.
	manifest.MarkStatus("unknown", SubagentStatusFailed, "cause", "", "")
	if len(manifest.Records) != 2 {
		t.Fatalf("MarkStatus on unknown ID changed the record count: %d", len(manifest.Records))
	}
}

func TestSubagentManifestSaveNoopWithoutSessionID(t *testing.T) {
	manifest := &SubagentManifest{}
	manifest.Upsert(SubagentRecord{ID: "a", Status: SubagentStatusRunning, SpawnedAt: time.Now()})
	if err := manifest.Save(); err != nil {
		t.Fatalf("Save with empty SessionID must be a no-op, got %v", err)
	}
}

func TestSubagentManifestRejectsUnsafeSessionIDs(t *testing.T) {
	for _, unsafeID := range []string{"", ".", "..", "../x", "a/b"} {
		if _, err := LoadSubagentManifest(unsafeID); err == nil {
			t.Errorf("LoadSubagentManifest(%q): expected an error", unsafeID)
		}
	}
}

func TestSubagentManifestCorruptFileIsAnError(t *testing.T) {
	tmp := t.TempDir()

	stubSessionDir(t, tmp)
	dir := filepath.Join(tmp, "session-x", "subagents")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSubagentManifest("session-x"); err == nil {
		t.Fatal("expected an error for a corrupt manifest, got nil")
	}
}

func TestSaveSubagentRecordBypassesSubagentSkipMetadata(t *testing.T) {
	tmp := t.TempDir()

	stubSessionDir(t, tmp)

	// A SUBAGENT session (skipMetadata=true) must still write the manifest
	// when it is keyed by a parent session folder: the manifest is a parent-
	// folder sidecar, not the child's metadata.
	sub := NewSubagentSession(nil, filepath.Join(tmp, "session-x", "subagents", "coder-subagent-0.json"), nil, "prompt")
	if err := sub.SaveSubagentRecord(SubagentRecord{
		ID:        "coder-subagent-0",
		AgentType: "coder",
		Goal:      "g",
		Status:    SubagentStatusRunning,
		SpawnedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveSubagentRecord from a subagent session: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "session-x", "subagents", "manifest.json")); err != nil {
		t.Fatalf("manifest not written through the subagent session: %v", err)
	}
}

func TestSaveSubagentRecordKeyedFromParentHistoryPath(t *testing.T) {
	tmp := t.TempDir()

	stubSessionDir(t, tmp)

	// A root session's manifest record key is derived from its history path.
	sess := manifestTestSession(t, tmp, "session-20250101-010101")
	if err := sess.SaveSubagentRecord(SubagentRecord{ID: "r0", AgentType: "coder", Goal: "g", Status: SubagentStatusRunning, SpawnedAt: time.Now()}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "session-20250101-010101", "subagents", "manifest.json")); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
}

func TestSaveSubagentRecordUpsertAcrossInstances(t *testing.T) {
	// Two writes through separate Session instances (simulating spawn then a
	// later session-less terminal write) must merge on disk, not overwrite.
	tmp := t.TempDir()

	stubSessionDir(t, tmp)

	first := manifestTestSession(t, tmp, "session-x")
	if err := first.SaveSubagentRecord(SubagentRecord{ID: "r0", AgentType: "coder", Goal: "g", Status: SubagentStatusRunning, SpawnedAt: time.Now()}); err != nil {
		t.Fatalf("first SaveSubagentRecord: %v", err)
	}

	second := manifestTestSession(t, tmp, "session-x")
	if err := second.SaveSubagentRecord(SubagentRecord{ID: "r1", AgentType: "researcher", Goal: "g", Status: SubagentStatusRunning, SpawnedAt: time.Now()}); err != nil {
		t.Fatalf("second SaveSubagentRecord: %v", err)
	}

	manifest, err := LoadSubagentManifest("session-x")
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	if len(manifest.Records) != 2 {
		t.Fatalf("records = %d, want 2 (the second write must merge, not clobber)", len(manifest.Records))
	}

	// And MarkSubagentStatus refines the existing record on disk.
	if err := second.MarkSubagentStatus("r0", SubagentStatusCompleted, "", "preview", ""); err != nil {
		t.Fatalf("MarkSubagentStatus: %v", err)
	}
	manifest, err = LoadSubagentManifest("session-x")
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	rec, ok := manifest.Get("r0")
	if !ok || rec.Status != SubagentStatusCompleted {
		t.Fatalf("r0 after MarkSubagentStatus = %+v (found=%v), want completed", rec, ok)
	}
	if rec.ResultPreview != "preview" {
		t.Errorf("ResultPreview = %q, want preview", rec.ResultPreview)
	}
	if rec.EndedAt.IsZero() {
		t.Error("MarkSubagentStatus must stamp EndedAt")
	}
	if rec.Goal != "g" || rec.AgentType != "coder" {
		t.Errorf("terminal write must preserve spawn identity fields, got %+v", rec)
	}
}

func TestSaveSubagentRecordNoopForInMemoryAndUnsafeSessions(t *testing.T) {
	// In-memory session (no history path): never writes anything.
	inMemory := New(nil, "", nil, "prompt", false)
	if err := inMemory.SaveSubagentRecord(SubagentRecord{ID: "r", Status: SubagentStatusRunning, SpawnedAt: time.Now()}); err != nil {
		t.Fatalf("in-memory SaveSubagentRecord = %v, want nil", err)
	}
	if err := inMemory.MarkSubagentStatus("r", SubagentStatusCompleted, "", "", ""); err != nil {
		t.Fatalf("in-memory MarkSubagentStatus = %v, want nil", err)
	}

	// Crafted unsafe history-path base: the manifest must refuse to write
	// outside a valid session folder.
	unsafe := New(nil, "/tmp/../evil.json", nil, "prompt", false)
	if err := unsafe.SaveSubagentRecord(SubagentRecord{ID: "r", Status: SubagentStatusRunning, SpawnedAt: time.Now()}); err != nil {
		t.Fatalf("unsafe SaveSubagentRecord = %v, want a silent no-op", err)
	}
}

func TestSubagentManifestJSONShape(t *testing.T) {
	// The JSON contract is snake_case with omitempty on optionals — the
	// documented on-disk shape resume depends on.
	ended := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	manifest := &SubagentManifest{
		SessionID: "session-x",
		Records: []SubagentRecord{{
			ID:             "coder-subagent-3",
			AgentType:      "coder",
			Goal:           "fix the bug",
			Status:         SubagentStatusFailed,
			SpawnedAt:      ended.Add(-time.Hour),
			EndedAt:        ended,
			TranscriptPath: "/t.md",
			Cause:          "crashed: boom",
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("re-Unmarshal: %v", err)
	}
	if _, ok := generic["session_id"]; !ok {
		t.Error("missing session_id key")
	}
	records, ok := generic["records"].([]any)
	if !ok || len(records) != 1 {
		t.Fatalf("records shape mismatch: %v", generic["records"])
	}
	rec := records[0].(map[string]any)
	for _, key := range []string{"id", "agent_type", "goal", "status", "spawned_at", "ended_at", "transcript_path", "cause"} {
		if _, ok := rec[key]; !ok {
			t.Errorf("missing %s key in %v", key, rec)
		}
	}
	for _, absent := range []string{"ctx_files", "history_path", "result_preview"} {
		if _, ok := rec[absent]; ok {
			t.Errorf("optional key %s must be omitted when empty, got %v", absent, rec)
		}
	}
}

func TestSubagentManifestFolderDisposedWithSession(t *testing.T) {
	tmp := t.TempDir()

	stubSessionDir(t, tmp)

	if err := manifestTestSession(t, tmp, "session-x").SaveSubagentRecord(SubagentRecord{ID: "r0", AgentType: "coder", Goal: "g", Status: SubagentStatusRunning, SpawnedAt: time.Now()}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}
	manifestPath := filepath.Join(tmp, "session-x", "subagents", "manifest.json")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("manifest missing before removal: %v", err)
	}

	if err := RemoveSessionFolder("session-x"); err != nil {
		t.Fatalf("RemoveSessionFolder: %v", err)
	}
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Errorf("manifest must be disposed with the session folder, stat err = %v", err)
	}
}

func TestSubagentStatusConstants(t *testing.T) {
	// The four documented statuses are the manifest's state machine; a typo
	// here would silently break resume's interpretation.
	if SubagentStatusRunning != "running" ||
		SubagentStatusCompleted != "completed" ||
		SubagentStatusFailed != "failed" ||
		SubagentStatusCancelled != "cancelled" {
		t.Fatalf("status constants drifted: %s/%s/%s/%s", SubagentStatusRunning, SubagentStatusCompleted, SubagentStatusFailed, SubagentStatusCancelled)
	}
	for _, status := range []string{SubagentStatusRunning, SubagentStatusCompleted, SubagentStatusFailed, SubagentStatusCancelled} {
		if strings.ContainsAny(status, " \t") {
			t.Errorf("status %q must not contain whitespace", status)
		}
	}
}

// TestMarkSubagentExecutionStampsRecord pins the per-model parallel gate's
// manifest surface: MarkSubagentExecution stamps the EFFECTIVE execution
// mode and the agent_models model reference onto an existing record, the
// stamp survives a reload (persisted), the fields stay omitted for sync
// children (omitempty), and an unknown ID is a no-op — a stamp may refine an
// existing spawn record but never invent one.
func TestMarkSubagentExecutionStampsRecord(t *testing.T) {
	tmp := t.TempDir()
	stubSessionDir(t, tmp)

	sess := manifestTestSession(t, tmp, "session-x")
	if err := sess.SaveSubagentRecord(SubagentRecord{ID: "coder-subagent-0", AgentType: "coder", Goal: "g", Status: SubagentStatusRunning, SpawnedAt: time.Now()}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}

	if err := sess.MarkSubagentExecution("coder-subagent-0", "serial", "local-llama"); err != nil {
		t.Fatalf("MarkSubagentExecution: %v", err)
	}

	manifest, err := LoadSubagentManifest("session-x")
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	rec, ok := manifest.Get("coder-subagent-0")
	if !ok {
		t.Fatal("record vanished")
	}
	if rec.Execution != "serial" || rec.Model != "local-llama" {
		t.Fatalf("stamped record = execution:%q model:%q, want serial/local-llama", rec.Execution, rec.Model)
	}

	// Reload from disk: the stamp persisted.
	data, err := os.ReadFile(filepath.Join(tmp, "session-x", "subagents", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"execution": "serial"`) || !strings.Contains(string(data), `"model": "local-llama"`) {
		t.Fatalf("stamp not persisted:\n%s", data)
	}

	// An unknown ID is a silent no-op.
	if err := manifestTestSession(t, tmp, "session-x").MarkSubagentExecution("ghost", "parallel", "m"); err != nil {
		t.Fatalf("MarkSubagentExecution(ghost) = %v, want a nil no-op", err)
	}
}

// TestSubagentRecordExecutionOmittedForSyncChildren pins that the Execution
// and Model fields stay out of the JSON for records that never reached the
// scheduler (sync spawns) — empty means sync, no model routing.
func TestSubagentRecordExecutionOmittedForSyncChildren(t *testing.T) {
	data, err := json.Marshal(SubagentRecord{ID: "r", AgentType: "coder", Goal: "g", Status: SubagentStatusCompleted, SpawnedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"execution", "model"} {
		if strings.Contains(string(data), key) {
			t.Fatalf("sync record must omit %q, got %s", key, data)
		}
	}
}
