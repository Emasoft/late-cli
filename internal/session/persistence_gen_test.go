package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"late/internal/client"
	"late/internal/common"
)

// --- stale-writer generation guard -------------------------------------------
//
// persistHistorySnapshot generation-checks every write so a slow writer
// (marshal + disk I/O run outside historyMu) can never overwrite newer bytes
// with older ones: a snapshot copied before a commit must not revert the
// committed message off disk, and a popped turn must not be resurrected by a
// pre-pop snapshot landing late.

// TestStaleSnapshotWriteIsSkipped pins the commit-vs-snapshot ordering: a
// snapshot captured before a commit, finishing its write after the commit's
// save, must not revert the file to the pre-commit bytes.
func TestStaleSnapshotWriteIsSkipped(t *testing.T) {
	s := snapshotTestSession(t, "session-gen-1")
	if err := s.AddUserMessage("m1"); err != nil {
		t.Fatalf("AddUserMessage: %v", err)
	}

	// The ticker's copy step: state and generation captured under historyMu.
	s.historyMu.Lock()
	stale := make([]client.ChatMessage, len(s.History))
	copy(stale, s.History)
	staleGen := s.historyGen.Load()
	s.historyMu.Unlock()

	// The commit lands while the snapshot is still marshaling.
	if err := s.AddUserMessage("m2"); err != nil {
		t.Fatalf("AddUserMessage: %v", err)
	}
	loaded, err := LoadHistory(s.HistoryPath)
	if err != nil || len(loaded) != 2 {
		t.Fatalf("after commit, disk holds %d messages (err=%v), want 2", len(loaded), err)
	}

	// The stale snapshot's write finally completes — it must be skipped.
	if err := s.persistHistorySnapshot(stale, staleGen); err != nil {
		t.Fatalf("stale persistHistorySnapshot: %v", err)
	}
	loaded, err = LoadHistory(s.HistoryPath)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(loaded) != 2 || loaded[1].Content.String() != "m2" {
		t.Fatalf("stale snapshot reverted the disk: %d messages, last=%q; want 2 messages ending m2",
			len(loaded), loaded[len(loaded)-1].Content.String())
	}
}

// TestFreshSnapshotWriteStillPersists pins the positive side of the guard: a
// writer whose generation is current writes (idempotently) — the check skips
// stale writers only.
func TestFreshSnapshotWriteStillPersists(t *testing.T) {
	s := snapshotTestSession(t, "session-gen-2")
	if err := s.AddUserMessage("m1"); err != nil {
		t.Fatalf("AddUserMessage: %v", err)
	}

	s.historyMu.Lock()
	snapshot := make([]client.ChatMessage, len(s.History))
	copy(snapshot, s.History)
	gen := s.historyGen.Load()
	s.historyMu.Unlock()

	if err := s.persistHistorySnapshot(snapshot, gen); err != nil {
		t.Fatalf("fresh persistHistorySnapshot: %v", err)
	}
	loaded, err := LoadHistory(s.HistoryPath)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Content.String() != "m1" {
		t.Fatalf("fresh write missing: %d messages (%+v), want [m1]", len(loaded), loaded)
	}
}

// TestPopResistsStaleSnapshot pins the pop-vs-snapshot ordering: a snapshot
// copied before PopLastUserMessage must not resurrect the popped turn on
// disk after the pop's own save removed it.
func TestPopResistsStaleSnapshot(t *testing.T) {
	tmp := t.TempDir()
	stubSessionDir(t, tmp)
	historyPath := filepath.Join(tmp, "session-gen-pop.json")
	s := New(nil, historyPath, nil, "sp", true)
	if err := s.AddUserMessage("first"); err != nil {
		t.Fatalf("AddUserMessage: %v", err)
	}

	s.historyMu.Lock()
	stale := make([]client.ChatMessage, len(s.History))
	copy(stale, s.History)
	staleGen := s.historyGen.Load()
	s.historyMu.Unlock()

	popped, err := s.PopLastUserMessage()
	if err != nil || !popped {
		t.Fatalf("PopLastUserMessage = %v, %v; want true, nil", popped, err)
	}
	if _, err := os.Stat(historyPath); !os.IsNotExist(err) {
		t.Fatalf("history file still exists after pop, want removed (stat err=%v)", err)
	}

	// The pre-pop snapshot lands late: it must not resurrect the message.
	if err := s.persistHistorySnapshot(stale, staleGen); err != nil {
		t.Fatalf("stale persistHistorySnapshot: %v", err)
	}
	if _, err := os.Stat(historyPath); !os.IsNotExist(err) {
		t.Fatalf("stale snapshot resurrected the history file (stat err=%v)", err)
	}
}

