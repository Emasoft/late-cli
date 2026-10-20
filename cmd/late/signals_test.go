package main

import (
	"path/filepath"
	"testing"
	"time"

	"late/internal/client"
	"late/internal/session"
)

// TestFlushInterruptedSubagentStatuses covers the exit-time flush's core
// contract: a running record the process is shepherding becomes
// SubagentStatusInterrupted, while frozen and terminal records keep their
// status and stamps untouched — the flush refines non-terminal children
// only, through the preserving writer.
func TestFlushInterruptedSubagentStatuses(t *testing.T) {
	tmp := t.TempDir()
	sessionID := "session-signal"
	sess := session.New(nil, filepath.Join(tmp, sessionID+".json"), []client.ChatMessage{}, "prompt", false)

	// Manifest records covering the skip matrix: running (live child,
	// flips), queued (never launched, flips), frozen (must survive),
	// completed (must survive with its stamps intact).
	historyPath, err := session.SubagentHistoryPath(sessionID, "child-live")
	if err != nil {
		t.Fatalf("SubagentHistoryPath: %v", err)
	}
	spawned := time.Now().Add(-time.Minute)
	records := []session.SubagentRecord{
		{
			ID: "child-live", AgentType: "coder", Goal: "live when the signal arrived",
			Status:      session.SubagentStatusRunning,
			SpawnedAt:   spawned,
			HistoryPath: historyPath,
		},
		{
			ID: "child-queued", AgentType: "coder", Goal: "queued but never launched",
			Status:    session.SubagentStatusQueued,
			SpawnedAt: spawned,
		},
		{
			ID: "child-frozen", AgentType: "coder", Goal: "frozen earlier",
			Status:    session.SubagentStatusFrozen,
			SpawnedAt: spawned,
		},
		{
			ID: "child-done", AgentType: "coder", Goal: "finished earlier",
			Status:        session.SubagentStatusCompleted,
			SpawnedAt:     spawned,
			ResultPath:    "/sessions/child-done.result.txt",
			ResultPreview: "all done",
		},
	}
	for i := range records {
		if err := sess.SaveSubagentRecord(records[i]); err != nil {
			t.Fatalf("SaveSubagentRecord(%s): %v", records[i].ID, err)
		}
	}

	// The scheduler holds the live child and the queued-but-unlaunched
	// entry — exactly the registry the trap reads at signal time. Field
	// assignment (same package) keeps the test free of goroutines.
	scheduler := newSubagentScheduler(nil, nil)
	scheduler.running["child-live"] = "parallel"
	scheduler.liveChildren["child-live"] = nil
	scheduler.serialQueue = append(scheduler.serialQueue, schedulerEntry{id: "child-queued", agentType: "coder"})

	deps := signalFlushDeps{sessionID: sessionID, sess: sess, scheduler: scheduler}
	if err := flushInterruptedSubagentStatuses(deps); err != nil {
		t.Fatalf("flushInterruptedSubagentStatuses: %v", err)
	}

	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}

	// Flipped: the two orphanable children.
	for _, id := range []string{"child-live", "child-queued"} {
		rec, ok := manifest.Get(id)
		if !ok {
			t.Fatalf("record %s missing after flush", id)
		}
		if rec.Status != session.SubagentStatusInterrupted {
			t.Errorf("%s: status = %q, want %q", id, rec.Status, session.SubagentStatusInterrupted)
		}
	}

	// Survived: frozen keeps its status, completed keeps its terminal
	// status AND its stamps (the preserve rule must not clobber them).
	if rec, _ := manifest.Get("child-frozen"); rec.Status != session.SubagentStatusFrozen {
		t.Errorf("child-frozen: status = %q, want frozen (untouched)", rec.Status)
	}
	rec, _ := manifest.Get("child-done")
	if rec.Status != session.SubagentStatusCompleted {
		t.Errorf("child-done: status = %q, want completed (untouched)", rec.Status)
	}
	if rec.ResultPath != records[3].ResultPath || rec.ResultPreview != records[3].ResultPreview {
		t.Errorf("child-done: stamps regressed: resultPath=%q resultPreview=%q", rec.ResultPath, rec.ResultPreview)
	}
}

// TestSchedulerInterruptibleChildren pins the registry union the flush
// enumerates: live running entries plus every queued entry whose launch
// never happened, deduplicated and nil-safe.
func TestSchedulerInterruptibleChildren(t *testing.T) {
	scheduler := newSubagentScheduler(nil, nil)
	if got := scheduler.interruptibleChildren(); len(got) != 0 {
		t.Errorf("empty scheduler: interruptibleChildren = %v, want empty", got)
	}

	scheduler.running["a"] = "serial"
	scheduler.liveChildren["a"] = nil
	scheduler.liveChildren["b"] = nil
	scheduler.serialQueue = append(scheduler.serialQueue, schedulerEntry{id: "c"})
	scheduler.parallelQueue = append(scheduler.parallelQueue, schedulerEntry{id: "b"}, schedulerEntry{id: "d"})

	// Nil-safe for the subagents-disabled wiring shape.
	var nilScheduler *SubagentScheduler
	if got := nilScheduler.interruptibleChildren(); len(got) != 0 {
		t.Errorf("nil scheduler: interruptibleChildren = %v", got)
	}

	got := scheduler.interruptibleChildren()
	want := map[string]bool{"a": true, "b": true, "c": true, "d": true}
	if len(got) != len(want) {
		t.Fatalf("interruptibleChildren = %v, want the union %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("interruptibleChildren: unexpected id %q", id)
		}
	}
}
