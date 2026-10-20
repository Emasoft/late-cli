package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"late/internal/client"
	"late/internal/common"
	"late/internal/orchestrator"
	"late/internal/session"
	"late/internal/tool"
)

// The scheduler-snapshot reconcile (Step 4): a resume loads the persisted
// scheduler queue snapshot, uses it ONLY to reorder (and re-mode) the
// relaunch batch — serial FIFO order, parallel batch membership — and then
// CONSUMES the file, so a second restore pass never reorders against a stale
// snapshot. (The file itself re-appears afterwards: the scheduler's own
// persistence hooks rewrite a fresh snapshot for the resumed queue; the
// stale-order consumption contract is pinned directly in
// TestConsumeSchedulerSnapshotForRestore.)

// childHistoryPath returns the sandboxed history path convention the other
// restore tests use.
func childHistoryPath(t *testing.T, sess *session.Session, sessionID, childID string) string {
	t.Helper()
	return filepath.Join(filepath.Dir(sess.HistoryPath), sessionID, "subagents", childID+".json")
}

// hangingChildrenDeps builds restore deps whose children each stream against
// a HANGING fake server, so a launched child stays genuinely mid-run until
// its server is closed (the crash suite's convention) — making scheduler
// start/queue states observable without sleeps. agentType→client routing is
// single-valued because the two agent types map to two distinct servers.
func newHangingChildServer(t *testing.T) *client.Client {
	t.Helper()
	srv := newHangingStreamServer(t)
	return client.NewClient(client.Config{BaseURL: srv.srv.URL, Model: "test-model"})
}

// TestConsumeSchedulerSnapshotForRestore pins the pure reconcile half: the
// snapshot's serial order and parallel batch membership reorder the batch
// and select modes, unknown children keep manifest order at the back, and
// the snapshot file is REMOVED (consumed) by the load itself.
func TestConsumeSchedulerSnapshotForRestore(t *testing.T) {
	const sessionID = "session-restore-snapshot-consume-1"
	runnerTestSession(t, sessionID)

	// Persist A, B serial, C, D parallel, E mid-flight (running), F unknown.
	if err := session.SaveSchedulerState(sessionID, session.SchedulerState{
		SerialQueue:   []string{"a", "b"},
		ParallelQueue: []string{"c", "d"},
		Running:       []string{"e"},
	}); err != nil {
		t.Fatalf("SaveSchedulerState: %v", err)
	}

	// Manifest order deliberately inverts every preference the snapshot
	// records: b, d, c, a, f, e.
	manifestOrder := []string{"b", "d", "c", "a", "f", "e"}
	batch := make([]*session.SubagentRecord, 0, len(manifestOrder))
	for _, id := range manifestOrder {
		batch = append(batch, &session.SubagentRecord{ID: id})
	}

	got, modes, err := consumeSchedulerSnapshotForRestore(sessionID, batch)
	if err != nil {
		t.Fatalf("consumeSchedulerSnapshotForRestore: %v", err)
	}

	wantOrder := []string{"e", "a", "b", "c", "d", "f"} // running, serial FIFO, batch, manifest-order tail
	gotOrder := make([]string, 0, len(got))
	for _, rec := range got {
		gotOrder = append(gotOrder, rec.ID)
	}
	if strings.Join(gotOrder, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("reordered batch = %v, want %v", gotOrder, wantOrder)
	}
	wantModes := map[string]string{
		"e": tool.SubagentExecutionParallel, // running slot keeps the live-resume path
		"a": tool.SubagentExecutionSerial,
		"b": tool.SubagentExecutionSerial,
		"c": tool.SubagentExecutionParallel,
		"d": tool.SubagentExecutionParallel,
	}
	for id, want := range wantModes {
		if got := restoreLaunchMode(modes, id); got != want {
			t.Errorf("mode of %s = %q, want %q", id, got, want)
		}
	}
	// Unknown child falls through exactly as today: the historical parallel
	// relaunch, manifest order preserved.
	if got := restoreLaunchMode(modes, "f"); got != tool.SubagentExecutionParallel {
		t.Errorf("mode of unknown child = %q, want parallel", got)
	}

	// Consumed: the file is gone so a second pass reorders nothing.
	if loaded, err := session.LoadSchedulerState(sessionID); err != nil {
		t.Fatalf("reload consumed snapshot: %v", err)
	} else if len(loaded.SerialQueue)+len(loaded.ParallelQueue)+len(loaded.Running) != 0 {
		t.Errorf("snapshot file survived consumption: %+v", loaded)
	}

	// A second pass over the original manifest order is a no-op.
	again := make([]*session.SubagentRecord, 0, len(manifestOrder))
	for _, id := range manifestOrder {
		again = append(again, &session.SubagentRecord{ID: id})
	}
	got2, modes2, _ := consumeSchedulerSnapshotForRestore(sessionID, again)
	got2Order := make([]string, 0, len(got2))
	for _, rec := range got2 {
		got2Order = append(got2Order, rec.ID)
	}
	if strings.Join(got2Order, ",") != strings.Join(manifestOrder, ",") {
		t.Errorf("second pass reordered without a snapshot: %v", got2Order)
	}
	if len(modes2) != 0 {
		t.Errorf("second pass returned modes %+v, want none", modes2)
	}
}

