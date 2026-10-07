package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"late/internal/common"
	"late/internal/orchestrator"
	"late/internal/session"
	"late/internal/tool"
)

// gatedRun is a scheduler run closure the test releases explicitly, so
// completion order is deterministic regardless of goroutine scheduling.
type gatedRun struct {
	release chan struct{}
}

func newGatedRun() *gatedRun { return &gatedRun{release: make(chan struct{})} }

func (g *gatedRun) run(completion subagentCompletion) subagentCompletion {
	<-g.release
	return completion
}

func (g *gatedRun) letFinish() { close(g.release) }

// statusRecorder collects scheduler-driven manifest transitions from
// whatever goroutine the scheduler calls it on.
type statusRecorder struct {
	mu          sync.Mutex
	transitions [][2]string // {id, status}
}

func (r *statusRecorder) write(id, status string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transitions = append(r.transitions, [2]string{id, status})
	return nil
}

func (r *statusRecorder) count(id, status string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, t := range r.transitions {
		if t[0] == id && t[1] == status {
			n++
		}
	}
	return n
}

// eventually polls cond until it holds or the timeout elapses. The
// scheduler's completions and drains run on their own goroutines, so every
// transition a test asserts must be observed, not assumed.
func eventually(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v: %s", timeout, desc)
}

func completionFor(id, agentType, status string) subagentCompletion {
	return subagentCompletion{ID: id, AgentType: agentType, Status: status, Preview: "preview of " + id}
}

// TestSchedulerSerialRunsAloneParallelQueuesThenBatches pins the core
// scheduling rules: a parallel launch while a serial child runs QUEUES
// (positions in arrival order), a second serial child queues behind the
// running one, the serial chain launches one-at-a-time as each finishes,
// and only after the serial chain fully drains does the queued parallel
// batch launch — together.
func TestSchedulerSerialRunsAloneParallelQueuesThenBatches(t *testing.T) {
	s := newSubagentScheduler(nil, nil)

	s1 := newGatedRun()
	p1 := newGatedRun()
	p2 := newGatedRun()
	s2 := newGatedRun()

	if launched, pos := s.Launch("coder-subagent-1", "coder", tool.SubagentExecutionSerial, func() subagentCompletion {
		return s1.run(completionFor("coder-subagent-1", "coder", session.SubagentStatusCompleted))
	}); !launched || pos != 0 {
		t.Fatalf("first serial launch: launched=%v pos=%d, want true/0", launched, pos)
	}
	eventually(t, time.Second, "serial child live", func() bool { return s.State("coder-subagent-1") == session.SubagentStatusRunning+" (serial)" })

	// Parallel arrivals during the serial run queue, in arrival order.
	if launched, pos := s.Launch("researcher-subagent-2", "researcher", tool.SubagentExecutionParallel, func() subagentCompletion {
		return p1.run(completionFor("researcher-subagent-2", "researcher", session.SubagentStatusCompleted))
	}); launched || pos != 1 {
		t.Fatalf("parallel during serial: launched=%v pos=%d, want false/1", launched, pos)
	}
	if launched, pos := s.Launch("researcher-subagent-3", "researcher", tool.SubagentExecutionParallel, func() subagentCompletion {
		return p2.run(completionFor("researcher-subagent-3", "researcher", session.SubagentStatusCompleted))
	}); launched || pos != 2 {
		t.Fatalf("second parallel during serial: launched=%v pos=%d, want false/2", launched, pos)
	}
	// A second serial child also queues (serial runs ALONE — nothing else).
	if launched, pos := s.Launch("coder-subagent-4", "coder", tool.SubagentExecutionSerial, func() subagentCompletion {
		return s2.run(completionFor("coder-subagent-4", "coder", session.SubagentStatusCompleted))
	}); launched || pos != 1 {
		t.Fatalf("serial behind serial: launched=%v pos=%d, want false/1", launched, pos)
	}

	// Serial finishes → the NEXT serial launches (chain), parallel batch
	// must still wait behind the serial queue.
	s1.letFinish()
	eventually(t, time.Second, "second serial child live after first finished", func() bool {
		return s.State("coder-subagent-4") == session.SubagentStatusRunning+" (serial)"
	})
	if state := s.State("researcher-subagent-2"); !strings.HasPrefix(state, session.SubagentStatusQueued) {
		t.Errorf("parallel still queued while serial chain drains, state = %q", state)
	}

	// Serial chain drains → the parallel queue launches as ONE batch.
	s2.letFinish()
	eventually(t, time.Second, "both parallel children live after serial chain drained", func() bool {
		return strings.HasPrefix(s.State("researcher-subagent-2"), session.SubagentStatusRunning) &&
			strings.HasPrefix(s.State("researcher-subagent-3"), session.SubagentStatusRunning)
	})

	// Batch finishes → machine empty.
	p1.letFinish()
	p2.letFinish()
	eventually(t, time.Second, "scheduler empty after batch finished", func() bool {
		return s.State("researcher-subagent-2") == "" && s.State("researcher-subagent-3") == "" && s.State("coder-subagent-4") == ""
	})
}

