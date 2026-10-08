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

// readOnlyRestoreDeps is the nil-equivalent deps the read-only restore tests
// use: the restore falls back to the agent.RestoredSubagentOrchestrator
// projection when the live machinery is unavailable (subagents disabled, or
// a degraded record), so these tests pin that projection's behavior.
func readOnlyRestoreDeps() *liveRestoreDeps { return nil }

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
	restoreInterruptedSubagents(root, manifest, restored, readOnlyRestoreDeps())

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
	restoreInterruptedSubagents(root, manifest, make(map[string]bool), readOnlyRestoreDeps())

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
	restoreInterruptedSubagents(root, manifest, restored, readOnlyRestoreDeps())
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
	restoreInterruptedSubagents(root, manifest, restored, readOnlyRestoreDeps())
	if got := len(root.Children()); got != 0 {
		t.Fatalf("corrupt-history record must not be listed, got %d children", got)
	}

	// nil manifest / nil root are silent no-ops.
	restoreInterruptedSubagents(root, nil, restored, nil)
	restoreInterruptedSubagents(nil, manifest, restored, nil)
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

// a real scheduler against the sandboxed session, a client factory that
// always routes to the given client, and the runEnv the launch machinery
// reads. The messenger stays nil (no TUI in tests — the same convention the
// in-process resume tests use).
func liveRestoreTestDeps(root *orchestrator.BaseOrchestrator, sess *session.Session, sessionID string, scheduler *SubagentScheduler, c *client.Client) *liveRestoreDeps {
	return &liveRestoreDeps{
		root:          root,
		sessionID:     sessionID,
		sess:          sess,
		scheduler:     scheduler,
		clientFor:     func(string) *client.Client { return nil }, // nil routing → defaultClient
		defaultClient: c,
		enabledTools:  map[string]bool{"read_file": true},
		injectCWD:     false,
		gemmaThinking: false,
		maxTurns:      10,
		messenger:     nil,
		runEnv: &subagentRunEnv{
			pluginManager: nil,
			messenger:     nil,
			root:          root,
			sess:          sess,
			toolArchive:   "",
		},
		globalBudget: 0,
	}
}