// TestRestoreInterruptedSubagentsReorderedFromSnapshot restores a batch whose
// persisted crash snapshot says serial a, b — parallel batch c, d — while the
// MANIFEST order (d, c, b, a) inverts every preference, and asserts the
// reorder through the REAL scheduler: the serial head a runs alone and b, c,
// d queue behind it in the snapshot's own order (b first in the serial FIFO,
// c before d in the parallel batch), and the snapshot is consumed.
func TestRestoreInterruptedSubagentsReorderedFromSnapshot(t *testing.T) {
	const sessionID = "session-restore-snapshot-order-1"
	sess := runnerTestSession(t, sessionID)

	histories := map[string]string{}
	for _, id := range []string{"sub-a", "sub-b", "sub-c", "sub-d"} {
		histories[id] = childHistoryPath(t, sess, sessionID, id)
	}
	for _, rec := range []session.SubagentRecord{
		{ID: "sub-d", AgentType: "coder", Goal: "goal d", Status: session.SubagentStatusFrozen, SpawnedAt: nowMinus(t, time.Hour), HistoryPath: histories["sub-d"]},
		{ID: "sub-c", AgentType: "coder", Goal: "goal c", Status: session.SubagentStatusFrozen, SpawnedAt: nowMinus(t, time.Hour), HistoryPath: histories["sub-c"]},
		{ID: "sub-b", AgentType: "researcher", Goal: "goal b", Status: session.SubagentStatusQueued, SpawnedAt: nowMinus(t, time.Hour), HistoryPath: histories["sub-b"]},
		{ID: "sub-a", AgentType: "coder", Goal: "goal a", Status: session.SubagentStatusFrozen, SpawnedAt: nowMinus(t, 2*time.Hour), HistoryPath: histories["sub-a"]},
	} {
		// Save the manifest record first: its write creates the subagents
		// directory the history file needs.
		if err := sess.SaveSubagentRecord(rec); err != nil {
			t.Fatalf("SaveSubagentRecord(%s): %v", rec.ID, err)
		}
		if err := session.SaveHistory(histories[rec.ID], []client.ChatMessage{
			{Role: "user", Content: client.TextContent("Goal: " + rec.Goal)},
			{Role: "assistant", Content: client.TextContent("halfway through " + rec.Goal)},
		}); err != nil {
			t.Fatalf("SaveHistory(%s): %v", rec.ID, err)
		}
	}
	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatal(err)
	}

	// Persisted crash snapshot: a, b serially queued, c, d one batch.
	if err := session.SaveSchedulerState(sessionID, session.SchedulerState{
		SerialQueue:   []string{"sub-a", "sub-b"},
		ParallelQueue: []string{"sub-c", "sub-d"},
	}); err != nil {
		t.Fatalf("SaveSchedulerState: %v", err)
	}

	// One hanging server per agent type: the coders (a, c, d — only a ever
	// streams, the rest queue) and the researcher b are kept mid-run by
	// their servers, which pins the launch/queue states deterministically.
	clients := map[string]*client.Client{
		"coder":      newHangingChildServer(t),
		"researcher": newHangingChildServer(t),
	}

	root := orchestrator.NewBaseOrchestrator(common.MainAgentID, sess, nil, 0)
	sched := newSubagentScheduler(sess, nil)
	sched.sessionID = sessionID
	deps := liveRestoreTestDeps(root, sess, sessionID, sched, clients["coder"])
	deps.clientFor = func(agentType string) *client.Client { return clients[agentType] }

	restored := make(map[string]bool)
	restoreInterruptedSubagents(root, manifest, restored, deps)
	for _, id := range []string{"sub-a", "sub-b", "sub-c", "sub-d"} {
		if !restored[id] {
			t.Fatalf("child %s not restored", id)
		}
	}

	// Scheduler per-child state strings read "<kind> (<mode>… position N)>".
	live := func(id, mode string) bool { return strings.Contains(sched.State(id), "running ("+mode+")") }
	queuedSerial := func(id, pos string) bool {
		s := sched.State(id)
		return strings.HasPrefix(s, "queued") && strings.Contains(s, tool.SubagentExecutionSerial+", position "+pos)
	}
	queuedParallel := func(id, pos string) bool {
		s := sched.State(id)
		return strings.HasPrefix(s, "queued") && strings.Contains(s, tool.SubagentExecutionParallel+", position "+pos)
	}

	// The snapshot's serial order: a launches alone; b queues next in the
	// serial FIFO (not the manifest order, which had d first).
	eventually(t, 10*time.Second, "serial head a running, b queued serial #1", func() bool {
		return live("sub-a", tool.SubagentExecutionSerial) && queuedSerial("sub-b", "1")
	})

	// The parallel batch queues as ONE batch — c (position 1) before d
	// (position 2), the snapshot's batch order, not the manifest's.
	if !queuedParallel("sub-c", "1") || !queuedParallel("sub-d", "2") {
		t.Errorf("batch queue = c:%q d:%q, want c position 1, d position 2 (snapshot batch order)", sched.State("sub-c"), sched.State("sub-d"))
	}
	// And the restored MODES follow the snapshot slots: a, b serial — c, d
	// parallel — even though the historical relaunch was always parallel.
	if sched.State("sub-a") == "" || !strings.Contains(sched.State("sub-a"), tool.SubagentExecutionSerial) {
		t.Errorf("a relaunches %q, want the snapshot's serial slot", sched.State("sub-a"))
	}

	// The pre-restore snapshot was consumed and the scheduler's own
	// persistence hooks wrote a fresh one describing the resumed queue:
	// serial [b], parallel [c, d], running {a} — the old serial head a is
	// recorded OUT of the queue (it launched), the FIFO tail b remains in
	// order, and nothing of the pre-restore snapshot survives.
	fresh, err := session.LoadSchedulerState(sessionID)
	if err != nil {
		t.Fatalf("reload fresh snapshot: %v", err)
	}
	if strings.Join(fresh.SerialQueue, ",") != "sub-b" ||
		strings.Join(fresh.ParallelQueue, ",") != "sub-c,sub-d" {
		t.Errorf("fresh snapshot = %+v, want serial [sub-b], parallel [sub-c sub-d]", fresh)
	}
}

