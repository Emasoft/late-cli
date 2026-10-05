package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"late/internal/client"
	"late/internal/session"
)

// TestDanglingSubagentInterruptedTextDirective pins the Phase C directive
// wording for interrupted children whose history file EXISTS at notify
// time: the exact spawn_subagent resume JSON snippet, the
// ignored-parameters note, and the do-not-respawn warning.
func TestDanglingSubagentInterruptedTextDirective(t *testing.T) {
	historyPath := filepath.Join(t.TempDir(), "coder-subagent-0.json")
	if err := os.WriteFile(historyPath, []byte("[]"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	transcriptPath := filepath.Join(t.TempDir(), "transcript.md")
	if err := os.WriteFile(transcriptPath, []byte("transcript"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got := danglingSubagentInterruptedText("coder-subagent-0", "coder", historyPath, transcriptPath)
	for _, want := range []string{
		"Subagent coder-subagent-0 (coder) was interrupted by a previous late exit",
		"Its full work state is preserved",
		`{"resume": "coder-subagent-0"}`,
		"all other parameters are ignored",
		"restores with its complete history and continues its task",
		"Do NOT re-state the goal or spawn a new agent for this work unless resume fails",
		historyPath,
		transcriptPath,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("directive wording missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "DISABLED") {
		t.Errorf("existing history file must keep the preserved wording, got:\n%s", got)
	}
}

// TestDanglingSubagentInterruptedTextPersistenceDisabled pins the honest
// wording when the child's history file does NOT exist at notify time:
// persistence was disabled for that run, no resume directive is advertised
// (it can only fail with "no persisted history"), and the parent is told to
// re-dispatch. Covers both the empty-path (persistence off) and the
// path-set-but-file-missing variants.
func TestDanglingSubagentInterruptedTextPersistenceDisabled(t *testing.T) {
	t.Run("empty history path (persistence disabled for the run)", func(t *testing.T) {
		got := danglingSubagentInterruptedText("coder-subagent-1", "researcher", "", "")
		for _, want := range []string{
			"Subagent coder-subagent-1 (researcher) was interrupted",
			"subagent history persistence was DISABLED for this session",
			"enable save-subagent-histories or update late",
			"its in-flight work could not be preserved",
			"re-dispatch the mission",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("disabled wording missing %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, `"resume"`) {
			t.Errorf("disabled wording must not advertise a resume call: %s", got)
		}
		if strings.Contains(got, "preserved in the session manifest") {
			t.Errorf("disabled wording must not claim manifest-preserved work: %s", got)
		}
	})

	t.Run("history path set but file missing at notify time", func(t *testing.T) {
		gone := filepath.Join(t.TempDir(), "deleted.json")
		got := danglingSubagentInterruptedText("coder-subagent-2", "coder", gone, "")
		if !strings.Contains(got, "subagent history persistence was DISABLED for this session") {
			t.Errorf("missing history file must use the disabled wording:\n%s", got)
		}
		if strings.Contains(got, `"resume"`) {
			t.Errorf("missing history file must not advertise a resume call: %s", got)
		}
	})
}

// TestDanglingSubagentInterruptedTextUnknownCallID pins the generic
// fallback: with no manifest record there is no resume ID to advertise, so
// the wording keeps the historical review-and-continue shape.
func TestDanglingSubagentInterruptedTextUnknownCallID(t *testing.T) {
	got := danglingSubagentInterruptedText("", "", "", "")
	if strings.Contains(got, `"resume"`) {
		t.Errorf("generic fallback must not advertise a resume call: %s", got)
	}
	if !strings.Contains(got, "Review it and continue or re-spawn as needed.") {
		t.Errorf("generic fallback lost the review wording: %s", got)
	}
}

// TestTerminalResumeError pins the model-facing error for terminal records.
func TestTerminalResumeError(t *testing.T) {
	completed := terminalResumeError(&session.SubagentRecord{ID: "a-1", Status: session.SubagentStatusCompleted})
	if !strings.Contains(completed.Error(), "agent a-1 already terminated (completed)") ||
		!strings.Contains(completed.Error(), "spawn a fresh agent instead") {
		t.Errorf("completed error = %v", completed)
	}

	failed := terminalResumeError(&session.SubagentRecord{ID: "a-2", Status: session.SubagentStatusFailed, Cause: "crashed: boom"})
	if !strings.Contains(failed.Error(), "agent a-2 already terminated (crashed: boom)") {
		t.Errorf("failed error = %v", failed)
	}

	// A failed record without a cause falls back to the status name.
	noCause := terminalResumeError(&session.SubagentRecord{ID: "a-3", Status: session.SubagentStatusFailed})
	if !strings.Contains(noCause.Error(), "agent a-3 already terminated (failed)") {
		t.Errorf("no-cause error = %v", noCause)
	}
}

// TestValidateResumeRecord pins the resumability gate: only a
// running-at-read (interrupted) record WITH a persisted history passes.
func TestValidateResumeRecord(t *testing.T) {
	interrupted := &session.SubagentRecord{ID: "a-1", Status: session.SubagentStatusRunning, HistoryPath: "/s/a-1.json"}
	if err := validateResumeRecord(interrupted); err != nil {
		t.Errorf("interrupted record with history = %v, want nil", err)
	}

	noHistory := &session.SubagentRecord{ID: "a-2", Status: session.SubagentStatusRunning}
	if err := validateResumeRecord(noHistory); err == nil || !strings.Contains(err.Error(), "no persisted history") {
		t.Errorf("interrupted record without history = %v, want the no-history error", err)
	}

	for status, name := range map[string]string{
		session.SubagentStatusCompleted: "completed",
		session.SubagentStatusFailed:    "failed",
		session.SubagentStatusCancelled: "cancelled",
	} {
		rec := &session.SubagentRecord{ID: "a-x", Status: status, HistoryPath: "/s/a-x.json"}
		if err := validateResumeRecord(rec); err == nil || !strings.Contains(err.Error(), "already terminated") {
			t.Errorf("%s record = %v, want the terminated error", name, err)
		}
	}

	unknown := &session.SubagentRecord{ID: "a-9", Status: "banana", HistoryPath: "/s/a-9.json"}
	if err := validateResumeRecord(unknown); err == nil || !strings.Contains(err.Error(), "unknown manifest status") {
		t.Errorf("unknown status = %v, want the unknown-status error", err)
	}
}

// TestResumeWorktreeWordingThroughSynthesis runs a worktree child's
// interrupted wording through the full synthesis path: the resumed record
// (carrying the Phase C fields) produces the directive resume call for the
// preserved child.
func TestResumeWorktreeWordingThroughSynthesis(t *testing.T) {
	const sessionID = "session-phasec-worktree-wording"
	sess := runnerTestSession(t, sessionID)

	if err := sess.AddAssistantMessageWithTools("", "", []client.ToolCall{{
		Index: 0, ID: "call_wt", Type: "function",
		Function: client.FunctionCall{Name: spawnSubagentToolName, Arguments: `{"goal":"port","agent_type":"coder"}`},
	}}); err != nil {
		t.Fatalf("AddAssistantMessageWithTools: %v", err)
	}

	worktree := filepath.Join(t.TempDir(), "repo-worktrees", "feature-x")
	// The directive wording is only honest when the child's history file
	// exists at notify time — give the record a real one.
	historyPath := filepath.Join(t.TempDir(), "coder-subagent-12.json")
	if err := os.WriteFile(historyPath, []byte("[]"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: "coder-subagent-12", AgentType: "coder", Goal: "port",
		Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Hour),
		HistoryPath: historyPath, WorktreePath: worktree, WorkingDir: "/repo",
		ResumeCount: 1,
	}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}

	saved, err := session.LoadHistory(sess.HistoryPath)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	resumed := session.New(nil, sess.HistoryPath, saved, "prompt", false)
	if _, err := synthesizeDanglingSpawnResults(resumed); err != nil {
		t.Fatalf("synthesizeDanglingSpawnResults: %v", err)
	}
	var text string
	for _, m := range resumed.History {
		if m.Role == "tool" && m.ToolCallID == "call_wt" {
			text = m.Content.String()
		}
	}
	if text == "" {
		t.Fatal("no synthesized tool result for the dangling call")
	}
	for _, want := range []string{
		`{"resume": "coder-subagent-12"}`,
		"all other parameters are ignored",
		"Do NOT re-state the goal",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("synthesized wording missing %q:\n%s", want, text)
		}
	}
}