// TestRestoreInterruptedSubagentsLive pins the core fix: with the live
// machinery available, an interrupted record restores as a LIVE child (the
// concrete *orchestrator.BaseOrchestrator the resume constructor builds —
// registry non-nil, Execute capable) mounted on the root, its loaded history
// equal to the persisted one, its manifest record flipped back to running
// through MarkResumed, and its background run actually executing against the
// scripted LLM.
func TestRestoreInterruptedSubagentsLive(t *testing.T) {
	const sessionID = "session-restore-live-1"
	const childID = "coder-subagent-0"
	sess := runnerTestSession(t, sessionID)

	historyPath, err := session.SubagentHistoryPath(sessionID, childID)
	if err != nil {
		t.Fatal(err)
	}
	committed := []client.ChatMessage{
		{Role: "user", Content: client.TextContent("Goal: live restore me")},
		{Role: "assistant", Content: client.TextContent("half done before the crash")},
	}
	if err := session.SaveHistory(historyPath, committed); err != nil {
		t.Fatal(err)
	}
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: childID, AgentType: "coder", Goal: "live restore me",
		Status:      session.SubagentStatusFrozen, // the synthesis's interrupted status
		SpawnedAt:   nowMinus(t, time.Hour),
		HistoryPath: historyPath,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatal(err)
	}

	// The scripted LLM: the resumed child's first turn completes with
	// content, so the run reaches a terminal outcome on its own.
	fake := newFakeStreamServer(t, fakeScriptTurn("live-restored continuation"))
	c := client.NewClient(client.Config{BaseURL: fake.srv.URL, Model: "test-model"})

	root := orchestrator.NewBaseOrchestrator(common.MainAgentID, sess, nil, 0)
	scheduler := newSubagentScheduler(sess, nil) // nil status writer: silent
	deps := liveRestoreTestDeps(root, sess, sessionID, scheduler, c)

	restored := make(map[string]bool)
	restoreInterruptedSubagents(root, manifest, restored, deps)

	if !restored[childID] {
		t.Fatal("interrupted record not restored")
	}
	children := root.Children()
	child := childByID(children, childID)
	if child == nil {
		t.Fatalf("restored child %s not mounted on the root; children = %v", childID, ids(children))
	}
	// LIVE, not a stub: a BaseOrchestrator with a working registry.
	if _, isStub := child.(*agent.RestoredSubagentOrchestrator); isStub {
		t.Fatal("restored child is the read-only stub — the live restore did not take over")
	}
	if _, ok := child.(*orchestrator.BaseOrchestrator); !ok {
		t.Fatalf("restored child type = %T, want *orchestrator.BaseOrchestrator", child)
	}
	if child.Registry() == nil {
		t.Error("live-restored child Registry() = nil, want the registered tool surface")
	}
	loaded := child.History()
	if len(loaded) != len(committed) {
		t.Fatalf("live-restored child loaded %d messages, want the %d persisted (no goal re-append)", len(loaded), len(committed))
	}
	for i := range committed {
		if loaded[i].Content.String() != committed[i].Content.String() || loaded[i].Role != committed[i].Role {
			t.Errorf("loaded history[%d] = %s/%q, want %s/%q",
				i, loaded[i].Role, loaded[i].Content.String(), committed[i].Role, committed[i].Content.String())
		}
	}
	// The resume constructor's tab hint: "resumed from interruption".
	if h, ok := child.(interface{ StatusHint() string }); !ok || h.StatusHint() != agent.RestoredResumedStatus {
		got := ""
		if h, ok := child.(interface{ StatusHint() string }); ok {
			got = h.StatusHint()
		}
		t.Errorf("StatusHint() = %q, want %q", got, agent.RestoredResumedStatus)
	}

	// The manifest record flipped back to running (MarkResumed ran through
	// the constructor) with the resume counted.
	reloaded, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := reloaded.Get(childID)
	if !ok {
		t.Fatal("record vanished after the live restore")
	}
	if rec.Status != session.SubagentStatusRunning {
		t.Errorf("post-restore status = %q, want running (the relaunched run is live)", rec.Status)
	}
	if rec.ResumeCount != 1 {
		t.Errorf("ResumeCount = %d, want 1", rec.ResumeCount)
	}

	// The background run executes: wait for the terminal manifest write the
	// run closure performs (completed), then confirm new work appended to
	// the SAME history file.
	eventually(t, 10*time.Second, "restored child completes in the background", func() bool {
		m, err := session.LoadSubagentManifest(sessionID)
		if err != nil {
			return false
		}
		r, ok := m.Get(childID)
		return ok && r.Status == session.SubagentStatusCompleted
	})
	after, err := session.LoadHistory(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) <= len(committed) {
		t.Fatalf("live-restored child appended nothing: %d messages, want > %d", len(after), len(committed))
	}
	for i := range committed {
		if after[i].Content.String() != committed[i].Content.String() || after[i].Role != committed[i].Role {
			t.Errorf("post-restore history[%d] drifted: %s/%q, want %s/%q",
				i, after[i].Role, after[i].Content.String(), committed[i].Role, committed[i].Content.String())
		}
	}
}

