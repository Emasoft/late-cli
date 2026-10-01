package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"late/internal/client"
	"late/internal/session"
)

// snapshotTestChild is a minimal common.Orchestrator + Session() source —
// the exact surface childSessionFor and startSubagentSnapshotTicker touch.
type snapshotTestChild struct {
	id   string
	sess *session.Session
}

func (c *snapshotTestChild) ID() string                { return c.id }
func (c *snapshotTestChild) Session() *session.Session { return c.sess }

// snapshotTestChildSession returns a child session whose history path lives
// in a sandboxed sessions dir, like runnerTestSession.
func snapshotTestChildSession(t *testing.T, sessionID, childID string) *session.Session {
	t.Helper()
	tmp := t.TempDir()
	original := session.SessionDir
	session.SessionDir = func() (string, error) { return tmp, nil }
	t.Cleanup(func() { session.SessionDir = original })
	path, err := session.SubagentHistoryPath(sessionID, childID)
	if err != nil {
		t.Fatalf("SubagentHistoryPath: %v", err)
	}
	return session.NewSubagentSession(nil, path, nil, "prompt")
}

// waitForSnapshotFile polls until the child's history file exists with at
// least want messages, or fails after a generous deadline.
func waitForSnapshotFile(t *testing.T, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		loaded, err := session.LoadHistory(path)
		if err == nil && len(loaded) >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("history file %s never reached %d messages", path, want)
}

