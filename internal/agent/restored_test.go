package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"late/internal/client"
	"late/internal/common"
	"late/internal/session"
)

// restoredTestHistory writes a child history file into a sandboxed sessions
// dir and returns its path.
func restoredTestHistory(t *testing.T, sessionID, childID string, msgs ...client.ChatMessage) string {
	t.Helper()
	tmp := t.TempDir()
	original := session.SetSessionDirOverrideForTest(func() (string, error) { return tmp, nil })
	t.Cleanup(func() { session.SetSessionDirOverrideForTest(original) })
	path, err := session.SubagentHistoryPath(sessionID, childID)
	if err != nil {
		t.Fatalf("SubagentHistoryPath: %v", err)
	}
	if err := session.SaveHistory(path, msgs); err != nil {
		t.Fatalf("SaveHistory: %v", err)
	}
	return path
}

// TestNewRestoredSubagentOrchestratorLoadsHistory pins the core contract: the
// restored stub loads the child's persisted history and satisfies
// common.Orchestrator.
func TestNewRestoredSubagentOrchestratorLoadsHistory(t *testing.T) {
	path := restoredTestHistory(t, "session-restore-1", "coder-subagent-0",
		client.ChatMessage{Role: "user", Content: client.TextContent("Goal: fix it")},
		client.ChatMessage{Role: "assistant", Content: client.TextContent("half done")},
	)

	o, err := NewRestoredSubagentOrchestrator("coder-subagent-0", "coder", "fix it", path, "")
	if err != nil {
		t.Fatalf("NewRestoredSubagentOrchestrator: %v", err)
	}

	var _ common.Orchestrator = o // interface satisfaction (compile-time)

	if got := o.ID(); got != "coder-subagent-0" {
		t.Errorf("ID() = %q, want coder-subagent-0", got)
	}
	if got := o.AgentType(); got != "coder" {
		t.Errorf("AgentType() = %q, want coder", got)
	}
	if got := o.Goal(); got != "fix it" {
		t.Errorf("Goal() = %q, want fix it", got)
	}
	history := o.History()
	if len(history) != 2 {
		t.Fatalf("History() holds %d messages, want 2", len(history))
	}
	if history[0].Content.String() != "Goal: fix it" || history[1].Content.String() != "half done" {
		t.Errorf("History() content mismatch: %q / %q", history[0].Content.String(), history[1].Content.String())
	}
	if got := o.StatusText(); got != RestoredSubagentStatus {
		t.Errorf("StatusText() = %q, want the default restored wording", got)
	}
}

// TestRestoredSubagentIsReadOnly pins the read-only surface: Execute, Submit,
// Reset, and Rewind all refuse; Cancel and the queue are silent no-ops; the
// registry is empty so no tool can ever run.
func TestRestoredSubagentIsReadOnly(t *testing.T) {
	path := restoredTestHistory(t, "session-restore-2", "coder-subagent-0",
		client.ChatMessage{Role: "user", Content: client.TextContent("Goal: g")},
	)
	o, err := NewRestoredSubagentOrchestrator("coder-subagent-0", "coder", "g", path, "")
	if err != nil {
		t.Fatalf("NewRestoredSubagentOrchestrator: %v", err)
	}

	if _, err := o.Execute(""); err == nil {
		t.Error("Execute() must fail on a restored subagent")
	}
	if err := o.Submit("hi", nil); err == nil {
		t.Error("Submit() must fail on a restored subagent")
	}
	if err := o.Reset(); err == nil {
		t.Error("Reset() must fail on a restored subagent")
	}
	if err := o.Rewind(0); err == nil {
		t.Error("Rewind() must fail on a restored subagent")
	}

	o.Cancel() // no-op, must not panic
	if o.IsStopRequested() {
		t.Error("IsStopRequested() = true, want false")
	}
	if o.Registry() != nil {
		t.Error("Registry() must be nil: no tool can run on a restored record")
	}
	if got := o.QueuedMessages(); len(got) != 0 {
		t.Errorf("QueuedMessages() = %v, want empty", got)
	}
	if got := o.DrainQueuedMessages(); len(got) != 0 {
		t.Errorf("DrainQueuedMessages() = %v, want empty", got)
	}

	// The history survives every refused mutation.
	if got := len(o.History()); got != 1 {
		t.Errorf("History() holds %d messages after refused mutations, want 1", got)
	}
}