// TestSchedulerParallelLaunchesImmediatelyAndSerialWaits: with no serial
// child live, parallel children start at once — even several at once — and
// a queued serial child only launches when the LAST parallel finishes, not
// an earlier one.
func TestSchedulerParallelLaunchesImmediatelyAndSerialWaits(t *testing.T) {
	s := newSubagentScheduler(nil, nil)

	p1 := newGatedRun()
	p2 := newGatedRun()
	p3 := newGatedRun()
	serial := newGatedRun()

	for i, g := range []*gatedRun{p1, p2, p3} {
		id := fmt.Sprintf("researcher-subagent-%d", i+1)
		if launched, pos := s.Launch(id, "researcher", tool.SubagentExecutionParallel, func() subagentCompletion {
			return g.run(completionFor(id, "researcher", session.SubagentStatusCompleted))
		}); !launched || pos != 0 {
			t.Fatalf("parallel launch %d: launched=%v pos=%d, want true/0", i+1, launched, pos)
		}
	}
	// The serial child queues behind the live parallel batch.
	if launched, pos := s.Launch("coder-subagent-9", "coder", tool.SubagentExecutionSerial, func() subagentCompletion {
		return serial.run(completionFor("coder-subagent-9", "coder", session.SubagentStatusCompleted))
	}); launched || pos != 1 {
		t.Fatalf("serial during parallel batch: launched=%v pos=%d, want false/1", launched, pos)
	}

	// One of two parallels finishing must NOT unblock the serial child.
	p1.letFinish()
	eventually(t, time.Second, "first parallel drained", func() bool { return s.State("researcher-subagent-1") == "" })
	if state := s.State("coder-subagent-9"); !strings.HasPrefix(state, session.SubagentStatusQueued) {
		t.Errorf("serial launched while parallels still run: %q", state)
	}

	// The LAST parallel finishing unblocks the serial child.
	p2.letFinish()
	p3.letFinish()
	eventually(t, time.Second, "serial child live after parallel batch drained", func() bool {
		return s.State("coder-subagent-9") == session.SubagentStatusRunning+" (serial)"
	})
	serial.letFinish()
}

// TestSchedulerRecordsQueueAndLaunchStatuses checks the scheduler-driven
// manifest transitions: queued at enqueue (never for immediate launches),
// running again at actual launch.
func TestSchedulerRecordsQueueAndLaunchStatuses(t *testing.T) {
	rec := &statusRecorder{}
	s := newSubagentScheduler(nil, rec.write)

	p1 := newGatedRun()
	s1 := newGatedRun()

	// Immediate parallel launch: running already written by the child's
	// constructor — no queued stamp.
	if launched, _ := s.Launch("researcher-subagent-1", "researcher", tool.SubagentExecutionParallel, func() subagentCompletion {
		return p1.run(completionFor("researcher-subagent-1", "researcher", session.SubagentStatusCompleted))
	}); !launched {
		t.Fatal("parallel should launch immediately on an idle machine")
	}
	// Queued serial: queued stamp, then a running stamp at actual launch.
	s.Launch("coder-subagent-2", "coder", tool.SubagentExecutionSerial, func() subagentCompletion {
		return s1.run(completionFor("coder-subagent-2", "coder", session.SubagentStatusCompleted))
	})
	eventually(t, time.Second, "queued stamp recorded", func() bool { return rec.count("coder-subagent-2", session.SubagentStatusQueued) == 1 })
	if rec.count("researcher-subagent-1", session.SubagentStatusQueued) != 0 {
		t.Error("immediately launched child must not be stamped queued")
	}

	p1.letFinish()
	eventually(t, time.Second, "drained serial stamped running", func() bool {
		return rec.count("coder-subagent-2", session.SubagentStatusRunning) == 1
	})
	s1.letFinish()
}

