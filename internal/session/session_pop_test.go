package session

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestPopLastUserMessage covers the rollback primitive used by the
// orchestrator when the API terminally rejects a turn with HTTP 400: the
// trailing user message must be removed from history and the removal must be
// persisted, while non-user or empty tails must be left untouched.
func TestPopLastUserMessage(t *testing.T) {
	t.Run("pops trailing user message and persists the removal", func(t *testing.T) {
		tmpDir := t.TempDir()
		oldSessionDir := SessionDir
		SessionDir = func() (string, error) { return tmpDir, nil }
		t.Cleanup(func() { SessionDir = oldSessionDir })

		historyPath := filepath.Join(tmpDir, "session-pop.json")
		s := New(nil, historyPath, nil, "sp", true)
		if err := s.AddUserMessage("first"); err != nil {
			t.Fatalf("AddUserMessage returned error: %v", err)
		}
		if err := s.AddAssistantMessage("reply", ""); err != nil {
			t.Fatalf("AddAssistantMessage returned error: %v", err)
		}
		if err := s.AddUserMessage("second"); err != nil {
			t.Fatalf("AddUserMessage returned error: %v", err)
		}

		if err := s.PopLastUserMessage(); err != nil {
			t.Fatalf("PopLastUserMessage returned error: %v", err)
		}

		// In-memory rollback: the assistant reply is now the tail.
		if len(s.History) != 2 {
			t.Fatalf("len(s.History) = %d, want 2", len(s.History))
		}
		if got := s.History[len(s.History)-1].Content.String(); got != "reply" {
			t.Errorf("last in-memory message = %q, want %q", got, "reply")
		}

		// Persisted rollback: the file on disk must match the trimmed history.
		loaded, err := LoadHistory(historyPath)
		if err != nil {
			t.Fatalf("LoadHistory returned error: %v", err)
		}
		if len(loaded) != 2 {
			t.Fatalf("persisted history has %d messages, want 2", len(loaded))
		}
		if got := loaded[len(loaded)-1].Content.String(); got != "reply" {
			t.Errorf("last persisted message = %q, want %q", got, "reply")
		}
	})

	t.Run("no-op when history ends with assistant", func(t *testing.T) {
		tmpDir := t.TempDir()
		oldSessionDir := SessionDir
		SessionDir = func() (string, error) { return tmpDir, nil }
		t.Cleanup(func() { SessionDir = oldSessionDir })

		historyPath := filepath.Join(tmpDir, "session-pop-assistant.json")
		s := New(nil, historyPath, nil, "sp", true)
		if err := s.AddUserMessage("only"); err != nil {
			t.Fatalf("AddUserMessage returned error: %v", err)
		}
		if err := s.AddAssistantMessage("reply", ""); err != nil {
			t.Fatalf("AddAssistantMessage returned error: %v", err)
		}

		before, err := os.ReadFile(historyPath)
		if err != nil {
			t.Fatalf("reading persisted history before pop: %v", err)
		}

		if err := s.PopLastUserMessage(); err != nil {
			t.Fatalf("PopLastUserMessage returned error: %v", err)
		}

		if len(s.History) != 2 {
			t.Errorf("len(s.History) = %d, want 2 (unchanged)", len(s.History))
		}
		after, err := os.ReadFile(historyPath)
		if err != nil {
			t.Fatalf("reading persisted history after pop: %v", err)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("history file changed on no-op pop:\nbefore: %s\nafter:  %s", before, after)
		}
	})

	t.Run("no-op on empty history", func(t *testing.T) {
		tmpDir := t.TempDir()
		oldSessionDir := SessionDir
		SessionDir = func() (string, error) { return tmpDir, nil }
		t.Cleanup(func() { SessionDir = oldSessionDir })

		historyPath := filepath.Join(tmpDir, "session-pop-empty.json")
		s := New(nil, historyPath, nil, "sp", true)

		if err := s.PopLastUserMessage(); err != nil {
			t.Fatalf("PopLastUserMessage returned error: %v", err)
		}
		if len(s.History) != 0 {
			t.Errorf("len(s.History) = %d, want 0", len(s.History))
		}
		if _, err := os.Stat(historyPath); !os.IsNotExist(err) {
			t.Errorf("expected no history file to be created, stat err=%v", err)
		}
	})
}
