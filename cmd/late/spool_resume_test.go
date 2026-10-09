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

// writeSpoolTranscript creates one partial transcript file with the given
// content under dir.
func writeSpoolTranscript(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", name, err)
	}
}

// newResumedChildForTest builds a live resumed child over a persisted
// history, exactly like the resume runner test does, so the spool-note
// synthesis runs against a real loaded history.
func newResumedChildForTest(t *testing.T, sessionID, childID string) (common.Orchestrator, string) {
	t.Helper()
	root := orchestrator.NewBaseOrchestrator("root-"+childID, runnerTestSession(t, sessionID), nil, 0)
	historyPath, err := session.SubagentHistoryPath(sessionID, childID)
	if err != nil {
		t.Fatalf("SubagentHistoryPath: %v", err)
	}
	preserved := []client.ChatMessage{
		{Role: "user", Content: client.TextContent("Goal: run the build")},
		{Role: "assistant", Content: client.TextContent("Running the build now.")},
	}
	if err := os.MkdirAll(filepath.Dir(historyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := session.SaveHistory(historyPath, preserved); err != nil {
		t.Fatalf("SaveHistory: %v", err)
	}
	c := client.NewClient(client.Config{BaseURL: "http://localhost:8080"})
	record := session.SubagentRecord{
		ID: childID, AgentType: "coder", Goal: "run the build",
		Status: session.SubagentStatusRunning, SpawnedAt: time.Now().Add(-time.Hour),
		HistoryPath: historyPath, WorkingDir: "/repo",
	}
	child, _, err := agent.NewResumedSubagentOrchestrator(c, record, "coder", map[string]bool{}, false, false, 10, root, nil)
	if err != nil {
		t.Fatalf("NewResumedSubagentOrchestrator: %v", err)
	}
	return child, historyPath
}

// TestResumeSynthesizesSpoolTranscriptNotes is the cmd/late half of perfect
// resume: orphaned shell-call transcripts left by the previous exit are
// surfaced as harness notes in the restored child's LOADED history (user
// role, [late harness] prefix, full retry-pointer wording), and each
// surfaced file is consumed by renaming to .consumed — a second synthesis
// finds nothing, so exactly one resume ever surfaces a transcript.
func TestResumeSynthesizesSpoolTranscriptNotes(t *testing.T) {
	child, historyPath := newResumedChildForTest(t, "session-spool", "coder-subagent-spool")
	spoolDir := spoolDirForHistoryPath(historyPath)

	// Two orphans (name order = chronological: nano-1000 is the older call),
	// plus noise a synthesis must ignore: an already-consumed transcript and
	// a promoted archive.
	writeSpoolTranscript(t, spoolDir, "partial-1000.txt", "compiling... 40%")
	writeSpoolTranscript(t, spoolDir, "partial-2000.txt", "test output before the kill")
	writeSpoolTranscript(t, spoolDir, "partial-3000.txt.consumed", "already surfaced once")
	writeSpoolTranscript(t, spoolDir, "deadbeef.txt", "a promoted archive, not a partial")

	noted := synthesizeSpoolTranscriptNotes(child, spoolDir)
	if noted != 2 {
		t.Fatalf("synthesized %d note(s), want 2", noted)
	}

	loaded := child.History()
	if len(loaded) != 4 {
		t.Fatalf("loaded history holds %d messages, want 2 preserved + 2 notes", len(loaded))
	}
	// Oldest transcript first: call IDs embed start times, so name order is
	// chronological and the notes read in execution order.
	for i, want := range []string{"partial-1000.txt", "partial-2000.txt"} {
		note := loaded[2+i]
		if note.Role != "user" {
			t.Errorf("note %d role = %q, want user (a tool-role note without a matching ToolCallID would be dropped by SanitizeForRequest)", i, note.Role)
		}
		content := note.Content.String()
		if !strings.HasPrefix(content, "[late harness] ") {
			t.Errorf("note %d content %q missing the [late harness] prefix", i, content)
		}
		if !strings.Contains(content, "tool call failed: interrupted by a previous late exit") {
			t.Errorf("note %d content %q missing the interrupted wording", i, content)
		}
		if !strings.Contains(content, "The transcript of its execution was preserved here — read it to decide whether to retry the tool call: "+filepath.Join(spoolDir, want)) {
			t.Errorf("note %d content %q missing the retry pointer to %s", i, content, want)
		}
	}

	// Consumed: both surfaced files renamed; the noise untouched.
	for _, name := range []string{"partial-1000.txt.consumed", "partial-2000.txt.consumed"} {
		if _, err := os.Stat(filepath.Join(spoolDir, name)); err != nil {
			t.Errorf("expected consumed marker %s: %v", name, err)
		}
	}
	for _, name := range []string{"partial-1000.txt", "partial-2000.txt", "partial-3000.txt.consumed", "deadbeef.txt"} {
		if name == "partial-1000.txt" || name == "partial-2000.txt" {
			if _, err := os.Stat(filepath.Join(spoolDir, name)); !os.IsNotExist(err) {
				t.Errorf("surfaced transcript %s was not consumed (err=%v)", name, err)
			}
			continue
		}
		if _, err := os.Stat(filepath.Join(spoolDir, name)); err != nil {
			t.Errorf("noise file %s must be untouched: %v", name, err)
		}
	}

	// Idempotent by consumption: a second pass (the next resume) finds no
	// orphans and appends nothing.
	if again := synthesizeSpoolTranscriptNotes(child, spoolDir); again != 0 {
		t.Fatalf("second synthesis surfaced %d note(s), want 0", again)
	}
	if len(child.History()) != 4 {
		t.Fatalf("history grew to %d messages after the second synthesis", len(child.History()))
	}
}

// TestSanitizeForRequestKeepsHarnessSpoolNotes validates the message-shape
// decision end to end: the synthesized notes survive the request-time
// sanitizer (user role), while a hypothetical tool-role note without a
// matching assistant tool_call would be dropped as a dangling result — the
// exact failure mode the user-role shape avoids.
func TestSanitizeForRequestKeepsHarnessSpoolNotes(t *testing.T) {
	child, historyPath := newResumedChildForTest(t, "session-spool-sanitize", "coder-subagent-spool-s2")
	spoolDir := spoolDirForHistoryPath(historyPath)
	writeSpoolTranscript(t, spoolDir, "partial-1000.txt", "make: *** error 1")
	if noted := synthesizeSpoolTranscriptNotes(child, spoolDir); noted != 1 {
		t.Fatalf("synthesized %d note(s), want 1", noted)
	}

	sanitized := session.SanitizeForRequest(child.History())
	if len(sanitized) != len(child.History()) {
		t.Fatalf("sanitizer dropped a message: %d in, %d out — the spool note must survive the request render", len(child.History()), len(sanitized))
	}
	found := false
	for _, m := range sanitized {
		if strings.Contains(m.Content.String(), "partial-1000.txt") {
			found = true
			if m.Role != "user" {
				t.Fatalf("spool note reached the request as role %q, want user", m.Role)
			}
		}
	}
	if !found {
		t.Fatal("spool note missing from the sanitized request messages")
	}

	// The counterfactual: the same note as a dangling tool result would be
	// silently dropped — this is why the synthesis uses the user shape.
	dangling := session.SanitizeForRequest([]client.ChatMessage{
		{Role: "user", Content: client.TextContent("Goal")},
		{Role: "tool", ToolCallID: "no-such-call", Content: client.TextContent("[late harness] orphaned note")},
	})
	if len(dangling) != 1 {
		t.Fatalf("dangling tool-role note was not dropped: %d messages out", len(dangling))
	}
}

// TestOrphanedSpoolTranscripts pins the discovery helper: only
// partial-*.txt is surfaced, name-ordered; missing/empty dirs yield nil.
func TestOrphanedSpoolTranscripts(t *testing.T) {
	if got := orphanedSpoolTranscripts(""); got != nil {
		t.Fatalf("orphanedSpoolTranscripts(\"\") = %v, want nil", got)
	}
	if got := orphanedSpoolTranscripts(filepath.Join(t.TempDir(), "missing")); got != nil {
		t.Fatalf("missing dir = %v, want nil", got)
	}
	dir := t.TempDir()
	writeSpoolTranscript(t, dir, "partial-b.txt", "2")
	writeSpoolTranscript(t, dir, "partial-a.txt", "1")
	writeSpoolTranscript(t, dir, "partial-c.txt.consumed", "3")
	writeSpoolTranscript(t, dir, "archive.txt", "4")
	got := orphanedSpoolTranscripts(dir)
	if len(got) != 2 || filepath.Base(got[0]) != "partial-a.txt" || filepath.Base(got[1]) != "partial-b.txt" {
		t.Fatalf("orphans = %v, want [partial-a.txt partial-b.txt] in name order", got)
	}
}

// TestSpoolDirForHistoryPath pins the session-layout mapping: a child
// history at <session>/subagents/<id>.json maps to the shared
// <session>/tool-outputs folder; empty paths disable the synthesis.
func TestSpoolDirForHistoryPath(t *testing.T) {
	if got := spoolDirForHistoryPath(""); got != "" {
		t.Fatalf("empty history path = %q, want \"\"", got)
	}
	got := spoolDirForHistoryPath("/sessions/s1/subagents/coder-subagent-1.json")
	if want := filepath.Join("/sessions/s1", "tool-outputs"); got != want {
		t.Fatalf("spoolDirForHistoryPath = %q, want %q", got, want)
	}
}