// TestBackgroundChildCompletionNotifiesParentAndPersistsResult is the
// background delivery path end-to-end at the unit level: the run closure
// writes the full-result artifact the way launchBackgroundSubagent does,
// the scheduler appends the [late harness] notification into the parent
// session, and the manifest record points at the result file.
func TestBackgroundChildCompletionNotifiesParentAndPersistsResult(t *testing.T) {
	const sessionID = "session-20260101-090000"
	sess := runnerTestSession(t, sessionID)
	rec := &statusRecorder{}
	s := newSubagentScheduler(sess, rec.write)

	const childID = "coder-subagent-7"
	// The child's constructor writes the spawn record; mirror it so the
	// result-path write has a record to land on.
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: childID, AgentType: "coder", Goal: "do background work",
		Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Minute),
	}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}

	full := "The subagent successfully completed its task. Final result:\n\nAll work finished."
	g := newGatedRun()
	s.Launch(childID, "coder", tool.SubagentExecutionParallel, func() subagentCompletion {
		// Same sequence as launchBackgroundSubagent's run closure.
		recordSubagentResult(sess, sessionID, childID, full)
		return subagentCompletion{
			ID: childID, AgentType: "coder",
			Status:  session.SubagentStatusCompleted,
			Preview: previewText(full, manifestResultPreviewLimit),
		}
	})
	g.letFinish()

	wantNote := fmt.Sprintf("[late harness] subagent %s (coder) %s. Result preview: %s. Full result: call subagent_results with {\"id\": %q}.",
		childID, session.SubagentStatusCompleted, previewText(full, manifestResultPreviewLimit), childID)
	eventually(t, 2*time.Second, "notification appended to parent history", func() bool {
		for _, m := range sess.HistorySnapshot() {
			if m.Role == "user" && m.Content.String() == wantNote {
				return true
			}
		}
		return false
	})

	// Full result on disk, path recorded in the manifest.
	resultPath, err := session.SubagentResultPath(sessionID, childID)
	if err != nil {
		t.Fatalf("SubagentResultPath: %v", err)
	}
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("result file not written: %v", err)
	}
	if string(data) != full {
		t.Errorf("result file = %q, want the full final text", string(data))
	}
	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	got, ok := manifest.Get(childID)
	if !ok || got.ResultPath != resultPath {
		t.Errorf("manifest ResultPath = %q (ok=%v), want %q", got.ResultPath, ok, resultPath)
	}
}