// TestRestoreInterruptedSubagentsBatchRunsConcurrently restores a batch whose
// snapshot holds ONLY a parallel pair (c, d): both enqueue into the same
// parallel batch and the scheduler launches the whole batch at once — asserted
// by both children being LIVE simultaneously while parked on their hanging
// servers (single-child-at-a-time would leave one drained and one queued).
func TestRestoreInterruptedSubagentsBatchRunsConcurrently(t *testing.T) {
	const sessionID = "session-restore-batch-concurrent-1"
	sess := runnerTestSession(t, sessionID)

	histories := map[string]string{}
	for _, id := range []string{"sub-c", "sub-d"} {
		histories[id] = childHistoryPath(t, sess, sessionID, id)
	}
	for _, rec := range []session.SubagentRecord{
		{ID: "sub-d", AgentType: "researcher", Goal: "goal d", Status: session.SubagentStatusFrozen, SpawnedAt: nowMinus(t, time.Hour), HistoryPath: histories["sub-d"]},
		{ID: "sub-c", AgentType: "coder", Goal: "goal c", Status: session.SubagentStatusFrozen, SpawnedAt: nowMinus(t, time.Hour), HistoryPath: histories["sub-c"]},
	} {
		if err := sess.SaveSubagentRecord(rec); err != nil {
			t.Fatalf("SaveSubagentRecord(%s): %v", rec.ID, err)
		}
		if err := session.SaveHistory(histories[rec.ID], []client.ChatMessage{
			{Role: "user", Content: client.TextContent("Goal: " + rec.Goal)},
			{Role: "assistant", Content: client.TextContent("halfway through " + rec.Goal)},
		}); err != nil {
			t.Fatalf("SaveHistory(%s): %v", rec.ID, err)
		}
	}
	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	// The crash snapshot: one parallel batch, nothing serial.
	if err := session.SaveSchedulerState(sessionID, session.SchedulerState{
		ParallelQueue: []string{"sub-c", "sub-d"},
	}); err != nil {
		t.Fatalf("SaveSchedulerState: %v", err)
	}

	clients := map[string]*client.Client{
		"coder":      newHangingChildServer(t),
		"researcher": newHangingChildServer(t),
	}
	root := orchestrator.NewBaseOrchestrator(common.MainAgentID, sess, nil, 0)
	sched := newSubagentScheduler(sess, nil)
	sched.sessionID = sessionID
	deps := liveRestoreTestDeps(root, sess, sessionID, sched, clients["coder"])
	deps.clientFor = func(agentType string) *client.Client { return clients[agentType] }

	restored := make(map[string]bool)
	restoreInterruptedSubagents(root, manifest, restored, deps)
	if !restored["sub-c"] || !restored["sub-d"] {
		t.Fatalf("batch members not restored: %v", restored)
	}

	// Both children LIVE at the same time — the batch-wide concurrency.
	eventually(t, 10*time.Second, "parallel batch c+d live concurrently", func() bool {
		return strings.Contains(sched.State("sub-c"), "running (parallel)") &&
			strings.Contains(sched.State("sub-d"), "running (parallel)")
	})
}