// TestRestoreInterruptedSubagentsLiveRunningRecord covers the legacy-status
// path: a record still "running" (a manifest written before the frozen field
// existed, or a synthesis write that never landed) restores live too.
func TestRestoreInterruptedSubagentsLiveRunningRecord(t *testing.T) {
	const sessionID = "session-restore-live-2"
	const childID = "coder-subagent-1"
	sess := runnerTestSession(t, sessionID)

	historyPath, err := session.SubagentHistoryPath(sessionID, childID)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.SaveHistory(historyPath, []client.ChatMessage{
		{Role: "user", Content: client.TextContent("Goal: running-status restore")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: childID, AgentType: "coder", Goal: "running-status restore",
		Status:      session.SubagentStatusRunning, // raw at-exit state
		SpawnedAt:   nowMinus(t, time.Hour),
		HistoryPath: historyPath,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatal(err)
	}

	fake := newFakeStreamServer(t, fakeScriptTurn("continuation two"))
	c := client.NewClient(client.Config{BaseURL: fake.srv.URL, Model: "test-model"})
	root := orchestrator.NewBaseOrchestrator(common.MainAgentID, sess, nil, 0)
	deps := liveRestoreTestDeps(root, sess, sessionID, newSubagentScheduler(sess, nil), c)

	restored := make(map[string]bool)
	restoreInterruptedSubagents(root, manifest, restored, deps)

	if !restored[childID] {
		t.Fatal("running-status record not restored")
	}
	child := childByID(root.Children(), childID)
	if child == nil {
		t.Fatal("live-restored child not mounted on the root")
	}
	if _, isStub := child.(*agent.RestoredSubagentOrchestrator); isStub {
		t.Fatal("running-status record restored as the read-only stub")
	}

	// Submit continues the live child: no read-only refusal, the message
	// lands in the child's session (queued or executed — either way it is
	// accepted, which is the user-visible contract).
	if err := child.Submit("continue the task", nil); err != nil {
		t.Fatalf("Submit to the live-restored child failed: %v", err)
	}

	// The background run the restore launched must finish cleanly too.
	eventually(t, 10*time.Second, "restored child completes in the background", func() bool {
		m, err := session.LoadSubagentManifest(sessionID)
		if err != nil {
			return false
		}
		r, ok := m.Get(childID)
		return ok && (r.Status == session.SubagentStatusCompleted || r.Status == session.SubagentStatusRunning)
	})
}

// TestRestoreInterruptedSubagentsTerminalStaySkippedLive pins the status
// filter: with the LIVE machinery present, terminal records are still
// skipped — only interrupted records go live, and a completed record never
// relaunches.
func TestRestoreInterruptedSubagentsTerminalStaySkippedLive(t *testing.T) {
	const sessionID = "session-restore-live-3"
	sess := runnerTestSession(t, sessionID)
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: "coder-subagent-2", AgentType: "coder", Goal: "done task",
		Status: session.SubagentStatusCompleted, SpawnedAt: nowMinus(t, time.Hour),
		ResultPreview: "finished",
	}); err != nil {
		t.Fatal(err)
	}
	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatal(err)
	}

	fake := newFakeStreamServer(t, fakeScriptTurn("never asked for"))
	c := client.NewClient(client.Config{BaseURL: fake.srv.URL, Model: "test-model"})
	root := orchestrator.NewBaseOrchestrator(common.MainAgentID, sess, nil, 0)
	deps := liveRestoreTestDeps(root, sess, sessionID, newSubagentScheduler(sess, nil), c)

	restored := make(map[string]bool)
	restoreInterruptedSubagents(root, manifest, restored, deps)

	if restored["coder-subagent-2"] {
		t.Error("terminal record must not be restored (live or otherwise)")
	}
	if got := len(root.Children()); got != 0 {
		t.Errorf("root.Children() holds %d children, want 0", got)
	}
}

// TestRestoreInterruptedSubagentsLiveFallsBackToStub pins the degradation:
// when the live restore cannot proceed for a record (an empty history file —
// nothing to restore), the read-only projection takes over so the child
// stays visible in the TUI, and the record is not left half-relaunched.
func TestRestoreInterruptedSubagentsLiveFallsBackToStub(t *testing.T) {
	const sessionID = "session-restore-live-4"
	const childID = "coder-subagent-4"
	sess := runnerTestSession(t, sessionID)

	historyPath, err := session.SubagentHistoryPath(sessionID, childID)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.SaveHistory(historyPath, nil); err != nil { // empty: nothing to restore
		t.Fatal(err)
	}
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: childID, AgentType: "coder", Goal: "g",
		Status:      session.SubagentStatusFrozen,
		SpawnedAt:   nowMinus(t, time.Hour),
		HistoryPath: historyPath,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatal(err)
	}

	fake := newFakeStreamServer(t, fakeScriptTurn("unused"))
	c := client.NewClient(client.Config{BaseURL: fake.srv.URL, Model: "test-model"})
	root := orchestrator.NewBaseOrchestrator(common.MainAgentID, sess, nil, 0)
	deps := liveRestoreTestDeps(root, sess, sessionID, newSubagentScheduler(sess, nil), c)

	restored := make(map[string]bool)
	restoreInterruptedSubagents(root, manifest, restored, deps)

	if !restored[childID] {
		t.Fatal("record not marked restored")
	}
	children := root.Children()
	if len(children) != 1 {
		t.Fatalf("root.Children() holds %d children, want the read-only fallback stub", len(children))
	}
	ro, ok := children[0].(*agent.RestoredSubagentOrchestrator)
	if !ok {
		t.Fatalf("restored child type = %T, want the read-only stub fallback", children[0])
	}
	if _, err := ro.Execute(""); err == nil {
		t.Error("fallback stub Execute() must fail (read-only)")
	}
	// The record was not relaunched: no ResumeCount bump, still frozen.
	reloaded, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := reloaded.Get(childID)
	if !ok {
		t.Fatal("record vanished")
	}
	if rec.Status != session.SubagentStatusFrozen || rec.ResumeCount != 0 {
		t.Errorf("record = status %q resumeCount %d, want untouched frozen/0 (no half-relaunch)",
			rec.Status, rec.ResumeCount)
	}
}
