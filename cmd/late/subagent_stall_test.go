package main

// Stall auto-resume tests at the cmd/late level (worker S): the outcome
// classifier and the subagent_results lookup must recognize the
// "stalled:" kill reason the orchestrator watchdog records, write the
// terminal manifest record with the stall cause, and hand the parent the
// resume directive — the notification half of stall → cancel → notify →
// resume.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"late/internal/orchestrator"
	"late/internal/session"
	"late/internal/tool"
)

// stalledChildStub is a real BaseOrchestrator (full common.Orchestrator,
// Session(), History()) with IdleKillReason overridden to return the stall
// verdict the way the watchdog's stall branch leaves it in production
// (internal field, so the test seeds it through the override).
type stalledChildStub struct {
	*orchestrator.BaseOrchestrator
	reason string
}

func (s *stalledChildStub) IdleKillReason() string { return s.reason }

// TestClassifyStalledChildWritesResumeDirective drives the classifier with
// a stalled child: the terminal manifest record must carry the stall cause
// verbatim, and the parent-facing result must spell out the resume
// directive with the child ID.
func TestClassifyStalledChildWritesResumeDirective(t *testing.T) {
	const sessionID = "session-stall-classify"
	sess := runnerTestSession(t, sessionID)
	const childID = "coder-subagent-40"
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: childID, AgentType: "coder", Goal: "long work",
		Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Minute),
	}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}

	stallCause := "stalled: no activity for 2m0s (threshold 1m0s); blocked in-flight tool call; last transcript entries: user: long work"
	child := &stalledChildStub{
		BaseOrchestrator: orchestrator.NewBaseOrchestrator(childID, sess, nil, 0),
		reason:           stallCause,
	}

	final, err := classifyAndReportSubagentOutcome(sess, child, "coder", "long work", "", context.Canceled,
		subagentOutcomeContext{runCtx: context.Background(), runBudget: 0})
	if err != nil {
		t.Fatalf("classifyAndReportSubagentOutcome: %v", err)
	}

	// The parent-facing text carries the resume directive with the ID.
	if !strings.Contains(final, `"resume": "coder-subagent-40"`) {
		t.Errorf("final text missing the resume directive:\n%s", final)
	}
	if !strings.Contains(final, stallCause) {
		t.Errorf("final text missing the stall cause:\n%s", final)
	}

	// Terminal manifest record: failed, cause recorded, transcript written.
	manifest, merr := session.LoadSubagentManifest(sessionID)
	if merr != nil {
		t.Fatalf("LoadSubagentManifest: %v", merr)
	}
	rec, ok := manifest.Get(childID)
	if !ok {
		t.Fatal("manifest record missing after classification")
	}
	if rec.Status != session.SubagentStatusFailed {
		t.Errorf("Status = %q, want failed", rec.Status)
	}
	if rec.Cause != stallCause {
		t.Errorf("Cause = %q, want %q", rec.Cause, stallCause)
	}
	if rec.TranscriptPath == "" {
		t.Error("TranscriptPath not recorded for the stalled child")
	} else if _, err := os.Stat(rec.TranscriptPath); err != nil {
		t.Errorf("transcript file missing: %v", err)
	}
}