// TestSubagentResultsLookupTable drives the subagent_results closure through
// every state: live scheduler states, each manifest status, the full-result
// file, the preview fallback, and the not-found case.
func TestSubagentResultsLookupTable(t *testing.T) {
	const sessionID = "session-20260101-091000"
	sess := runnerTestSession(t, sessionID)
	rec := &statusRecorder{}
	sched := newSubagentScheduler(sess, rec.write)
	lookup := subagentResultsLookup(sess, sched, sessionID)

	// Seed manifest records for every persisted state.
	seed := map[string]session.SubagentRecord{
		"researcher-subagent-10": {ID: "researcher-subagent-10", AgentType: "researcher", Goal: "g", Status: session.SubagentStatusRunning},
		"researcher-subagent-11": {ID: "researcher-subagent-11", AgentType: "researcher", Goal: "g", Status: session.SubagentStatusQueued},
		"coder-subagent-12":      {ID: "coder-subagent-12", AgentType: "coder", Goal: "g", Status: session.SubagentStatusFrozen, HistoryPath: "/sessions/s/subagents/coder-subagent-12.json"},
		"coder-subagent-13":      {ID: "coder-subagent-13", AgentType: "coder", Goal: "g", Status: session.SubagentStatusCompleted},
		"coder-subagent-14":      {ID: "coder-subagent-14", AgentType: "coder", Goal: "g", Status: session.SubagentStatusCompleted, ResultPreview: "short preview"},
		"coder-subagent-15":      {ID: "coder-subagent-15", AgentType: "coder", Goal: "g", Status: session.SubagentStatusFailed, Cause: "crashed: boom", TranscriptPath: "/tmp/t.md"},
	}
	for _, r := range seed {
		if err := sess.SaveSubagentRecord(r); err != nil {
			t.Fatalf("SaveSubagentRecord(%s): %v", r.ID, err)
		}
	}
	// Full result artifact for the completed child with a ResultPath.
	fullPath, err := session.SubagentResultPath(sessionID, "coder-subagent-13")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte("THE FULL RESULT"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sess.MarkSubagentResultPath("coder-subagent-13", fullPath); err != nil {
		t.Fatal(err)
	}

	// A live parallel child the scheduler still holds.
	blocked := newGatedRun()
	sched.Launch("researcher-subagent-16", "researcher", tool.SubagentExecutionParallel, func() subagentCompletion {
		return blocked.run(completionFor("researcher-subagent-16", "researcher", session.SubagentStatusCompleted))
	})
	// A live-queued one (a running parallel blocks nothing for a second
	// parallel — use a serial child to force the queue).
	sched.Launch("researcher-subagent-17", "researcher", tool.SubagentExecutionSerial, func() subagentCompletion {
		return blocked.run(completionFor("researcher-subagent-17", "researcher", session.SubagentStatusCompleted))
	})
	eventually(t, time.Second, "serial child queued", func() bool {
		return strings.HasPrefix(sched.State("researcher-subagent-17"), session.SubagentStatusQueued)
	})

	cases := []struct {
		name     string
		id       string
		contains []string
	}{
		{"running in scheduler", "researcher-subagent-16", []string{"still running (parallel)"}},
		{"queued in scheduler", "researcher-subagent-17", []string{"still queued"}},
		{"manifest running (not scheduler)", "researcher-subagent-10", []string{"still running"}},
		{"manifest queued", "researcher-subagent-11", []string{"still queued"}},
		{"frozen points at resume", "coder-subagent-12", []string{"FROZEN", "\"resume\": \"coder-subagent-12\""}},
		{"completed reads full result file", "coder-subagent-13", []string{"completed", "THE FULL RESULT"}},
		{"completed without file falls back to preview", "coder-subagent-14", []string{"recorded preview only", "short preview"}},
		{"failed surfaces cause and transcript", "coder-subagent-15", []string{"failed", "crashed: boom", "/tmp/t.md"}},
		{"unknown id", "coder-subagent-99", []string{"No subagent \"coder-subagent-99\""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := lookup(context.Background(), tc.id)
			if err != nil {
				t.Fatalf("lookup(%s): %v", tc.id, err)
			}
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("lookup(%s) = %q, want it to contain %q", tc.id, got, want)
				}
			}
		})
	}

	// In-memory session: no manifest to consult.
	memLookup := subagentResultsLookup(sess, nil, "")
	got, err := memLookup(context.Background(), "coder-subagent-1")
	if err != nil || !strings.Contains(got, "no manifest") {
		t.Errorf("in-memory lookup = %q, err = %v; want the no-manifest hint", got, err)
	}

	// Drain before t.TempDir cleanup: the released runs append their
	// notifications (and persist the parent history) on scheduler
	// goroutines, which must not race RemoveAll.
	blocked.letFinish()
	eventually(t, 2*time.Second, "scheduler drained before cleanup", func() bool {
		return sched.State("researcher-subagent-16") == "" && sched.State("researcher-subagent-17") == ""
	})
}

// TestSubagentResultsToolShell pins the tool shell: parse, empty-id hint,
// lookup plumbing.
func TestSubagentResultsToolShell(t *testing.T) {
	toolObj := tool.SubagentResultsTool{Lookup: func(ctx context.Context, id string) (string, error) {
		if id != "coder-subagent-2" {
			return "", fmt.Errorf("unexpected id %q", id)
		}
		return "FULL", nil
	}}

	// Schema advertises the id parameter.
	var schema map[string]any
	if err := json.Unmarshal(toolObj.Parameters(), &schema); err != nil {
		t.Fatalf("schema not JSON: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["id"]; !ok {
		t.Error("schema missing the id property")
	}

	got, err := toolObj.Execute(context.Background(), json.RawMessage(`{"id":"coder-subagent-2"}`))
	if err != nil || got != "FULL" {
		t.Errorf("Execute = %q, %v; want FULL, nil", got, err)
	}

	got, err = toolObj.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("empty id should be an error result, not a Go error: %v", err)
	}
	if !strings.Contains(got, "empty id") {
		t.Errorf("empty-id result = %q, want the retry hint", got)
	}
}