// TestRestoredSubagentEventsChannelClosed pins the forwarder-safety detail:
// Events() returns a CLOSED channel so ForwardOrchestratorEvents's range
// terminates instead of leaking a goroutine per restored child (a nil
// channel would block that range forever).
func TestRestoredSubagentEventsChannelClosed(t *testing.T) {
	o := &RestoredSubagentOrchestrator{id: "x"}
	ch := o.Events()
	if ch == nil {
		t.Fatal("Events() = nil, want a closed channel")
	}
	// A receive must deliver immediately (closed), not block.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("Events() channel delivered a value, want closed")
		}
	default:
		t.Fatal("Events() channel is open; the forwarder goroutine would leak")
	}
}

// TestRestoredSubagentMissingHistoryIsEmptyListed pins the resume-tolerance
// contract: a record whose history file never materialized (persistence
// disabled, or the child produced nothing) still restores — with an empty
// history — rather than failing.
func TestRestoredSubagentMissingHistoryIsEmptyListed(t *testing.T) {
	tmp := t.TempDir()
	original := session.SetSessionDirOverrideForTest(func() (string, error) { return tmp, nil })
	t.Cleanup(func() { session.SetSessionDirOverrideForTest(original) })
	path, err := session.SubagentHistoryPath("session-restore-3", "coder-subagent-9")
	if err != nil {
		t.Fatalf("SubagentHistoryPath: %v", err)
	}

	o, err := NewRestoredSubagentOrchestrator("coder-subagent-9", "coder", "g", path, "")
	if err != nil {
		t.Fatalf("missing history file must not fail the restore: %v", err)
	}
	if got := len(o.History()); got != 0 {
		t.Errorf("History() holds %d messages, want 0", got)
	}
}

// TestRestoredSubagentCorruptHistoryIsReported pins the integrity contract:
// the manifest pointed at preserved work; a file that cannot be decoded is
// an error, never a silently empty listing.
func TestRestoredSubagentCorruptHistoryIsReported(t *testing.T) {
	tmp := t.TempDir()
	original := session.SetSessionDirOverrideForTest(func() (string, error) { return tmp, nil })
	t.Cleanup(func() { session.SetSessionDirOverrideForTest(original) })
	path, err := session.SubagentHistoryPath("session-restore-4", "coder-subagent-2")
	if err != nil {
		t.Fatalf("SubagentHistoryPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := NewRestoredSubagentOrchestrator("coder-subagent-2", "coder", "g", path, ""); err == nil {
		t.Fatal("corrupt history must fail the restore, got nil error")
	} else if !strings.Contains(err.Error(), "coder-subagent-2") {
		t.Errorf("error must name the child, got %v", err)
	}
}

// TestRestoredSubagentMaxTokensZero pins the TUI-safety detail: MaxTokens
// returns 0 (context-size unknown) — a real ContextSize needs a live client,
// which a restored record deliberately has not. The TUI's context bar and
// the preflight check both treat 0 as "unknown/skip".
func TestRestoredSubagentMaxTokensZero(t *testing.T) {
	o := &RestoredSubagentOrchestrator{id: "x"}
	if got := o.MaxTokens(); got != 0 {
		t.Errorf("MaxTokens() = %d, want 0 (unknown)", got)
	}
	if o.SupportsVision() {
		t.Error("SupportsVision() = true, want false")
	}
	if got := o.Children(); got != nil {
		t.Errorf("Children() = %v, want nil", got)
	}
	if got := o.Parent(); got != nil {
		t.Errorf("Parent() = %v, want nil", got)
	}
	if _, ok := o.Context().Deadline(); ok {
		t.Error("Context() must be a plain background context")
	}
}

// TestRestoredSubagentEmptyIDRejected pins the guard against an ID-less
// record: the TUI keys state and focus by ID, so an empty one is refuse,
// not degrade.
func TestRestoredSubagentEmptyIDRejected(t *testing.T) {
	if _, err := NewRestoredSubagentOrchestrator("", "coder", "g", filepath.Join(t.TempDir(), "x.json"), ""); err == nil {
		t.Fatal("empty ID must fail the restore")
	}
}

// TestRestoredSubagentStatusTextDefault pins the status-text plumbing: an
// empty override yields the default restored wording; a caller-supplied one
// passes through (the resume layer appends the manifest cause).
func TestRestoredSubagentStatusTextDefault(t *testing.T) {
	o, err := NewRestoredSubagentOrchestrator("r0", "coder", "g", "", "custom cause line")
	if err != nil {
		t.Fatalf("NewRestoredSubagentOrchestrator: %v", err)
	}
	if got := o.StatusText(); got != "custom cause line" {
		t.Errorf("StatusText() = %q, want the passed-through override", got)
	}
	if RestoredSubagentStatus == "" {
		t.Fatal("RestoredSubagentStatus constant must not be empty")
	}
}
