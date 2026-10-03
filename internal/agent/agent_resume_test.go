package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"late/internal/client"
	"late/internal/common"
	"late/internal/orchestrator"
	"late/internal/session"
)

// StubTool is a minimal registered tool for registry-inheritance
// assertions.
type StubTool struct{}

func (t StubTool) Name() string                              { return "parent_tool" }
func (t StubTool) Description() string                       { return "stub" }
func (t StubTool) Parameters() json.RawMessage               { return json.RawMessage(`{}`) }
func (t StubTool) RequiresConfirmation(json.RawMessage) bool { return false }
func (t StubTool) CallString(json.RawMessage) string         { return "stub" }

func (t StubTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "stub result", nil
}

// TestSubagentRecord_WorkingDirWorktreeResumeCountRoundTrip pins the Phase C
// manifest additions: the new fields serialize into the manifest JSON and
// come back byte-identical through the load-modify-save path.
func TestSubagentRecord_WorkingDirWorktreeResumeCountRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	setSessionDirForTest(t, tmp)

	const sessionID = "session-phasec-fields"
	mockSession := session.New(nil, filepath.Join(tmp, sessionID+".json"), nil, "prompt", false)

	rec := session.SubagentRecord{
		ID:           "coder-subagent-4",
		AgentType:    "coder",
		Goal:         "port the module",
		Status:       session.SubagentStatusRunning,
		SpawnedAt:    time.Now().Truncate(time.Second),
		HistoryPath:  filepath.Join(tmp, sessionID, "subagents", "coder-subagent-4.json"),
		WorkingDir:   "/projects/late",
		WorktreePath: "/projects/late-worktrees/feature-x",
		ResumeCount:  2,
	}
	if err := mockSession.SaveSubagentRecord(rec); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}

	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	got, ok := manifest.Get("coder-subagent-4")
	if !ok {
		t.Fatalf("record lost; records = %+v", manifest.Records)
	}
	if got.WorkingDir != "/projects/late" {
		t.Errorf("WorkingDir = %q, want /projects/late", got.WorkingDir)
	}
	if got.WorktreePath != "/projects/late-worktrees/feature-x" {
		t.Errorf("WorktreePath = %q, want the worktree path", got.WorktreePath)
	}
	if got.ResumeCount != 2 {
		t.Errorf("ResumeCount = %d, want 2", got.ResumeCount)
	}

	// The fields must actually be in the JSON (not silently dropped).
	data, err := os.ReadFile(filepath.Join(tmp, sessionID, "subagents", "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	for _, fragment := range []string{`"working_dir"`, `"worktree_path"`, `"resume_count"`} {
		if !strings.Contains(string(data), fragment) {
			t.Errorf("manifest JSON missing %s:\n%s", fragment, data)
		}
	}

	// Old records without the fields load as zero values.
	legacy := `{"session_id":"` + sessionID + `","records":[{"id":"old-1","agent_type":"coder","goal":"g","status":"completed","spawned_at":"2025-01-01T10:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(tmp, sessionID, "subagents", "manifest.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err = session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("LoadSubagentManifest(legacy): %v", err)
	}
	oldRec, ok := manifest.Get("old-1")
	if !ok {
		t.Fatal("legacy record lost")
	}
	if oldRec.WorkingDir != "" || oldRec.WorktreePath != "" || oldRec.ResumeCount != 0 {
		t.Errorf("legacy record should carry zero values, got %+v", oldRec)
	}
}

// TestMarkSubagentResumed_BumpsCountAndReopensRecord pins the resume
// bookkeeping: ResumeCount increments, status flips back to running, and the
// previous termination stamps are cleared.
func TestMarkSubagentResumed_BumpsCountAndReopensRecord(t *testing.T) {
	tmp := t.TempDir()
	setSessionDirForTest(t, tmp)

	const sessionID = "session-phasec-resumed"
	sess := session.New(nil, filepath.Join(tmp, sessionID+".json"), nil, "prompt", false)

	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: "coder-subagent-1", AgentType: "coder", Goal: "g",
		Status: session.SubagentStatusFailed, Cause: "crashed: boom",
		SpawnedAt: time.Now().Add(-time.Hour), EndedAt: time.Now().Add(-time.Minute),
		WorkingDir: "/w", WorktreePath: "/wt",
	}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}

	if err := sess.MarkSubagentResumed("coder-subagent-1"); err != nil {
		t.Fatalf("MarkSubagentResumed: %v", err)
	}

	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	rec, ok := manifest.Get("coder-subagent-1")
	if !ok {
		t.Fatal("record lost")
	}
	if rec.Status != session.SubagentStatusRunning {
		t.Errorf("Status = %q, want running (the resumed run is live again)", rec.Status)
	}
	if rec.ResumeCount != 1 {
		t.Errorf("ResumeCount = %d, want 1", rec.ResumeCount)
	}
	if !rec.EndedAt.IsZero() {
		t.Errorf("EndedAt = %v, want cleared", rec.EndedAt)
	}
	if rec.Cause != "" {
		t.Errorf("Cause = %q, want cleared", rec.Cause)
	}
	// Identity fields survive.
	if rec.WorkingDir != "/w" || rec.WorktreePath != "/wt" || rec.Goal != "g" {
		t.Errorf("identity fields lost: %+v", rec)
	}

	// Unknown IDs are a no-op, and an in-memory session never errors.
	if err := sess.MarkSubagentResumed("does-not-exist"); err != nil {
		t.Errorf("MarkSubagentResumed(unknown) = %v, want nil", err)
	}
	inMemory := session.New(nil, "", nil, "prompt", false)
	if err := inMemory.MarkSubagentResumed("coder-subagent-1"); err != nil {
		t.Errorf("MarkSubagentResumed(in-memory) = %v, want nil", err)
	}
}

// TestNewResumedSubagentOrchestrator_LoadsHistoryAndRestores pins the live
// resume constructor: same record ID, the persisted history is loaded
// (NOT re-appended with a goal message), the registry is inherited minus
// the orchestrator-only tools, the client is wired into the session, and
// the child is added to the parent.
func TestNewResumedSubagentOrchestrator_LoadsHistoryAndRestores(t *testing.T) {
	tmp := t.TempDir()
	setSessionDirForTest(t, tmp)

	const sessionID = "session-phasec-live"
	historyPath := filepath.Join(tmp, sessionID, "subagents", "coder-subagent-3.json")
	preserved := []client.ChatMessage{
		{Role: "user", Content: client.TextContent("Goal: half-done work")},
		{Role: "assistant", Content: client.TextContent("I did the first half.")},
		{Role: "tool", ToolCallID: "call_1", Content: client.TextContent("tool result")},
	}
	if err := os.MkdirAll(filepath.Dir(historyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := session.SaveHistory(historyPath, preserved); err != nil {
		t.Fatalf("SaveHistory: %v", err)
	}

	c := client.NewClient(client.Config{BaseURL: "http://localhost:8080"})
	parentSess := session.New(c, filepath.Join(tmp, sessionID+".json"), nil, "parent prompt", true)
	parent := orchestrator.NewBaseOrchestrator(common.MainAgentID, parentSess, nil, 0)
	parent.Registry().Register(StubTool{})

	record := session.SubagentRecord{
		ID:          "coder-subagent-3",
		AgentType:   "coder",
		Goal:        "half-done work",
		Status:      session.SubagentStatusRunning,
		HistoryPath: historyPath,
		WorkingDir:  "/projects/late",
	}
	// The spawn-time record must exist in the manifest: MarkSubagentResumed
	// only refines existing records, never invents them.
	if err := parentSess.SaveSubagentRecord(record); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}

	child, id, err := NewResumedSubagentOrchestrator(c, record, "coder", map[string]bool{"bash": true, "read_file": true}, false, false, 10, parent, nil)
	if err != nil {
		t.Fatalf("NewResumedSubagentOrchestrator: %v", err)
	}
	if id != "coder-subagent-3" {
		t.Errorf("returned id = %q, want the record's exact id", id)
	}
	if child.ID() != "coder-subagent-3" {
		t.Errorf("child.ID() = %q, want the record's exact id", child.ID())
	}

	// History loaded, not goal-reappended: exactly the preserved messages.
	if got := len(child.History()); got != len(preserved) {
		t.Fatalf("child history holds %d messages, want %d (no goal re-append)", got, len(preserved))
	}
	if child.History()[0].Content.String() != "Goal: half-done work" {
		t.Errorf("first message = %q, want the preserved goal", child.History()[0].Content.String())
	}
	if child.History()[len(preserved)-1].Role != "tool" {
		t.Errorf("last message role = %q, want the preserved tool result", child.History()[len(preserved)-1].Role)
	}

	// Registry non-nil, inherited from the parent minus the
	// orchestrator-only tools, plus the config's allowed tools.
	reg := child.Registry()
	if reg == nil {
		t.Fatal("child registry is nil")
	}
	if reg.Get("parent_tool") == nil {
		t.Error("parent-inherited tool missing from the child registry")
	}
	if reg.Get("spawn_subagent") != nil {
		t.Error("spawn_subagent must not be inherited by a resumed child")
	}
	if reg.Get("bash") == nil || reg.Get("read_file") == nil {
		t.Error("config allowed tools missing from the resumed child registry")
	}

	// The client is wired into the resumed session.
	if bo, ok := child.(*orchestrator.BaseOrchestrator); !ok || bo.Session() == nil || bo.Session().Client() != c {
		t.Error("resumed session client not wired")
	}

	// The child was added to the parent.
	found := false
	for _, ch := range parent.Children() {
		if ch.ID() == "coder-subagent-3" {
			found = true
		}
	}
	if !found {
		t.Error("resumed child was not added to the parent")
	}

	// The manifest counted the resume (the parent session writes through).
	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	rec, ok := manifest.Get("coder-subagent-3")
	if !ok {
		t.Fatal("manifest record missing after resume")
	}
	if rec.ResumeCount != 1 || rec.Status != session.SubagentStatusRunning {
		t.Errorf("manifest after resume = %+v, want resume_count 1 / status running", rec)
	}
}

// TestNewResumedSubagentOrchestrator_SameIDCollisionGuard pins the guard:
// when the root already lists a child with the record's ID (restored then
// resumed within one session), the live twin is suffixed "-r1" so the
// parent never holds two children with the same ID.
func TestNewResumedSubagentOrchestrator_SameIDCollisionGuard(t *testing.T) {
	tmp := t.TempDir()
	setSessionDirForTest(t, tmp)

	const sessionID = "session-phasec-collision"
	historyPath := filepath.Join(tmp, sessionID, "subagents", "coder-subagent-6.json")
	if err := os.MkdirAll(filepath.Dir(historyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := session.SaveHistory(historyPath, []client.ChatMessage{
		{Role: "user", Content: client.TextContent("Goal: g")},
	}); err != nil {
		t.Fatal(err)
	}

	c := client.NewClient(client.Config{BaseURL: "http://localhost:8080"})
	parentSess := session.New(c, filepath.Join(tmp, sessionID+".json"), nil, "prompt", false)
	parent := orchestrator.NewBaseOrchestrator(common.MainAgentID, parentSess, nil, 0)

	// Simulate the TUI restore: a read-only child already occupies the ID.
	stale, err := NewRestoredSubagentOrchestrator("coder-subagent-6", "coder", "g", historyPath, "")
	if err != nil {
		t.Fatalf("NewRestoredSubagentOrchestrator: %v", err)
	}
	parent.AddChild(stale)

	record := session.SubagentRecord{ID: "coder-subagent-6", AgentType: "coder", Status: session.SubagentStatusRunning, HistoryPath: historyPath}
	_, id, err := NewResumedSubagentOrchestrator(c, record, "coder", map[string]bool{}, false, false, 10, parent, nil)
	if err != nil {
		t.Fatalf("NewResumedSubagentOrchestrator: %v", err)
	}
	if id != "coder-subagent-6-r1" {
		t.Errorf("id = %q, want the collision-renamed coder-subagent-6-r1", id)
	}

	seen := make(map[string]int)
	for _, ch := range parent.Children() {
		seen[ch.ID()]++
	}
	for childID, n := range seen {
		if n > 1 {
			t.Errorf("parent holds %d children with duplicate id %q", n, childID)
		}
	}
}

// TestNewResumedSubagentOrchestrator_WorktreeAndWorkingDirPrompt pins the
// CWD precedence on resume: an explicit worktree wins over the recorded
// worktree, which wins over the recorded spawn-time WorkingDir; with none
// recorded the current process CWD applies.
func TestNewResumedSubagentOrchestrator_WorktreeAndWorkingDirPrompt(t *testing.T) {
	tmp := t.TempDir()
	setSessionDirForTest(t, tmp)

	const sessionID = "session-phasec-cwd"
	historyPath := filepath.Join(tmp, sessionID, "subagents", "researcher-subagent-2.json")
	if err := os.MkdirAll(filepath.Dir(historyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := session.SaveHistory(historyPath, []client.ChatMessage{
		{Role: "user", Content: client.TextContent("Goal: research")},
	}); err != nil {
		t.Fatal(err)
	}

	c := client.NewClient(client.Config{BaseURL: "http://localhost:8080"})
	parentSess := session.New(c, filepath.Join(tmp, sessionID+".json"), nil, "prompt", false)
	parent := orchestrator.NewBaseOrchestrator(common.MainAgentID, parentSess, nil, 0)

	cases := []struct {
		name       string
		record     session.SubagentRecord
		wantSubstr string
	}{
		{
			name:       "worktree path wins over working dir",
			record:     session.SubagentRecord{ID: "researcher-subagent-2", AgentType: "researcher", Status: session.SubagentStatusRunning, HistoryPath: historyPath, WorkingDir: "/from/working-dir", WorktreePath: "/from/worktree"},
			wantSubstr: "/from/worktree",
		},
		{
			name:       "working dir used without a worktree",
			record:     session.SubagentRecord{ID: "researcher-subagent-2", AgentType: "researcher", Status: session.SubagentStatusRunning, HistoryPath: historyPath, WorkingDir: "/from/working-dir"},
			wantSubstr: "/from/working-dir",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			child, _, err := NewResumedSubagentOrchestrator(c, tc.record, "researcher", map[string]bool{}, true, false, 10, parent, nil)
			if err != nil {
				t.Fatalf("NewResumedSubagentOrchestrator: %v", err)
			}
			if !strings.Contains(child.SystemPrompt(), tc.wantSubstr) {
				t.Errorf("system prompt does not contain %q:\n%.200s", tc.wantSubstr, child.SystemPrompt())
			}
		})
	}
}

// TestNewSubagentOrchestrator_WorktreeSpawnRecordsWorktreePath pins the
// fresh worktree spawn: the worktree replaces ${{CWD}} in the prompt and
// lands in the manifest record's WorktreePath (so a later resume restores
// the same working environment).
func TestNewSubagentOrchestrator_WorktreeSpawnRecordsWorktreePath(t *testing.T) {
	tmp := t.TempDir()
	setSessionDirForTest(t, tmp)

	cfg := client.Config{BaseURL: "http://localhost:8080"}
	c := client.NewClient(cfg)

	mockSession := session.New(c, filepath.Join(tmp, "session-wt.json"), []client.ChatMessage{}, "prompt", true)
	parent := orchestrator.NewBaseOrchestrator("parent", mockSession, nil, 10)

	child, err := NewSubagentOrchestratorWithWorktree(
		c, "goal", nil, "researcher", map[string]bool{}, true, false, 10,
		"session-wt", true, "/repo-worktrees/feature-x", parent, nil,
	)
	if err != nil {
		t.Fatalf("NewSubagentOrchestratorWithWorktree: %v", err)
	}
	if !strings.Contains(child.SystemPrompt(), "/repo-worktrees/feature-x") {
		t.Errorf("system prompt must carry the worktree as ${{CWD}}, got:\n%.200s", child.SystemPrompt())
	}

	manifest, err := session.LoadSubagentManifest("session-wt")
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	rec, ok := manifest.Get("researcher-subagent-0")
	if !ok {
		t.Fatalf("no manifest record; records = %+v", manifest.Records)
	}
	if rec.WorktreePath != "/repo-worktrees/feature-x" {
		t.Errorf("WorktreePath = %q, want the worktree path", rec.WorktreePath)
	}
	if rec.WorkingDir == "" {
		t.Error("WorkingDir must still be captured at spawn")
	}
}

// TestNewResumedSubagentOrchestrator_TypeMismatch pins the guard: a resume
// whose agent_type disagrees with the record is refused — the record is the
// source of truth for what kind of agent the history belongs to.
func TestNewResumedSubagentOrchestrator_TypeMismatch(t *testing.T) {
	tmp := t.TempDir()
	setSessionDirForTest(t, tmp)

	c := client.NewClient(client.Config{BaseURL: "http://localhost:8080"})
	parentSess := session.New(c, filepath.Join(tmp, "session-x.json"), nil, "prompt", false)
	parent := orchestrator.NewBaseOrchestrator(common.MainAgentID, parentSess, nil, 0)

	record := session.SubagentRecord{ID: "coder-subagent-1", AgentType: "coder", Status: session.SubagentStatusRunning}
	if _, _, err := NewResumedSubagentOrchestrator(c, record, "researcher", map[string]bool{}, false, false, 10, parent, nil); err == nil {
		t.Fatal("resume with a mismatched agent type must fail")
	}
	if _, _, err := NewResumedSubagentOrchestrator(c, session.SubagentRecord{ID: ""}, "coder", map[string]bool{}, false, false, 10, parent, nil); err == nil {
		t.Fatal("resume with an empty record ID must fail")
	}
}

// TestNewResumedSubagentOrchestrator_EmptyHistoryRefused pins the
// deleted-history edge case: a record whose history file was removed by hand
// (or never got its bytes) loads as an EMPTY conversation, and a live resume
// of it would run a child with no goal at all — the loaded history is the
// child's only task statement, because resume never re-appends the goal. The
// constructor must refuse with the spawn-fresh hint instead of restoring a
// task-less agent, and must not add the child to the parent.
func TestNewResumedSubagentOrchestrator_EmptyHistoryRefused(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T) string{
		// The file was deleted after the manifest recorded it.
		"missing history file": func(t *testing.T) string {
			t.Helper()
			return filepath.Join(t.TempDir(), "subagents", "coder-subagent-9.json")
		},
		// The file exists but holds zero messages (a manually emptied or
		// truncated file; LoadHistory treats it like missing).
		"empty history file": func(t *testing.T) string {
			t.Helper()
			path := filepath.Join(t.TempDir(), "subagents", "coder-subagent-9.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		},
	} {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			setSessionDirForTest(t, tmp)

			historyPath := setup(t)
			c := client.NewClient(client.Config{BaseURL: "http://localhost:8080"})
			parentSess := session.New(c, filepath.Join(tmp, "session-empty-hist.json"), nil, "parent prompt", true)
			parent := orchestrator.NewBaseOrchestrator(common.MainAgentID, parentSess, nil, 0)

			record := session.SubagentRecord{
				ID:          "coder-subagent-9",
				AgentType:   "coder",
				Goal:        "half-done work",
				Status:      session.SubagentStatusRunning,
				HistoryPath: historyPath,
				WorkingDir:  "/projects/late",
			}
			if err := parentSess.SaveSubagentRecord(record); err != nil {
				t.Fatalf("SaveSubagentRecord: %v", err)
			}

			child, id, err := NewResumedSubagentOrchestrator(c, record, "coder", map[string]bool{}, false, false, 10, parent, nil)
			if err == nil {
				t.Fatal("resuming a child with no preserved history must fail")
			}
			if child != nil {
				t.Errorf("child = %v, want nil", child)
			}
			if id != "" {
				t.Errorf("id = %q, want empty on failure", id)
			}
			if !strings.Contains(err.Error(), "empty or missing") || !strings.Contains(err.Error(), "spawn a fresh agent") {
				t.Errorf("err = %v, want the empty-history wording with the spawn-fresh hint", err)
			}
			if n := len(parent.Children()); n != 0 {
				t.Errorf("parent holds %d children, want 0 (the refused child must not be wired in)", n)
			}
		})
	}
}

// TestNewSubagentOrchestrator_UnreadableCtxFileAnnotated pins the goal
// message contract for unreadable context files: a ctx_file whose read fails
// (missing between validation and spawn, permission, directory) is NAMED in
// the initial message instead of being silently dropped, so the child knows
// the context it was promised is absent.
func TestNewSubagentOrchestrator_UnreadableCtxFileAnnotated(t *testing.T) {
	tmp := t.TempDir()
	setSessionDirForTest(t, tmp)

	dir := filepath.Join(tmp, "docs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := client.Config{BaseURL: "http://localhost:8080"}
	c := client.NewClient(cfg)
	mockSession := session.New(c, filepath.Join(tmp, "session-ctx.json"), []client.ChatMessage{}, "prompt", true)
	parent := orchestrator.NewBaseOrchestrator("parent", mockSession, nil, 10)

	child, err := NewSubagentOrchestrator(
		c, "refactor the parser", []string{filepath.Join(tmp, "missing.md"), dir}, "researcher",
		map[string]bool{}, false, false, 10, "session-ctx", true, parent, nil,
	)
	if err != nil {
		t.Fatalf("NewSubagentOrchestrator: %v", err)
	}

	first := child.History()[0].Content.String()
	if !strings.Contains(first, "Goal: refactor the parser") {
		t.Errorf("initial message lost the goal:\n%s", first)
	}
	if !strings.Contains(first, "Context Files:") {
		t.Errorf("initial message lost the context-file section:\n%s", first)
	}
	for _, want := range []string{
		filepath.Join(tmp, "missing.md"),
		"could not be read",
		dir + ": (could not be read:",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("initial message must annotate the unreadable file, want substring %q in:\n%s", want, first)
		}
	}
	// The directory annotation must name the underlying reason, not a bare path.
	if !strings.Contains(first, "is a directory") {
		t.Errorf("directory ctx_file annotation must carry the read error, got:\n%s", first)
	}
}