// TestPersistHistoryOutOfPackage pins the locked persistence entry point the
// rewind and compaction-runner call sites use instead of reading sess.History
// directly.
func TestPersistHistoryOutOfPackage(t *testing.T) {
	s := snapshotTestSession(t, "session-gen-persist")
	if err := s.AddUserMessage("only"); err != nil {
		t.Fatalf("AddUserMessage: %v", err)
	}
	if err := s.PersistHistory(); err != nil {
		t.Fatalf("PersistHistory: %v", err)
	}
	loaded, err := LoadHistory(s.HistoryPath)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Content.String() != "only" {
		t.Fatalf("persisted history = %+v, want [only]", loaded)
	}
}

// TestAppendToLastMessageDoesNotRaceSnapshotMarshal is a -race test for the
// in-place Parts mutation: AppendToLastMessage used to write
// History[last].Content.Parts[i] in place, sharing the backing array with
// any snapshot copy taken under historyMu — marshaling that copy outside the
// lock raced the append. The parts slice must be cloned before mutating.
func TestAppendToLastMessageDoesNotRaceSnapshotMarshal(t *testing.T) {
	s := snapshotTestSession(t, "session-gen-parts")
	if err := s.AddUserMessage("question"); err != nil {
		t.Fatalf("AddUserMessage: %v", err)
	}
	// A multimodal assistant message as the tail.
	s.historyMu.Lock()
	s.History = append(s.History, client.ChatMessage{
		Role: "assistant",
		Content: client.MessageContent{Parts: []client.ContentPart{
			{Type: client.ContentPartText, Text: "start"},
		}},
	})
	// The ticker's copy: struct copy shares the Parts backing array.
	snapshot := make([]client.ChatMessage, len(s.History))
	copy(snapshot, s.History)
	s.historyGen.Add(1)
	s.historyMu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // the continuation path mutating the tail message
		defer wg.Done()
		for i := 0; i < 25; i++ {
			if err := s.AppendToLastMessage("+more", ""); err != nil {
				t.Errorf("AppendToLastMessage: %v", err)
				return
			}
		}
	}()
	go func() { // the snapshot's marshal, outside the lock
		defer wg.Done()
		for i := 0; i < 25; i++ {
			if _, err := json.Marshal(snapshot); err != nil {
				t.Errorf("marshal snapshot: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	got := s.History[len(s.History)-1].Content.String()
	if !strings.Contains(got, "start") || !strings.Contains(got, "+more") {
		t.Fatalf("tail message = %q, want the original text plus appends", got)
	}
}

// --- LoadHistory corruption handling -----------------------------------------

// TestLoadHistoryEmptyFile pins the zero-length edge: an empty file is an
// empty history, not a hard unmarshal failure that bricks --continue.
func TestLoadHistoryEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	history, err := LoadHistory(path)
	if err != nil {
		t.Fatalf("LoadHistory(empty file) error = %v, want nil", err)
	}
	if len(history) != 0 {
		t.Fatalf("LoadHistory(empty file) = %+v, want empty", history)
	}
}

// TestLoadHistoryRecoveringBacksUpCorruptFile pins the recovery loader: a
// corrupt history is preserved byte-for-byte next to the original before the
// error is reported, so the resume path's empty-start cannot destroy it.
func TestLoadHistoryRecoveringBacksUpCorruptFile(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "late-errors.log")
	l, err := common.OpenErrorLogAt(logPath)
	if err != nil {
		t.Fatal(err)
	}
	common.SetErrorLog(l)
	t.Cleanup(func() { common.SetErrorLog(nil) })

	path := filepath.Join(t.TempDir(), "corrupt.json")
	raw := []byte("[{\"role\":\"user\",\"cont") // torn document
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	history, err := LoadHistoryRecovering(path)
	if err == nil {
		t.Fatalf("LoadHistoryRecovering(corrupt) error = nil, want an error")
	}
	if len(history) != 0 {
		t.Fatalf("LoadHistoryRecovering(corrupt) = %+v, want empty history on error", history)
	}

	matches, err := filepath.Glob(path + ".corrupt-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("backups of the corrupt file = %v (err=%v), want exactly one", matches, err)
	}
	backed, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(backed) != string(raw) {
		t.Fatalf("backup bytes = %q, want the original %q", backed, raw)
	}

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("critical-error log unreadable: %v", err)
	}
	if !strings.Contains(string(logged), "corrupt.json") {
		t.Fatalf("critical-error log missing the history failure:\n%s", logged)
	}
}

// TestLoadHistoryRecoveringCleanLoad makes sure the recovery loader is a
// no-op wrapper on the happy path: no backup files, no error.
func TestLoadHistoryRecoveringCleanLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clean.json")
	if err := SaveHistory(path, []client.ChatMessage{{Role: "user", Content: client.TextContent("hi")}}); err != nil {
		t.Fatal(err)
	}
	history, err := LoadHistoryRecovering(path)
	if err != nil {
		t.Fatalf("LoadHistoryRecovering error = %v, want nil", err)
	}
	if len(history) != 1 || history[0].Content.String() != "hi" {
		t.Fatalf("loaded = %+v, want [hi]", history)
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 0 {
		t.Fatalf("clean load produced backups: %v", matches)
	}
}