// TestRestoreInterruptedSubagentsCorruptSnapshotDegrades pins the corrupt
// snapshot rule: an unreadable scheduler_state.json is logged and treated as
// empty — the batch restores untouched (manifest order, historical parallel
// relaunch) and the corrupt file is left on disk (never deleted unread).
func TestRestoreInterruptedSubagentsCorruptSnapshotDegrades(t *testing.T) {
	const sessionID = "session-restore-snapshot-corrupt-1"
	sess := runnerTestSession(t, sessionID)

	history := childHistoryPath(t, sess, sessionID, "sub-a")
	// Save the manifest record first: its write creates the subagents dir
	// the history file needs.
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: "sub-a", AgentType: "coder", Goal: "goal a",
		Status: session.SubagentStatusFrozen, SpawnedAt: nowMinus(t, time.Hour),
		HistoryPath: history,
	}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}
	if err := session.SaveHistory(history, []client.ChatMessage{
		{Role: "user", Content: client.TextContent("Goal: goal a")},
	}); err != nil {
		t.Fatalf("SaveHistory: %v", err)
	}
	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatal(err)
	}

	// A corrupt snapshot at the exact scheduler state path.
	statePath, err := session.SchedulerStatePath(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := orchestrator.NewBaseOrchestrator(common.MainAgentID, sess, nil, 0)
	// Live machinery present but with no default client: the consume path
	// still runs (deps.sessionID is set), the live relaunch degrades to the
	// read-only fallback, and the corrupt snapshot must survive untouched.
	deps := liveRestoreTestDeps(root, sess, sessionID, newSubagentScheduler(sess, nil), nil)

	restored := make(map[string]bool)
	restoreInterruptedSubagents(root, manifest, restored, deps)
	if !restored["sub-a"] {
		t.Fatal("child not restored despite the corrupt snapshot")
	}
	if _, loadErr := session.LoadSchedulerState(sessionID); loadErr == nil {
		t.Error("corrupt snapshot file read back clean — it was overwritten or removed unread")
	}
	// The file is still there to inspect: an unread state is never deleted.
	if _, statErr := os.Stat(statePath); statErr != nil {
		t.Errorf("corrupt snapshot file was removed without being read: %v", statErr)
	}
}