// TestSchedulerStalledChildNotifiesAndDrains is the scheduler-level stall
// auto-resume test: a stalled child (run closure returning the stall
// completion) must (a) append the parent notification WITH the resume
// directive, (b) write the terminal manifest record with the stall cause,
// and (c) drain a queued child behind it — a wedged serial child must
// never hold the queue forever.
func TestSchedulerStalledChildNotifiesAndDrains(t *testing.T) {
	const sessionID = "session-stall-scheduler"
	sess := runnerTestSession(t, sessionID)
	rec := &statusRecorder{}
	s := newSubagentScheduler(sess, rec.write)

	const stalledID = "coder-subagent-50"
	const queuedID = "coder-subagent-51"
	for _, id := range []string{stalledID, queuedID} {
		if err := sess.SaveSubagentRecord(session.SubagentRecord{
			ID: id, AgentType: "coder", Goal: "work",
			Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Minute),
		}); err != nil {
			t.Fatalf("SaveSubagentRecord(%s): %v", id, err)
		}
	}

	stallCause := "stalled: no activity for 90s (threshold 1m0s); blocked in-flight tool call"
	stalledRun := func() subagentCompletion {
		// Mirror the outcome path of a stalled background child: terminal
		// write with the stall cause, result artifact, completion payload.
		transcript := filepath.Join(t.TempDir(), "transcript.md")
		if err := sess.MarkSubagentStatus(stalledID, session.SubagentStatusFailed, stallCause, "", transcript); err != nil {
			t.Fatalf("MarkSubagentStatus: %v", err)
		}
		res := fmt.Sprintf("subagent %s stalled (%s) — cancelled, state preserved; resumable via spawn {\"resume\": %q}.",
			stalledID, stallCause, stalledID)
		recordSubagentResult(sess, sessionID, stalledID, res)
		return subagentCompletion{
			ID: stalledID, AgentType: "coder",
			Status: session.SubagentStatusFailed, Cause: stallCause,
			Preview: previewText(res, manifestResultPreviewLimit),
		}
	}

	s.Launch(stalledID, "coder", tool.SubagentExecutionSerial, stalledRun)
	// While the stalled serial child is "live", a parallel child queues —
	// exactly the wedged-serial-queue scenario from the error log.
	s.Launch(queuedID, "coder", tool.SubagentExecutionParallel, func() subagentCompletion {
		return subagentCompletion{
			ID: queuedID, AgentType: "coder", Status: session.SubagentStatusCompleted,
		}
	})

	eventually(t, 2*time.Second, "stalled child notification with resume directive", func() bool {
		for _, m := range sess.HistorySnapshot() {
			if m.Role != "user" {
				continue
			}
			text := m.Content.String()
			if strings.Contains(text, stalledID) &&
				strings.Contains(text, "stalled") &&
				strings.Contains(text, `"resume": "coder-subagent-50"`) {
				return true
			}
		}
		return false
	})

	// The queued child drained after the stalled one left the running set.
	eventually(t, 2*time.Second, "queue drained behind the stalled child", func() bool {
		return rec.count(queuedID, session.SubagentStatusRunning) > 0
	})

	// Let the scheduler finish EVERYTHING before the test returns: the
	// completion goroutines append into the parent session (SessionDir
	// global) on their own schedule, and the test's cleanup restores
	// SessionDir — a return before the last goroutine's notify is a
	// data race the race detector correctly flags.
	eventually(t, 2*time.Second, "scheduler fully idle", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.running) == 0 && len(s.parallelQueue) == 0 && len(s.serialQueue) == 0
	})

	// Terminal manifest record for the stalled child.
	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	recStalled, ok := manifest.Get(stalledID)
	if !ok || recStalled.Status != session.SubagentStatusFailed {
		t.Fatalf("stalled record = %+v (found=%v), want failed", recStalled, ok)
	}
	if recStalled.Cause != stallCause {
		t.Errorf("Cause = %q, want %q", recStalled.Cause, stallCause)
	}
}

// TestSubagentResultsStalledDirective pins the lookup branch: a failed
// record whose cause starts with "stalled:" answers with the resume
// directive instead of the generic terminal wording.
func TestSubagentResultsStalledDirective(t *testing.T) {
	const sessionID = "session-stall-lookup"
	sess := runnerTestSession(t, sessionID)
	const childID = "coder-subagent-60"
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: childID, AgentType: "coder", Goal: "work",
		Status:    session.SubagentStatusFailed,
		Cause:     "stalled: no activity for 2m0s (threshold 1m0s); blocked in-flight tool call",
		SpawnedAt: nowMinus(t, time.Hour),
	}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}

	lookup := subagentResultsLookup(sess, nil, sessionID)
	answer, err := lookup(context.Background(), childID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !strings.Contains(answer, "stalled") {
		t.Errorf("answer = %q, want it to mention the stall", answer)
	}
	if !strings.Contains(answer, `"resume": "coder-subagent-60"`) {
		t.Errorf("answer = %q, want the resume directive", answer)
	}
}