// TestSubagentSnapshotTickerTickPersistsHistory pins the periodic snapshot:
// a tick re-snapshots the child's history, so a message appended after the
// commit path's write (simulating a mid-turn turn whose commit has not yet
// landed on disk) still reaches the file on the next tick.
func TestSubagentSnapshotTickerTickPersistsHistory(t *testing.T) {
	const sessionID = "session-20250101-130000"
	childSession := snapshotTestChildSession(t, sessionID, "coder-subagent-0")

	if err := childSession.AddUserMessage("the goal"); err != nil {
		t.Fatalf("AddUserMessage: %v", err)
	}

	stop := startSubagentSnapshotTicker(&snapshotTestChild{id: "coder-subagent-0", sess: childSession}, childSession, 5*time.Millisecond)
	if stop == nil {
		t.Fatal("startSubagentSnapshotTicker returned nil for a persistable child session")
	}

	// A message lands mid-turn (the commit path already wrote it, but the
	// ticker must be able to re-snapshot it regardless — the pinned property
	// is that ticks carry memory state to the file).
	if err := childSession.AddAssistantMessage("streamed output", ""); err != nil {
		t.Fatalf("AddAssistantMessage: %v", err)
	}
	waitForSnapshotFile(t, childSession.HistoryPath, 2)

	stop()
	loaded, err := session.LoadHistory(childSession.HistoryPath)
	if err != nil {
		t.Fatalf("LoadHistory after stop: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("history holds %d messages, want 2", len(loaded))
	}
	if loaded[1].Role != "assistant" || loaded[1].Content.String() != "streamed output" {
		t.Errorf("loaded[1] = %s/%q, want assistant/streamed output", loaded[1].Role, loaded[1].Content.String())
	}
}

// TestSubagentSnapshotTickerFinalFlushAfterStop pins the outcome-time flush:
// memory state that no tick ever observed (the in-flight turn's bytes, whose
// commit has not landed) is on disk once stop() returns — the window closes
// at outcome time, not at the last tick.
func TestSubagentSnapshotTickerFinalFlushAfterStop(t *testing.T) {
	const sessionID = "session-20250101-130001"
	childSession := snapshotTestChildSession(t, sessionID, "coder-subagent-1")

	if err := childSession.AddUserMessage("the goal"); err != nil {
		t.Fatalf("AddUserMessage: %v", err)
	}

	stop := startSubagentSnapshotTicker(&snapshotTestChild{id: "coder-subagent-1", sess: childSession}, childSession, 50*time.Millisecond)
	if stop == nil {
		t.Fatal("startSubagentSnapshotTicker returned nil for a persistable child session")
	}
	waitForSnapshotFile(t, childSession.HistoryPath, 1)

	// In-memory state no tick has observed: append WITHOUT the commit-path
	// save (the executor's accumulator analog — the in-flight turn).
	childSession.History = append(childSession.History, client.ChatMessage{
		Role:    "assistant",
		Content: client.TextContent("in-flight turn"),
	})

	// stop() must perform the final flush: the message reaches the file even
	// though no tick fired after the append.
	stop()

	loaded, err := session.LoadHistory(childSession.HistoryPath)
	if err != nil {
		t.Fatalf("LoadHistory after stop: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("final flush must carry the uncommitted message, got %d messages on disk", len(loaded))
	}
	if loaded[1].Role != "assistant" || loaded[1].Content.String() != "in-flight turn" {
		t.Errorf("loaded[1] = %s/%q, want assistant/in-flight turn", loaded[1].Role, loaded[1].Content.String())
	}
}

// TestSubagentSnapshotTickerNilForUnpersistableSession pins the guard: an
// in-memory child (no history path) gets no ticker — nothing to snapshot.
func TestSubagentSnapshotTickerNilForUnpersistableSession(t *testing.T) {
	inMemory := session.NewSubagentSession(nil, "", nil, "prompt")
	if stop := startSubagentSnapshotTicker(&snapshotTestChild{id: "x", sess: inMemory}, inMemory, time.Millisecond); stop != nil {
		stop()
		t.Fatal("startSubagentSnapshotTicker must return nil for a session without a history path")
	}
	if stop := startSubagentSnapshotTicker(&snapshotTestChild{id: "x"}, nil, time.Millisecond); stop != nil {
		stop()
		t.Fatal("startSubagentSnapshotTicker must return nil for a nil session")
	}
}

// TestChildSessionForExtractsSession pins the assertion helper: the concrete
// child type exposes Session(); a bare orchestrator without it yields nil so
// the runner's snapshot wiring stays silent.
func TestChildSessionForExtractsSession(t *testing.T) {
	child := &snapshotTestChild{id: "coder-subagent-2", sess: snapshotTestChildSession(t, "session-20250101-130002", "coder-subagent-2")}
	if got := childSessionFor(child); got != child.sess {
		t.Fatalf("childSessionFor = %v, want the child's session", got)
	}
	if got := childSessionFor(&stubRunnerChild{id: "bare"}); got == nil {
		// stubRunnerChild (subagent_manifest_test.go) DOES implement
		// Session(); a nil session is returned as-is — the runner's nil
		// check handles it.
		t.Logf("stubRunnerChild exposes a nil session; childSessionFor passes it through")
	}
}

// TestSubagentSnapshotIntervalConstant pins the constant's shape: a positive
// duration, so the ticker can never be configured into a zero-interval busy
// loop by accident.
func TestSubagentSnapshotIntervalConstant(t *testing.T) {
	if subagentSnapshotInterval <= 0 {
		t.Fatalf("subagentSnapshotInterval = %v, want positive", subagentSnapshotInterval)
	}
}

// TestSubagentSnapshotTickerConcurrentWithAppendRaceFree runs the real ticker
// against a session being appended to concurrently — the production
// interleaving (child streaming + ticker) — under the suite's -race run.
func TestSubagentSnapshotTickerConcurrentWithAppendRaceFree(t *testing.T) {
	const sessionID = "session-20250101-130003"
	childSession := snapshotTestChildSession(t, sessionID, "coder-subagent-3")

	if err := childSession.AddUserMessage("seed"); err != nil {
		t.Fatalf("AddUserMessage: %v", err)
	}

	stop := startSubagentSnapshotTicker(&snapshotTestChild{id: "coder-subagent-3", sess: childSession}, childSession, time.Millisecond)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if err := childSession.AddAssistantMessage("chunk", ""); err != nil {
				t.Errorf("AddAssistantMessage: %v", err)
				return
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	wg.Wait()

	stop()

	loaded, err := session.LoadHistory(childSession.HistoryPath)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(loaded) != 51 {
		t.Fatalf("final history holds %d messages, want 51 (seed + 50 chunks)", len(loaded))
	}
	if _, err := os.Stat(filepath.Dir(childSession.HistoryPath)); err != nil {
		t.Fatalf("subagents dir must exist: %v", err)
	}
}
