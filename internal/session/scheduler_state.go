package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// schedulerStateFileName is the persisted scheduler queue snapshot inside
// <sessionsDir>/<sessionID>/subagents (next to the manifest and the child
// histories).
const schedulerStateFileName = "scheduler_state.json"

// SchedulerState is the scheduler's crash-time queue snapshot: the ordered
// serial and parallel queue child IDs plus the set of live running IDs. The
// slices keep the scheduler's dequeue order (fifo serial, batch parallel)
// so a resume can reconstruct the launch order; running is unordered
// (in-memory it is a map).
type SchedulerState struct {
	SerialQueue   []string `json:"serial_queue"`
	ParallelQueue []string `json:"parallel_queue"`
	Running       []string `json:"running"`
}

// SaveSchedulerState atomically persists the scheduler queue snapshot via
// the shared temp+rename primitive (writeAtomic). Duplicated IDs are
// dropped while preserving the first-seen order — the snapshot is read by
// resume to REORDER a relaunch batch, so anything a race could double-queue
// is normalized at the write boundary instead of the read one.
func SaveSchedulerState(sessionID string, state SchedulerState) error {
	path, err := SchedulerStatePath(sessionID)
	if err != nil {
		return err
	}
	// Dedupe preserved-order at the write boundary: the snapshot orders the
	// relaunch batch, so a duplicated ID is normalized here once for every
	// reader.
	for dst, src := range map[*[]string][]string{
		&state.SerialQueue:   state.SerialQueue,
		&state.ParallelQueue: state.ParallelQueue,
		&state.Running:       state.Running,
	} {
		seen := make(map[string]bool, len(src))
		out := make([]string, 0, len(src))
		for _, id := range src {
			if id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		*dst = out
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to encode scheduler state: %w", err)
	}
	// Same modes as the sibling sidecars: file 0600, directory 0700.
	return writeAtomic(path, encoded, historyFileMode, 0700)
}

// LoadSchedulerState reads a session's scheduler queue snapshot. A missing
// file is NOT a failure: it returns an empty state and nil error (legacy
// sessions predate queue persistence). A present-but-invalid file returns
// an empty state AND the error — the caller treats that as "no snapshot"
// and must never mutate scheduler state on a load failure.
func LoadSchedulerState(sessionID string) (SchedulerState, error) {
	path, err := SchedulerStatePath(sessionID)
	if err != nil {
		return SchedulerState{}, err
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return SchedulerState{}, nil
		}
		return SchedulerState{}, fmt.Errorf("failed to read scheduler state: %w", readErr)
	}
	var state SchedulerState
	if err := json.Unmarshal(data, &state); err != nil {
		return SchedulerState{}, fmt.Errorf("failed to decode scheduler state: %w", err)
	}
	if state.Running == nil {
		state.Running = []string{}
	}
	return state, nil
}
