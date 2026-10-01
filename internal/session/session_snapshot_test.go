package session

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"late/internal/client"
)

// snapshotTestSession returns a session whose history path lives in the temp
// sessions dir, so SnapshotHistory writes stay out of the real user
// directory. SessionDir is stubbed because SnapshotHistory → SaveHistory →
// writeAtomic only touches the path itself, but UpdateSessionMetadata (the
// saveAndNotify commit path the tests also exercise) resolves the sidecar
// through it.
func snapshotTestSession(t *testing.T, sessionID string) *Session {
	t.Helper()
	tmp := t.TempDir()
	stubSessionDir(t, tmp)
	s := New(nil, filepath.Join(tmp, sessionID+".json"), nil, "prompt", false)
	return s
}

// TestSnapshotHistoryWritesHistory pins the primitive the mid-turn snapshot
// ticker calls: after appends (which persist via the commit path), the
// snapshot must rewrite the exact same history bytes — idempotent with the
// commit-path write.
func TestSnapshotHistoryWritesHistory(t *testing.T) {
	s := snapshotTestSession(t, "session-snapshot-1")

	if err := s.AddUserMessage("the goal"); err != nil {
		t.Fatalf("AddUserMessage: %v", err)
	}
	if err := s.AddAssistantMessage("working on it", ""); err != nil {
		t.Fatalf("AddAssistantMessage: %v", err)
	}

	if err := s.SnapshotHistory(); err != nil {
		t.Fatalf("SnapshotHistory: %v", err)
	}

	loaded, err := LoadHistory(s.HistoryPath)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("snapshot on disk holds %d messages, want 2", len(loaded))
	}
	if loaded[0].Role != "user" || loaded[0].Content.String() != "the goal" {
		t.Errorf("loaded[0] = %s/%q, want user/the goal", loaded[0].Role, loaded[0].Content.String())
	}
	if loaded[1].Role != "assistant" || loaded[1].Content.String() != "working on it" {
		t.Errorf("loaded[1] = %s/%q, want assistant/working on it", loaded[1].Role, loaded[1].Content.String())
	}
}

// TestSnapshotHistoryNoopWithoutPathAndHistory pins the two no-op guards:
// an in-memory session (no history path) and an empty history (no file must
// be materialized for a child that never produced anything).
func TestSnapshotHistoryNoopWithoutPathAndHistory(t *testing.T) {
	inMemory := New(nil, "", nil, "prompt", false)
	if err := inMemory.SnapshotHistory(); err != nil {
		t.Fatalf("in-memory SnapshotHistory = %v, want nil", err)
	}

	tmp := t.TempDir()
	empty := New(nil, filepath.Join(tmp, "session-snapshot-2.json"), nil, "prompt", false)
	if err := empty.SnapshotHistory(); err != nil {
		t.Fatalf("empty-history SnapshotHistory = %v, want nil", err)
	}
	if _, err := os.Stat(empty.HistoryPath); !os.IsNotExist(err) {
		t.Errorf("empty history must not materialize a file, stat err = %v", err)
	}
}

// TestSnapshotHistoryConcurrentAppendRaceFree runs SnapshotHistory
// concurrently with appendMessage-driven Add* calls — the exact interleaving
// the mid-turn ticker creates while a child streams. Run under -race: the
// point is that the snapshot's copy takes historyMu and appends hold it for
// the slice write, so no racy read of History is reported.
func TestSnapshotHistoryConcurrentAppendRaceFree(t *testing.T) {
	s := snapshotTestSession(t, "session-snapshot-3")

	if err := s.AddUserMessage("seed"); err != nil {
		t.Fatalf("AddUserMessage: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	// The appender stands in for the streaming run loop's commit path.
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := s.AddAssistantMessage("chunk", ""); err != nil {
				t.Errorf("AddAssistantMessage: %v", err)
				return
			}
		}
	}()

	// The snapshotter stands in for the mid-turn ticker.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
				if err := s.SnapshotHistory(); err != nil {
					t.Errorf("SnapshotHistory: %v", err)
					return
				}
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Every snapshot wrote a coherent prefix of the history; the final file
	// must hold at least the seed plus everything the race window appended.
	loaded, err := LoadHistory(s.HistoryPath)
	if err != nil {
		t.Fatalf("final LoadHistory: %v", err)
	}
	if len(loaded) < 2 {
		t.Fatalf("final history holds %d messages, want at least the seed + one chunk", len(loaded))
	}
	if loaded[0].Role != "user" || loaded[0].Content.String() != "seed" {
		t.Errorf("loaded[0] = %s/%q, want user/seed", loaded[0].Role, loaded[0].Content.String())
	}
}

// TestSnapshotHistorySeesMidTurnAppends pins the loss-window property 3a
// buys: a message appended to memory (as the executor's commit path does)
// is visible to the very next snapshot even when the file was last written
// before the append.
func TestSnapshotHistorySeesMidTurnAppends(t *testing.T) {
	s := snapshotTestSession(t, "session-snapshot-4")

	if err := s.AddUserMessage("first"); err != nil {
		t.Fatalf("AddUserMessage: %v", err)
	}
	if err := s.SnapshotHistory(); err != nil {
		t.Fatalf("first SnapshotHistory: %v", err)
	}

	// A mid-turn append lands in memory and — via the commit path — on disk;
	// the next snapshot must carry it either way.
	if err := s.AddToolResultMessage("call_1", "tool output"); err != nil {
		t.Fatalf("AddToolResultMessage: %v", err)
	}
	if err := s.SnapshotHistory(); err != nil {
		t.Fatalf("second SnapshotHistory: %v", err)
	}

	loaded, err := LoadHistory(s.HistoryPath)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("history on disk holds %d messages, want 2", len(loaded))
	}
	if loaded[1].Role != "tool" || loaded[1].ToolCallID != "call_1" {
		t.Errorf("loaded[1] = %s (call %q), want tool result for call_1", loaded[1].Role, loaded[1].ToolCallID)
	}
}

// TestTruncateHistoryDropsTail pins the rollback primitive the rewind path
// and the unsupported-image rollback use: the tail below index is gone,
// out-of-range indexes are a no-op, and the change persists on the next
// save.
func TestTruncateHistoryDropsTail(t *testing.T) {
	s := snapshotTestSession(t, "session-truncate-1")

	for _, role := range []string{"user", "assistant", "user", "assistant"} {
		if err := s.AddMessage(client.ChatMessage{Role: role, Content: client.TextContent(role)}); err != nil {
			t.Fatalf("AddMessage(%s): %v", role, err)
		}
	}

	s.TruncateHistory(2)
	if len(s.History) != 2 {
		t.Fatalf("after TruncateHistory(2) len = %d, want 2", len(s.History))
	}
	if s.History[1].Role != "assistant" {
		t.Errorf("History[1] = %s, want assistant (the kept tail)", s.History[1].Role)
	}

	// Out-of-range and negative indexes never panic and never change state.
	s.TruncateHistory(-1)
	s.TruncateHistory(99)
	if len(s.History) != 2 {
		t.Fatalf("out-of-range TruncateHistory changed len to %d, want 2", len(s.History))
	}

	// The truncation reaches disk on the next snapshot.
	if err := s.SnapshotHistory(); err != nil {
		t.Fatalf("SnapshotHistory: %v", err)
	}
	loaded, err := LoadHistory(s.HistoryPath)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("history on disk holds %d messages, want 2", len(loaded))
	}
}