// TestSpawnSubagentExecutionParam pins the execution argument surface:
// schema enum, normalization, validation, and pass-through to the runner.
func TestSpawnSubagentExecutionParam(t *testing.T) {
	// Schema advertises the enum with the three modes.
	var schema map[string]any
	if err := json.Unmarshal(tool.SpawnSubagentTool{}.Parameters(), &schema); err != nil {
		t.Fatalf("schema not JSON: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	exec, ok := props["execution"].(map[string]any)
	if !ok {
		t.Fatal("schema missing the execution property")
	}
	enum, _ := exec["enum"].([]any)
	if len(enum) != 3 {
		t.Fatalf("execution enum = %v, want the three modes", enum)
	}

	var seen string
	runner := tool.SubagentRunner(func(ctx context.Context, request tool.SubagentSpawnRequest, timeoutOverride *time.Duration) (string, error) {
		seen = request.Execution
		return "ok", nil
	})
	toolObj := tool.SpawnSubagentTool{Runner: runner}

	cases := []struct {
		name      string
		execution string
		wantSeen  string
		wantOK    bool
	}{
		{"absent defaults to sync marker", `{"goal":"g","agent_type":"coder"}`, "", true},
		{"explicit sync", `{"goal":"g","agent_type":"coder","execution":"sync"}`, "sync", true},
		{"parallel passes through", `{"goal":"g","agent_type":"coder","execution":"parallel"}`, "parallel", true},
		{"serial passes through", `{"goal":"g","agent_type":"coder","execution":"serial"}`, "serial", true},
		{"typo rejected before the runner", `{"goal":"g","agent_type":"coder","execution":"paralell"}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen = "unset"
			got, err := toolObj.Execute(context.Background(), json.RawMessage(tc.execution))
			if tc.wantOK {
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
				if got != "ok" || seen != tc.wantSeen {
					t.Errorf("runner saw execution %q (result %q), want %q", seen, got, tc.wantSeen)
				}
				return
			}
			if err != nil {
				t.Fatalf("invalid execution should be an error result, not a Go error: %v", err)
			}
			if !strings.Contains(got, "unknown execution") {
				t.Errorf("result = %q, want the retry hint", got)
			}
			if seen != "unset" {
				t.Errorf("runner must not run for an invalid execution, saw %q", seen)
			}
		})
	}

	// Resume requests bypass fresh-spawn validation entirely: even a
	// garbage "execution" argument and no goal still dispatch to the
	// runner, which branches on IsResume.
	seen = "unset"
	if _, err := toolObj.Execute(context.Background(), json.RawMessage(`{"resume":"coder-subagent-1","execution":"bogus"}`)); err != nil {
		t.Fatalf("resume Execute: %v", err)
	}
	if seen == "unset" {
		t.Errorf("resume must dispatch to the runner (with IsResume), not run validation")
	}
}

// TestSchedulerConcurrentCompletionsRace is the -race probe: a burst of
// background children completing at once — notifications appended into the
// shared parent session, manifest writes interleaving — must leave the
// scheduler drained and the history complete.
func TestSchedulerConcurrentCompletionsRace(t *testing.T) {
	const sessionID = "session-20260101-092000"
	sess := runnerTestSession(t, sessionID)
	rec := &statusRecorder{}
	s := newSubagentScheduler(sess, rec.write)

	const n = 8
	start := make(chan struct{})
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("researcher-subagent-%d", i)
		if err := sess.SaveSubagentRecord(session.SubagentRecord{
			ID: id, AgentType: "researcher", Goal: "g",
			Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Minute),
		}); err != nil {
			t.Fatalf("SaveSubagentRecord: %v", err)
		}
		_, _ = s.Launch(id, "researcher", tool.SubagentExecutionParallel, func() subagentCompletion {
			// Fire all completions at once.
			<-start
			recordSubagentResult(sess, sessionID, id, "result of "+id)
			return subagentCompletion{
				ID: id, AgentType: "researcher",
				Status:  session.SubagentStatusCompleted,
				Preview: "preview of " + id,
			}
		})
	}
	close(start) // releases every blocked run closure at once — the race
	eventually(t, 5*time.Second, "all children drained", func() bool {
		for i := 1; i <= n; i++ {
			if s.State(fmt.Sprintf("researcher-subagent-%d", i)) != "" {
				return false
			}
		}
		return true
	})

	// Every completion produced exactly one notification and one file.
	notes := 0
	for _, m := range sess.HistorySnapshot() {
		if m.Role == "user" && strings.HasPrefix(m.Content.String(), "[late harness] subagent researcher-subagent-") {
			notes++
		}
	}
	if notes != n {
		t.Errorf("notifications = %d, want %d", notes, n)
	}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("researcher-subagent-%d", i)
		path, err := session.SubagentResultPath(sessionID, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("result file for %s missing: %v", id, err)
		}
	}
}

// TestSynthesizeFreezesNonTerminalRecords pins the frozen semantics: at
// resume, non-terminal records (running AND queued) flip to frozen —
// persisted — while terminal records stay untouched, and the TUI restore
// picks the frozen ones up.
func TestSynthesizeFreezesNonTerminalRecords(t *testing.T) {
	const sessionID = "session-20260101-093000"
	sess := runnerTestSession(t, sessionID)

	for id, status := range map[string]string{
		"coder-subagent-20":      session.SubagentStatusRunning,
		"coder-subagent-21":      session.SubagentStatusQueued,
		"researcher-subagent-22": session.SubagentStatusCompleted,
	} {
		if err := sess.SaveSubagentRecord(session.SubagentRecord{
			ID: id, AgentType: "coder", Goal: "g", Status: status,
			SpawnedAt: nowMinus(t, time.Minute),
		}); err != nil {
			t.Fatalf("SaveSubagentRecord(%s): %v", id, err)
		}
	}

	if _, err := synthesizeDanglingSpawnResults(sess); err != nil {
		t.Fatalf("synthesizeDanglingSpawnResults: %v", err)
	}

	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	for id, want := range map[string]string{
		"coder-subagent-20":      session.SubagentStatusFrozen,
		"coder-subagent-21":      session.SubagentStatusFrozen,
		"researcher-subagent-22": session.SubagentStatusCompleted,
	} {
		rec, ok := manifest.Get(id)
		if !ok {
			t.Fatalf("record %s vanished", id)
		}
		if rec.Status != want {
			t.Errorf("record %s status = %q, want %q", id, rec.Status, want)
		}
	}

	// The TUI restore accepts the frozen records.
	root := restoredRootForTest(t)
	restored := make(map[string]bool)
	restoreInterruptedSubagents(root, manifest, restored)
	if !restored["coder-subagent-20"] || !restored["coder-subagent-21"] {
		t.Errorf("frozen records not restored: %v", restored)
	}
	if restored["researcher-subagent-22"] {
		t.Error("terminal record must not be restored into the TUI")
	}
}

// restoredRootForTest builds the root orchestrator the TUI restore needs.
func restoredRootForTest(t *testing.T) *orchestrator.BaseOrchestrator {
	t.Helper()
	return orchestrator.NewBaseOrchestrator(common.MainAgentID, runnerTestSession(t, "session-20260101-093000-root"), nil, 0)
}

// TestResumeAcceptsFrozenAndQueuedRecords: the resume validator must treat
// every interrupted status as resumable and keep refusing terminal ones.
func TestResumeAcceptsFrozenAndQueuedRecords(t *testing.T) {
	cases := []struct {
		status   string
		wantErr  bool
		contains string
	}{
		{session.SubagentStatusRunning, false, ""},
		{session.SubagentStatusQueued, false, ""},
		{session.SubagentStatusFrozen, false, ""},
		{session.SubagentStatusCompleted, true, "completed"},
		{session.SubagentStatusFailed, true, "terminated"},
		{session.SubagentStatusCancelled, true, "terminated"},
		{"bogus", true, "unknown manifest status"},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			record := &session.SubagentRecord{
				ID: "coder-subagent-30", AgentType: "coder", Goal: "g",
				Status:      tc.status,
				HistoryPath: "/sessions/s/subagents/coder-subagent-30.json",
			}
			err := validateResumeRecord(record)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("status %q: want error, got nil", tc.status)
				}
				if !strings.Contains(err.Error(), tc.contains) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tc.contains)
				}
				return
			}
			if err != nil {
				t.Errorf("status %q: unexpected error %v", tc.status, err)
			}
		})
	}
}

// --- Per-model parallel gate (Phase 1b) ---

// TestEffectiveSubagentExecutionMode pins the per-model gate as a pure
// function: sync is untouched (the historical blocking path), an explicit
// serial stays serial even on an allowed model, and a requested parallel is
// honored only when the child's model explicitly allows parallel execution —
// otherwise (flag false or ABSENT, or NO agent_models routing at all) it is
// downgraded to serial.
func TestEffectiveSubagentExecutionMode(t *testing.T) {
	cases := []struct {
		name                string
		requested           string
		modelAllowsParallel bool
		want                string
	}{
		{"sync stays sync on an allowed model", tool.SubagentExecutionSync, true, tool.SubagentExecutionSync},
		{"sync stays sync on a gated model", tool.SubagentExecutionSync, false, tool.SubagentExecutionSync},
		{"serial stays serial on an allowed model", tool.SubagentExecutionSerial, true, tool.SubagentExecutionSerial},
		{"serial stays serial on a gated model", tool.SubagentExecutionSerial, false, tool.SubagentExecutionSerial},
		{"parallel honored when the model allows it", tool.SubagentExecutionParallel, true, tool.SubagentExecutionParallel},
		{"parallel downgraded when the model forbids it", tool.SubagentExecutionParallel, false, tool.SubagentExecutionSerial},
		{"parallel downgraded when there is no model routing", tool.SubagentExecutionParallel, false, tool.SubagentExecutionSerial},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveSubagentExecutionMode(tc.requested, tc.modelAllowsParallel); got != tc.want {
				t.Fatalf("effectiveSubagentExecutionMode(%q, %v) = %q, want %q", tc.requested, tc.modelAllowsParallel, got, tc.want)
			}
		})
	}
}

// TestSchedulerModelGateMixedBatch pins the brief's mixed-model scenario end
// to end at the scheduler level, with the gate function wired in exactly as
// the runner wires it: child A (allowed model) and child B (gated model)
// both REQUEST parallel. A launches in parallel immediately; B is downgraded
// to serial, joins the serial queue, and runs ALONE only after A completes.
func TestSchedulerModelGateMixedBatch(t *testing.T) {
	s := newSubagentScheduler(nil, nil)

	// Compute the effective modes through the gate, like the runner does.
	allowed := effectiveSubagentExecutionMode(tool.SubagentExecutionParallel, true)
	gated := effectiveSubagentExecutionMode(tool.SubagentExecutionParallel, false)
	if allowed != tool.SubagentExecutionParallel || gated != tool.SubagentExecutionSerial {
		t.Fatalf("gate produced %q/%q, want parallel/serial", allowed, gated)
	}

	a := newGatedRun()
	b := newGatedRun()

	// A launches immediately (empty field, allowed model).
	if launched, pos := s.Launch("researcher-subagent-40", "researcher", allowed, func() subagentCompletion {
		return a.run(completionFor("researcher-subagent-40", "researcher", session.SubagentStatusCompleted))
	}); !launched || pos != 0 {
		t.Fatalf("allowed parallel launch: launched=%v pos=%d, want true/0", launched, pos)
	}
	eventually(t, time.Second, "A live (parallel)", func() bool {
		return s.State("researcher-subagent-40") == session.SubagentStatusRunning+" (parallel)"
	})

	// B (requested parallel, gated model) queues for SERIAL execution.
	if launched, pos := s.Launch("coder-subagent-41", "coder", gated, func() subagentCompletion {
		return b.run(completionFor("coder-subagent-41", "coder", session.SubagentStatusCompleted))
	}); launched || pos != 1 {
		t.Fatalf("gated parallel launch: launched=%v pos=%d, want false/1", launched, pos)
	}
	if state := s.State("coder-subagent-41"); state != session.SubagentStatusQueued+" (serial, position 1)" {
		t.Fatalf("downgraded child state = %q, want queued serial position 1", state)
	}

	// B must not start while A is live.
	if strings.HasPrefix(s.State("coder-subagent-41"), session.SubagentStatusRunning) {
		t.Fatal("downgraded child launched while the parallel child is still live")
	}

	// A completes → B drains (serial, alone) — after A completes, per the
	// established rules.
	a.letFinish()
	eventually(t, time.Second, "B live (serial) after A finished", func() bool {
		return s.State("researcher-subagent-40") == "" &&
			s.State("coder-subagent-41") == session.SubagentStatusRunning+" (serial)"
	})

	// B's completion leaves the machine empty (no stuck counters).
	b.letFinish()
	eventually(t, time.Second, "machine empty after B", func() bool {
		return s.State("coder-subagent-41") == ""
	})
}

// TestSchedulerModelGateSerialsRunBeforeQueuedParallelBatch pins the other
// half of the mixed-model rules: with a serial child live (here: an
// explicit serial, but a downgraded child behaves identically), downgraded
// children join the serial queue in REQUEST order and drain one-at-a-time
// in FIFO order, the model-allowed parallel children queue as a batch, and
// that batch only launches (together) once the serial chain — explicit and
// downgraded alike — is empty.
func TestSchedulerModelGateSerialsRunBeforeQueuedParallelBatch(t *testing.T) {
	s := newSubagentScheduler(nil, nil)

	allowed := effectiveSubagentExecutionMode(tool.SubagentExecutionParallel, true)
	gated := effectiveSubagentExecutionMode(tool.SubagentExecutionParallel, false)

	serial := newGatedRun() // live explicit serial child
	b1 := newGatedRun()     // first downgraded child
	b2 := newGatedRun()     // second downgraded child (FIFO behind b1)
	p1 := newGatedRun()     // queued allowed parallel
	p2 := newGatedRun()     // queued allowed parallel

	// The explicit serial runs alone; everything else queues.
	if launched, _ := s.Launch("coder-subagent-53", "coder", tool.SubagentExecutionSerial, func() subagentCompletion {
		return serial.run(completionFor("coder-subagent-53", "coder", session.SubagentStatusCompleted))
	}); !launched {
		t.Fatal("serial launch failed")
	}
	eventually(t, time.Second, "serial live", func() bool {
		return s.State("coder-subagent-53") == session.SubagentStatusRunning+" (serial)"
	})

	// Downgraded children join the serial queue in request order...
	for i, g := range []*gatedRun{b1, b2} {
		id := fmt.Sprintf("coder-subagent-54-%d", i+1)
		if launched, pos := s.Launch(id, "coder", gated, func() subagentCompletion {
			return g.run(completionFor(id, "coder", session.SubagentStatusCompleted))
		}); launched || pos != i+1 {
			t.Fatalf("downgraded child %d: launched=%v pos=%d, want false/%d", i+1, launched, pos, i+1)
		}
	}
	// ...and the allowed parallels queue as a batch behind the serials.
	for i, g := range []*gatedRun{p1, p2} {
		id := fmt.Sprintf("researcher-subagent-55-%d", i+1)
		if launched, pos := s.Launch(id, "researcher", allowed, func() subagentCompletion {
			return g.run(completionFor(id, "researcher", session.SubagentStatusCompleted))
		}); launched || pos != i+1 {
			t.Fatalf("queued parallel %d: launched=%v pos=%d, want false/%d", i+1, launched, pos, i+1)
		}
	}

	// The serial finishes → the FIRST downgraded child runs alone (FIFO);
	// the second one and the parallel batch keep waiting.
	serial.letFinish()
	eventually(t, time.Second, "first downgraded child live", func() bool {
		return s.State("coder-subagent-54-1") == session.SubagentStatusRunning+" (serial)"
	})
	if state := s.State("coder-subagent-54-2"); !strings.HasPrefix(state, session.SubagentStatusQueued) {
		t.Fatalf("second downgraded child launched out of order: %q", state)
	}
	if state := s.State("researcher-subagent-55-1"); !strings.HasPrefix(state, session.SubagentStatusQueued) {
		t.Fatalf("parallel launched while serials are queued: %q", state)
	}

	// FIFO: the second downgraded child follows, still alone.
	b1.letFinish()
	eventually(t, time.Second, "second downgraded child live", func() bool {
		return s.State("coder-subagent-54-2") == session.SubagentStatusRunning+" (serial)"
	})
	if state := s.State("researcher-subagent-55-1"); !strings.HasPrefix(state, session.SubagentStatusQueued) {
		t.Fatalf("parallel launched while a serial child is live: %q", state)
	}

	// Serial chain empty → the allowed parallel batch launches together.
	b2.letFinish()
	eventually(t, time.Second, "parallel batch launched", func() bool {
		return s.State("researcher-subagent-55-1") == session.SubagentStatusRunning+" (parallel)" &&
			s.State("researcher-subagent-55-2") == session.SubagentStatusRunning+" (parallel)"
	})
	p1.letFinish()
	p2.letFinish()
}
