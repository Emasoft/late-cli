package session

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestSchedulerStateRoundTrip covers save/load symmetry, the atomic
// overwrite of a second save, and the missing-file sentinel contract.
func TestSchedulerStateRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	stubSessionDir(t, tmp)

	sessionID := "session-sched-state"
	state := SchedulerState{
		SerialQueue:   []string{"a", "b", "a"}, // duplicate dropped on save
		ParallelQueue: []string{"c", "d"},
		Running:       []string{"e"},
	}
	if err := SaveSchedulerState(sessionID, state); err != nil {
		t.Fatalf("SaveSchedulerState: %v", err)
	}

	got, err := LoadSchedulerState(sessionID)
	if err != nil {
		t.Fatalf("LoadSchedulerState: %v", err)
	}
	if !slices.Equal(got.SerialQueue, []string{"a", "b"}) {
		t.Fatalf("serial queue order after dedupe: %v", got.SerialQueue)
	}
	if !slices.Equal(got.ParallelQueue, []string{"c", "d"}) {
		t.Fatalf("parallel queue order: %v", got.ParallelQueue)
	}
	if !slices.Equal(got.Running, []string{"e"}) {
		t.Fatalf("running: %v", got.Running)
	}

	// The snapshot sits exactly where the path helper says it does.
	path, err := SchedulerStatePath(sessionID)
	if err != nil {
		t.Fatalf("SchedulerStatePath: %v", err)
	}
	if want := filepath.Join(tmp, sessionID, subagentsDirName, "scheduler_state.json"); path != want {
		t.Fatalf("path helper mismatch: got %s want %s", path, want)
	}

	// Second save atomically replaces the snapshot.
	if err := SaveSchedulerState(sessionID, SchedulerState{ParallelQueue: []string{"c"}}); err != nil {
		t.Fatalf("SaveSchedulerState (overwrite): %v", err)
	}
	got, err = LoadSchedulerState(sessionID)
	if err != nil {
		t.Fatalf("LoadSchedulerState (overwrite): %v", err)
	}
	if !slices.Equal(got.ParallelQueue, []string{"c"}) || len(got.SerialQueue) != 0 {
		t.Fatalf("overwrite did not replace state: %+v", got)
	}

	// Missing file: empty state, NOT a failure.
	missing, err := LoadSchedulerState("session-no-state")
	if err != nil {
		t.Fatalf("missing snapshot should not error: %v", err)
	}
	if len(missing.SerialQueue) != 0 || len(missing.ParallelQueue) != 0 || len(missing.Running) != 0 {
		t.Fatalf("missing snapshot should be empty state: %+v", missing)
	}
}

// TestLoadSchedulerStateInvalidPayload ensures a corrupt snapshot is a
// reported failure with an empty state — the sentinel the Step 4 restore
// pass reconciles on.
func TestLoadSchedulerStateInvalidPayload(t *testing.T) {
	tmp := t.TempDir()
	stubSessionDir(t, tmp)

	sessionID := "session-sched-bad"
	if err := SaveSchedulerState(sessionID, SchedulerState{Running: []string{"x"}}); err != nil {
		t.Fatalf("SaveSchedulerState: %v", err)
	}
	path, err := SchedulerStatePath(sessionID)
	if err != nil {
		t.Fatalf("SchedulerStatePath: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}

	got, err := LoadSchedulerState(sessionID)
	if err == nil {
		t.Fatal("corrupt snapshot should error")
	}
	if len(got.Running) != 0 {
		t.Fatalf("corrupt snapshot must return empty state, got %+v", got)
	}
}
