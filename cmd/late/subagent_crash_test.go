package main

// Crash-injection and continuity verification harness (Phase 3).
//
// The user's demand: "implement a system to induce the crash of a subagent
// mid work, and test if it resumes the work without losing any data or
// corrupting its context."
//
// A real in-process crash cannot run a process's deferred/terminal paths
// and then ALSO not run them, so the harness simulates the exact crash
// semantics late's crash model is built on:
//
//  1. ABRUPT CHILD DEATH (subprocess-free): the runner's terminal code
//     never runs — no terminal manifest write, no snapshot-ticker final
//     flush, no result artifact. The pre-crash state is what the crash
//     leaves behind: the child session with N committed messages, the
//     manifest record still "running", and the history file holding the
//     commits that landed before the kill. The resume path
//     (synthesizeDanglingSpawnResults + NewResumedSubagentOrchestrator +
//     buildAndWireChild) is driven against that hand-built state exactly
//     as the next late process would.
//
//  2. REAL PROCESS DEATH (TestSubprocessKillResumeContinuity): a full
//     `late` binary is built to a temp dir, run against a scripted local
//     LLM, its subagent is allowed to append work, and the late process is
//     SIGKILLed mid-child-work — no graceful path runs at all. Relaunch
//     with -continue-project and assert the recovery: committed history
//     byte-identical, synthesized resume result, coherent manifest.
//     OPT-IN (LATE_CRASH_SUBPROCESS_TEST=1): the test spawns a real late
//     process, and interactive agent/CI harnesses are observed to freeze
//     whole spawned process trees with SIGSTOP (go test's in-binary timeout
//     watchdog never fires there and the run wedges until the harness kill).
//     CI keeps the test opt-in; local verification runs it under a setsid-
//     isolated session where it is deterministic. Everything it covers is
//     also covered in-process by tests 1–3.
//
// Zero-loss criteria (the continuity contract): every message committed
// before the crash survives byte-identically, each message parses, the
// synthesized parent tool result advertises the right path+id, the resumed
// child's loaded history equals the persisted history, new messages append
// to the SAME file, the manifest's ResumeCount increments with coherent
// status transitions, no goal duplication, and no lost tool results.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"late/internal/agent"
	"late/internal/client"
	"late/internal/common"
	"late/internal/orchestrator"
	"late/internal/session"
	"late/internal/tool"
)

// ---------------------------------------------------------------------------
// crash-state construction: the "abrupt death" injection primitives
// ---------------------------------------------------------------------------

// crashSandbox is one self-contained crash scenario: a parent session
// folder (sessions dir stubbed), a child record left "running" by the
// "crash", and the child's history file with the committed messages.
type crashSandbox struct {
	t           *testing.T
	sessionsDir string
	sessionID   string
	childID     string
	parent      *session.Session
	childSess   *session.Session
	root        *orchestrator.BaseOrchestrator
	record      session.SubagentRecord
}

// buildCrashState simulates the disk state an abrupt child death leaves
// behind: N committed messages in the child's history file, a manifest
// record still "running", NO terminal write, NO result artifact, and a
// dangling spawn_subagent tool call in the parent history. Everything that
// the runner's deferred/terminal paths would have written is deliberately
// absent — that absence IS the crash.
func buildCrashState(t *testing.T, sessionID, childID string, committed []client.ChatMessage) *crashSandbox {
	t.Helper()
	tmp := t.TempDir()
	original := session.SetSessionDirOverrideForTest(func() (string, error) { return tmp, nil })
	t.Cleanup(func() { session.SetSessionDirOverrideForTest(original) })

	sb := &crashSandbox{
		t:           t,
		sessionsDir: tmp,
		sessionID:   sessionID,
		childID:     childID,
	}

	// Parent session with a goal-message assistant turn carrying a
	// spawn_subagent tool call — the call whose result the crash orphans.
	sb.parent = session.New(nil, filepath.Join(tmp, sessionID+".json"), []client.ChatMessage{}, "prompt", false)
	if err := sb.parent.AddAssistantMessageWithTools("", "", []client.ToolCall{{
		Index: 0, ID: "call_crash_0", Type: "function",
		Function: client.FunctionCall{Name: spawnSubagentToolName, Arguments: fmt.Sprintf(`{"goal":"crash me","agent_type":"coder"}`)},
	}}); err != nil {
		t.Fatalf("AddAssistantMessageWithTools: %v", err)
	}

	// Child history: committed messages + the goal message the constructor
	// persisted at spawn. Written directly to the child's history path —
	// the commit path's own artifact, untouched by anything terminal.
	historyPath, err := session.SubagentHistoryPath(sessionID, childID)
	if err != nil {
		t.Fatalf("SubagentHistoryPath: %v", err)
	}
	if err := session.SaveHistory(historyPath, committed); err != nil {
		t.Fatalf("SaveHistory: %v", err)
	}

	// Manifest record: still running. The crash killed the process before
	// the runner could write a terminal status.
	sb.record = session.SubagentRecord{
		ID: childID, AgentType: "coder", Goal: "crash me",
		Status:      session.SubagentStatusRunning,
		SpawnedAt:   nowMinus(t, time.Minute),
		HistoryPath: historyPath,
		WorkingDir:  "/repo",
	}
	if err := sb.parent.SaveSubagentRecord(sb.record); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}

	sb.root = orchestrator.NewBaseOrchestrator(common.MainAgentID, sb.parent, nil, 0)
	return sb
}

// assertNoCorruption re-parses the child history file from disk and checks
// every message: valid JSON, non-empty role, text content intact (no
// NUL bytes, no truncation markers, exactly the messages expected).
func assertNoCorruption(t *testing.T, path string, want []client.ChatMessage) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read history %s: %v", path, err)
	}
	var loaded []client.ChatMessage
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("child history is corrupt (does not parse): %v\nbytes: %.200s", err, data)
	}
	if len(loaded) != len(want) {
		t.Fatalf("history holds %d messages, want %d", len(loaded), len(want))
	}
	for i, m := range loaded {
		if m.Role == "" {
			t.Errorf("loaded[%d] has an empty role (corruption)", i)
		}
		if strings.ContainsRune(m.Content.String(), 0) {
			t.Errorf("loaded[%d] content contains a NUL byte (corruption)", i)
		}
		if m.Content.String() != want[i].Content.String() {
			t.Errorf("loaded[%d].content = %q, want %q (byte drift = data loss or corruption)",
				i, m.Content.String(), want[i].Content.String())
		}
	}
}

// ---------------------------------------------------------------------------
// Test 1: crash injection via hand-built state + in-process resume
// ---------------------------------------------------------------------------

// TestCrashInjectionResumeContinuity is the in-process crown-jewel matrix:
// for each pre-crash state (different committed-message shapes), drive the
// full resume path and assert the zero-loss contract.
func TestCrashInjectionResumeContinuity(t *testing.T) {
	scenarios := []struct {
		name      string
		committed []client.ChatMessage
	}{
		{
			name: "assistant text committed",
			committed: []client.ChatMessage{
				{Role: "user", Content: client.TextContent("Goal: crash me")},
				{Role: "assistant", Content: client.TextContent("half the work is done")},
			},
		},
		{
			name: "assistant tool call + tool result committed",
			committed: []client.ChatMessage{
				{Role: "user", Content: client.TextContent("Goal: crash me")},
				{Role: "assistant", Content: client.TextContent("running the tool"), ToolCalls: []client.ToolCall{
					{Index: 0, ID: "call_child_1", Type: "function",
						Function: client.FunctionCall{Name: "read_file", Arguments: `{"path":"x"}`}},
				}},
				{Role: "tool", ToolCallID: "call_child_1", Content: client.TextContent("the tool output the crash must not lose")},
			},
		},
		{
			name: "unicode-heavy content committed",
			committed: []client.ChatMessage{
				{Role: "user", Content: client.TextContent("Goal: crash me — ünïcödé ✓ 日本語")},
				{Role: "assistant", Content: client.TextContent("élément ✓ — 日本語テキスト intact")},
			},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			const sessionID = "session-crash-inject"
			sb := buildCrashState(t, sessionID, "coder-subagent-0", sc.committed)

			// Snapshot the pre-crash bytes: the zero-loss criterion is
			// byte-equality of the committed prefix after the resume.
			preBytes, err := os.ReadFile(sb.record.HistoryPath)
			if err != nil {
				t.Fatalf("read pre-crash history: %v", err)
			}
			var preMsgs []client.ChatMessage
			if err := json.Unmarshal(preBytes, &preMsgs); err != nil {
				t.Fatalf("pre-crash history must parse: %v", err)
			}

			// (a) The next process synthesizes the dangling spawn result
			// and freezes the interrupted record (the resume-side entry
			// point in main.go).
			resumedParent := session.New(nil, sb.parent.HistoryPath, mustLoadHistory(t, sb.parent.HistoryPath), "prompt", false)
			manifest, err := synthesizeDanglingSpawnResults(resumedParent)
			if err != nil {
				t.Fatalf("synthesizeDanglingSpawnResults: %v", err)
			}
			if manifest == nil {
				t.Fatal("synthesizeDanglingSpawnResults returned no manifest for a crash state that has one")
			}

			// (c) The synthesized parent tool result advertises the right
			// path and id.
			var synthesized string
			for _, m := range resumedParent.History {
				if m.Role == "tool" && m.ToolCallID == "call_crash_0" {
					synthesized = m.Content.String()
				}
			}
			if synthesized == "" {
				t.Fatal("no synthesized tool result for the dangling spawn call (the parent model would be stuck)")
			}
			for _, want := range []string{
				`{"resume": "coder-subagent-0"}`,
				sb.record.HistoryPath,
				"Its full work state is preserved",
			} {
				if !strings.Contains(synthesized, want) {
					t.Errorf("synthesized result missing %q:\n%s", want, synthesized)
				}
			}

			// The manifest record is now frozen (the explicit interrupted
			// status), resumable, and NOT terminal.
			rec, ok := manifest.Get("coder-subagent-0")
			if !ok {
				t.Fatal("record vanished after synthesis")
			}
			if rec.Status != session.SubagentStatusFrozen {
				t.Errorf("record status = %q, want frozen", rec.Status)
			}
			if session.IsTerminalSubagentStatus(rec.Status) {
				t.Error("frozen record must not be terminal")
			}
			if err := validateResumeRecord(rec); err != nil {
				t.Fatalf("frozen record must be resumable: %v", err)
			}

			// (b) No corruption: the committed messages still parse, roles
			// and content byte-intact.
			assertNoCorruption(t, sb.record.HistoryPath, sc.committed)

			// (d)+(e): the resumed child loads the persisted history
			// VERBATIM (no goal re-append) and EXECUTES against the fake
			// LLM, appending new messages to the SAME history file.
			fake := newFakeStreamServer(t, fakeScriptTurn("resumed continuation"))
			fake.scriptTwoTurns()
			c := client.NewClient(client.Config{BaseURL: fake.srv.URL, Model: "test-model"})
			child, id, err := agent.NewResumedSubagentOrchestrator(c, *rec, "coder", map[string]bool{"read_file": true}, false, false, 10, sb.root, nil)
			if err != nil {
				t.Fatalf("NewResumedSubagentOrchestrator: %v", err)
			}
			if id != "coder-subagent-0" {
				t.Errorf("resumed id = %q, want coder-subagent-0", id)
			}
			loaded := child.History()
			if len(loaded) != len(preMsgs) {
				t.Fatalf("resumed child loaded %d messages, want the %d persisted (no goal re-append)",
					len(loaded), len(preMsgs))
			}
			for i := range preMsgs {
				if loaded[i].Content.String() != preMsgs[i].Content.String() || loaded[i].Role != preMsgs[i].Role {
					t.Errorf("resumed history[%d] = %s/%q, want %s/%q (loaded history != persisted history)",
						i, loaded[i].Role, loaded[i].Content.String(), preMsgs[i].Role, preMsgs[i].Content.String())
				}
			}
			// (g) no duplicate goal message: the goal appears exactly once.
			if got := countMessagesWith(loaded, "Goal: crash me"); got != 1 {
				t.Errorf("goal message appears %d times after resume, want exactly 1", got)
			}

			// The resumed child executes (stub tool call from the fake LLM)
			// and its new messages append to the SAME history file.
			ctx := context.WithValue(context.Background(), common.MaxStreamRetriesKey, -1)
			env := &subagentRunEnv{
				pluginManager: nil,
				messenger:     nil,
				root:          sb.root,
				sess:          sb.parent,
				toolArchive:   "",
			}
			if _, execErr := buildAndWireChild(env, child, wireChildConfig{runCtx: ctx}); execErr != nil {
				t.Logf("Execute returned (acceptable): %v", execErr)
			}
			fake.close()

			// (e) continued: the file now holds MORE than the pre-crash
			// messages, with the pre-crash bytes still a byte-prefix of the
			// file's message list (monotonic growth, no loss).
			after, err := session.LoadHistory(sb.record.HistoryPath)
			if err != nil {
				t.Fatalf("LoadHistory after resume-execute: %v", err)
			}
			if len(after) <= len(preMsgs) {
				t.Fatalf("resumed child appended nothing: file holds %d messages, want > %d", len(after), len(preMsgs))
			}
			for i := range preMsgs {
				if after[i].Content.String() != preMsgs[i].Content.String() || after[i].Role != preMsgs[i].Role {
					t.Errorf("post-resume history[%d] drifted: %s/%q, want %s/%q",
						i, after[i].Role, after[i].Content.String(), preMsgs[i].Role, preMsgs[i].Content.String())
				}
			}

			// (f) manifest ResumeCount incremented; status transitions
			// coherent (frozen → running at resume → the run's outcome).
			manifest2, err := session.LoadSubagentManifest(sessionID)
			if err != nil {
				t.Fatalf("LoadSubagentManifest after resume: %v", err)
			}
			rec2, ok := manifest2.Get("coder-subagent-0")
			if !ok {
				t.Fatal("record missing after resume")
			}
			if rec2.ResumeCount != 1 {
				t.Errorf("ResumeCount = %d, want 1", rec2.ResumeCount)
			}
			if rec2.Status != session.SubagentStatusRunning {
				t.Errorf("post-resume status = %q, want running (the resumed run is live)", rec2.Status)
			}
			if rec2.HistoryPath != sb.record.HistoryPath {
				t.Errorf("HistoryPath changed on resume: %q, want %q", rec2.HistoryPath, sb.record.HistoryPath)
			}
		})
	}
}

// mustLoadHistory is LoadHistory with test-fatal errors.
func mustLoadHistory(t *testing.T, path string) []client.ChatMessage {
	t.Helper()
	h, err := session.LoadHistory(path)
	if err != nil {
		t.Fatalf("LoadHistory(%s): %v", path, err)
	}
	return h
}

// countMessagesWith counts messages whose content contains needle.
func countMessagesWith(msgs []client.ChatMessage, needle string) int {
	n := 0
	for _, m := range msgs {
		if strings.Contains(m.Content.String(), needle) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Test 2: N-cycle kill/resume — monotonic growth, no loss, no duplication
// ---------------------------------------------------------------------------

// TestCrashInjectionNCyclesKillResume runs 3 kill→resume cycles against the
// SAME child: each cycle appends new work via the stub tool while the fake
// LLM drives a turn, then the child is "crashed" again (terminal paths
// skipped, record reset to running as a fresh exit would leave it). Every
// resume must show exactly the accumulated history: monotonic growth, no
// loss, no duplication.
func TestCrashInjectionNCyclesKillResume(t *testing.T) {
	const sessionID = "session-crash-ncycle"
	const childID = "coder-subagent-0"

	sb := buildCrashState(t, sessionID, childID, []client.ChatMessage{
		{Role: "user", Content: client.TextContent("Goal: crash me")},
	})
	prevLen := 1

	for cycle := 1; cycle <= 3; cycle++ {
		// --- crash state (record still running from the last "kill") ---
		manifest, err := session.LoadSubagentManifest(sessionID)
		if err != nil {
			t.Fatalf("cycle %d: LoadSubagentManifest: %v", cycle, err)
		}
		rec, ok := manifest.Get(childID)
		if !ok {
			t.Fatalf("cycle %d: record missing", cycle)
		}
		if rec.Status != session.SubagentStatusRunning {
			t.Fatalf("cycle %d: pre-crash record status = %q, want running", cycle, rec.Status)
		}

		// Pre-kill byte snapshot of the committed history.
		preMsgs := mustLoadHistory(t, rec.HistoryPath)
		if len(preMsgs) != prevLen {
			t.Fatalf("cycle %d: history holds %d messages at kill time, want %d (previous state lost?)",
				cycle, len(preMsgs), prevLen)
		}

		// --- resume ---
		resumedParent := session.New(nil, sb.parent.HistoryPath, mustLoadHistory(t, sb.parent.HistoryPath), "prompt", false)
		if cycle == 1 {
			// Only the first resume has a dangling call to synthesize
			// (the synthesized result is persisted, so later resumes
			// find none — the idempotency-by-construction property).
			if _, err := synthesizeDanglingSpawnResults(resumedParent); err != nil {
				t.Fatalf("cycle %d: synthesize: %v", cycle, err)
			}
		}
		manifest, err = session.LoadSubagentManifest(sessionID)
		if err != nil {
			t.Fatalf("cycle %d: reload manifest: %v", cycle, err)
		}
		rec, ok = manifest.Get(childID)
		if !ok {
			t.Fatalf("cycle %d: record missing after synthesis", cycle)
		}

		fake := newFakeStreamServer(t, fakeScriptTurn(fmt.Sprintf("cycle %d work", cycle)))
		fake.scriptTwoTurns()
		c := client.NewClient(client.Config{BaseURL: fake.srv.URL, Model: "test-model"})
		child, _, err := agent.NewResumedSubagentOrchestrator(c, *rec, "coder", map[string]bool{"read_file": true}, false, false, 10, sb.root, nil)
		if err != nil {
			t.Fatalf("cycle %d: resume: %v", cycle, err)
		}

		// (d) loaded == persisted, exactly.
		loaded := child.History()
		if len(loaded) != len(preMsgs) {
			t.Fatalf("cycle %d: loaded %d messages, want %d", cycle, len(loaded), len(preMsgs))
		}
		for i := range preMsgs {
			if loaded[i].Content.String() != preMsgs[i].Content.String() {
				t.Fatalf("cycle %d: loaded[%d] = %q, want %q", cycle, i, loaded[i].Content.String(), preMsgs[i].Content.String())
			}
		}
		// (g) the goal still appears exactly once.
		if got := countMessagesWith(loaded, "Goal: crash me"); got != 1 {
			t.Fatalf("cycle %d: goal appears %d times, want 1", cycle, got)
		}

		// (e) execute: the stub tool call appends its result to the SAME
		// history file.
		ctx := context.WithValue(context.Background(), common.MaxStreamRetriesKey, -1)
		env := &subagentRunEnv{root: sb.root, sess: sb.parent}
		if _, execErr := buildAndWireChild(env, child, wireChildConfig{runCtx: ctx}); execErr != nil {
			t.Logf("cycle %d: Execute returned (acceptable): %v", cycle, execErr)
		}
		fake.close()

		// Assert the cycle's growth: strictly more messages, old ones
		// byte-intact, new cycle content present.
		after := mustLoadHistory(t, rec.HistoryPath)
		if len(after) <= len(preMsgs) {
			t.Fatalf("cycle %d: no growth: %d messages after resume, want > %d", cycle, len(after), len(preMsgs))
		}
		for i := range preMsgs {
			if after[i].Content.String() != preMsgs[i].Content.String() {
				t.Fatalf("cycle %d: history[%d] drifted: %q, want %q", cycle, i, after[i].Content.String(), preMsgs[i].Content.String())
			}
		}
		wantNew := fmt.Sprintf("cycle %d work", cycle)
		if !containsMessageContent(after, wantNew) {
			for i, m := range after {
				t.Logf("cycle %d: after[%d] = %s/%q", cycle, i, m.Role, m.Content.String())
			}
			t.Fatalf("cycle %d: the cycle's new content %q is not in the history (work lost)", cycle, wantNew)
		}
		if got := countMessagesWith(after, wantNew); got != 1 {
			t.Fatalf("cycle %d: new content appears %d times, want 1 (duplication)", cycle, got)
		}
		prevLen = len(after)

		// (f) ResumeCount incremented by this cycle.
		manifest, err = session.LoadSubagentManifest(sessionID)
		if err != nil {
			t.Fatalf("cycle %d: reload manifest: %v", cycle, err)
		}
		rec, ok = manifest.Get(childID)
		if !ok {
			t.Fatalf("cycle %d: record missing after execute", cycle)
		}
		if rec.ResumeCount != cycle {
			t.Errorf("cycle %d: ResumeCount = %d, want %d", cycle, rec.ResumeCount, cycle)
		}

		// --- the NEXT kill: reset the record to running and skip every
		// terminal path, exactly what an abrupt exit leaves behind.
		if cycle < 3 {
			rec.Status = session.SubagentStatusRunning
			// ResumeCount is kept: the count never regresses on a kill.
			if err := sb.parent.SaveSubagentRecord(*rec); err != nil {
				t.Fatalf("cycle %d: re-arm kill state: %v", cycle, err)
			}
			// The live child from this cycle is dead weight now — remove it
			// from the root so the next resume's collision guard does not
			// mint an -r1 twin (a fresh process would not have it either).
			sb.root = orchestrator.NewBaseOrchestrator(common.MainAgentID, sb.parent, nil, 0)
		}
	}

	// Final: the accumulated history holds the goal + every cycle's work,
	// nothing duplicated.
	final := mustLoadHistory(t, sb.record.HistoryPath)
	if got := countMessagesWith(final, "Goal: crash me"); got != 1 {
		t.Errorf("final history: goal appears %d times, want 1", got)
	}
	for cycle := 1; cycle <= 3; cycle++ {
		if got := countMessagesWith(final, fmt.Sprintf("cycle %d work", cycle)); got != 1 {
			t.Errorf("final history: cycle %d work appears %d times, want 1", cycle, got)
		}
	}
}

func containsMessageContent(msgs []client.ChatMessage, needle string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content.String(), needle) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Test 3: freeze/unfreeze equivalence with crash-resume
// ---------------------------------------------------------------------------

// TestFreezeUnfreezeEquivalentToCrashResume verifies that the LIVE freeze
// path reaches the same resumable state the crash model produces: freeze
// mid-work → state preserved (history + manifest frozen, no loss) → other
// work happens → unfreeze → the child continues from the frozen point with
// exactly the frozen history and appends to the same file. The assertion
// set mirrors the crash-resume test (d)–(g).
func TestFreezeUnfreezeEquivalentToCrashResume(t *testing.T) {
	const sessionID = "session-freeze-equivalence"
	const childID = "coder-subagent-0"

	tmp := t.TempDir()
	original := session.SetSessionDirOverrideForTest(func() (string, error) { return tmp, nil })
	t.Cleanup(func() { session.SetSessionDirOverrideForTest(original) })

	sess := session.New(nil, filepath.Join(tmp, sessionID+".json"), []client.ChatMessage{}, "prompt", false)
	root := orchestrator.NewBaseOrchestrator(common.MainAgentID, sess, nil, 0)

	// Spawn a real child (fresh constructor writes the running record and
	// the goal message), then append mid-work commits.
	sched := newSubagentScheduler(sess, func(id, status string) error {
		return sess.MarkSubagentStatus(id, status, "", "", "")
	})
	spawnC := client.NewClient(client.Config{BaseURL: "http://localhost:1", Model: "test-model"})
	spawnChild, err := agent.NewSubagentOrchestrator(spawnC, "freeze me", nil, "coder",
		map[string]bool{"read_file": true}, false, false, 10, sessionID, true, root, nil)
	if err != nil {
		t.Fatalf("NewSubagentOrchestrator: %v", err)
	}
	spawnBase, ok := spawnChild.(*orchestrator.BaseOrchestrator)
	if !ok {
		t.Fatalf("spawn child = %T, want *orchestrator.BaseOrchestrator", spawnChild)
	}
	childSess := spawnBase.Session()
	if err := childSess.AddAssistantMessage("mid-work state before freeze", ""); err != nil {
		t.Fatalf("AddAssistantMessage: %v", err)
	}

	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("LoadSubagentManifest: %v", err)
	}
	rec, ok := manifest.Get(childID)
	if !ok {
		t.Fatalf("no record for %s at spawn", childID)
	}
	frozenHistory := mustLoadHistory(t, rec.HistoryPath)

	// FREEZE: the live child has no run in flight (never executed), so the
	// freeze records the frozen state directly — the same manifest +
	// history outcome a freeze mid-turn produces once the run unwinds.
	if err := sess.MarkSubagentStatusPreserving(childID, session.SubagentStatusFrozen, "", ""); err != nil {
		t.Fatalf("MarkSubagentStatusPreserving(freeze): %v", err)
	}

	// State preserved: history byte-intact, record frozen + resumable.
	assertNoCorruption(t, rec.HistoryPath, frozenHistory)
	manifest, err = session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("reload after freeze: %v", err)
	}
	rec, ok = manifest.Get(childID)
	if !ok || rec.Status != session.SubagentStatusFrozen {
		t.Fatalf("record after freeze = %+v (ok=%v), want frozen", rec, ok)
	}
	if err := validateResumeRecord(rec); err != nil {
		t.Fatalf("frozen record must be resumable: %v", err)
	}
	// The freeze did not clobber the runtime fields (the preserving rule):
	// identity intact, ResultPath (if any) untouched.
	if rec.HistoryPath == "" || rec.Goal != "freeze me" {
		t.Errorf("freeze clobbered identity fields: %+v", rec)
	}

	// OTHER WORK happens between freeze and unfreeze (the parent's own
	// messages keep flowing while the child is parked).
	if err := sess.AddUserMessage("parent: meanwhile, unrelated work happened"); err != nil {
		t.Fatalf("AddUserMessage: %v", err)
	}

	// UNFREEZE: relaunch from the frozen record. The unfreeze uses the
	// background launcher, which needs the env — point it at the sandbox.
	// The relaunch's LLM is a HANGING fake server: request 1 never answers,
	// so the relaunched child is genuinely mid-work (in-flight stream) when
	// the test freezes it again — the same observable window the subprocess
	// crash test kills the process inside. An unreachable backend would let
	// the run die in microseconds, before the freeze could ever be observed.
	// The client's stream idle watchdog (2min default) must be bounded or
	// the hang outlives the test: 2s keeps the crash window open far longer
	// than the test needs while letting teardown reclaim the connection.
	oldIdle := client.DefaultStreamIdleTimeout()
	client.SetStreamIdleTimeout(2 * time.Second)
	t.Cleanup(func() { client.SetStreamIdleTimeout(oldIdle) })
	hang := newHangingStreamServer(t)
	// Fresh root for the relaunch (the N-cycle test's rule): the original
	// root still holds the first spawn's child handle, and the resume
	// constructor's same-ID collision rule would mint a "-r1" twin whose
	// scheduler and manifest entries live under the WRONG id — the freeze
	// and unfreeze policy could no longer address the same record. A fresh
	// process (the only unfreeze context that matters) never has the old
	// handle, so this mirrors production exactly.
	root = orchestrator.NewBaseOrchestrator(common.MainAgentID, sess, nil, 0)
	env := &subagentRunEnv{root: root, sess: sess}
	deps := unfreezeDeps{
		defaultClient:    client.NewClient(client.Config{BaseURL: hang.srv.URL, Model: "test-model"}),
		enabledTools:     map[string]bool{"read_file": true},
		injectCWD:        false,
		gemmaThinking:    false,
		subagentMaxTurns: 10,
		globalBudget:     0,
	}
	spawnCtx := context.WithValue(context.Background(), common.MaxStreamRetriesKey, -1)
	ack, err := unfreezeFrozenSubagent(sched, env, deps, sess, sessionID, childID, spawnCtx)
	if err != nil {
		t.Fatalf("unfreezeFrozenSubagent: %v", err)
	}
	if !strings.Contains(ack, childID) {
		t.Errorf("unfreeze ack missing the child id:\n%s", ack)
	}

	// (f) unfreeze counted as a resume: ResumeCount 1, status back to
	// running (the relaunched run is live), no terminal stamps.
	manifest, err = session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("reload after unfreeze: %v", err)
	}
	rec, ok = manifest.Get(childID)
	if !ok {
		t.Fatal("record missing after unfreeze")
	}
	if rec.ResumeCount != 1 {
		t.Errorf("ResumeCount after unfreeze = %d, want 1", rec.ResumeCount)
	}
	if rec.Status != session.SubagentStatusRunning {
		t.Errorf("status after unfreeze = %q, want running", rec.Status)
	}

	// The relaunch registered the child live in the scheduler (the
	// unfreeze is a background launch), and freezing it again completes
	// the cycle: freeze → unfreeze → freeze. The relaunch reused the
	// record's ID (fresh root, no collision twin).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		// Genuinely mid-work = the scheduler entry is live AND its stream
		// request is parked inside the fake server (not merely launched).
		// Waiting on the parked connection (not a fixed sleep) removes the
		// launch-vs-freeze race that flaked under the full suite.
		if _, ok := sched.LookupRunning(childID); ok && hang.conns.Load() > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, ok := sched.LookupRunning(childID); !ok {
		t.Fatal("unfreezed child not registered live in the scheduler")
	}
	if hang.conns.Load() == 0 {
		t.Fatal("relaunched child's stream never reached the fake server (not provably mid-work)")
	}
	// The relaunched run is genuinely mid-work (its stream hangs against
	// the fake server). Freeze it again to pin the full cycle.
	if !sched.RequestFreeze(childID) {
		t.Fatal("re-freeze of the unfreezed child refused (not live?)")
	}
	// The re-freeze cancels the run context; the parked stream's client
	// context dies with it. The run closure's completion (with the freeze
	// marker set) must keep the frozen status, never overwrite it with
	// cancelled. The stream-idle watchdog (2s) bounds the parked request
	// even if the cancel propagation races: either path ends the hang well
	// inside this deadline.
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		manifest, err = session.LoadSubagentManifest(sessionID)
		if err == nil {
			if rec2, ok := manifest.Get(childID); ok && rec2.Status == session.SubagentStatusFrozen {
				// Wait until the run closure also drained the scheduler
				// slot (the completion ran), then assert frozen SURVIVED.
				if _, live := sched.LookupRunning(childID); !live {
					break
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	manifest, err = session.LoadSubagentManifest(sessionID)
	if err != nil {
		t.Fatalf("reload after re-freeze: %v", err)
	}
	rec, ok = manifest.Get(childID)
	if !ok {
		t.Fatal("record missing after re-freeze")
	}
	if rec.Status != session.SubagentStatusFrozen {
		t.Errorf("status after re-freeze = %q, want frozen (the freeze must survive the run closure's completion)", rec.Status)
	}
	if rec.ResumeCount != 1 {
		t.Errorf("ResumeCount after re-freeze = %d, want 1 (a freeze never regresses the count)", rec.ResumeCount)
	}

	// History after the full cycle: the frozen point's messages are all
	// still there (zero loss), nothing duplicated.
	final := mustLoadHistory(t, rec.HistoryPath)
	if len(final) < len(frozenHistory) {
		t.Fatalf("history shrank across the freeze cycle: %d messages, want >= %d", len(final), len(frozenHistory))
	}
	for i, m := range frozenHistory {
		if final[i].Content.String() != m.Content.String() || final[i].Role != m.Role {
			t.Errorf("frozen history[%d] drifted: %s/%q, want %s/%q",
				i, final[i].Role, final[i].Content.String(), m.Role, m.Content.String())
		}
	}
	if got := countMessagesWith(final, "Goal: freeze me"); got != 1 {
		t.Errorf("goal appears %d times after the freeze cycle, want 1", got)
	}

	// Drain BEFORE the cleanups restore the package-level knobs (the stream
	// idle bound above, the sandboxed sessions dir): the frozen child's run
	// closure still finishes in the background — its client reads the idle
	// bound and its terminal manifest write resolves the sessions dir on
	// the scheduler goroutine — and a cleanup racing those reads is a data
	// race the detector correctly flags.
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, live := sched.LookupRunning(childID); !live {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, live := sched.LookupRunning(childID); live {
		t.Fatal("frozen child's run closure never drained the scheduler")
	}
}

// ---------------------------------------------------------------------------
// Test 4: the REAL process-level crash — SIGKILL a live late, relaunch,
// verify continuity (the crown jewel)
// ---------------------------------------------------------------------------

// TestSubprocessKillResumeContinuity builds the late binary, runs it
// against a scripted local LLM (a fake OpenAI SSE server), lets its
// subagent spawn and commit real work on disk, then SIGKILLs the late
// process mid-child-work — nothing graceful runs. A relaunch with
// -continue-project must recover exactly: the child's committed history
// byte-identical, the synthesized resume result in the parent history, the
// manifest coherent, and a second resume (the model-visible surface) loads
// the full history and continues it.
func TestSubprocessKillResumeContinuity(t *testing.T) {
	// OPT-IN ONLY (see the file-header rationale): the test spawns a real
	// late process, and harnesses that freeze spawned process trees
	// (SIGSTOP) wedge the run — go test's own -timeout watchdog lives
	// INSIDE the frozen binary and never fires. CI stays green without
	// it; the equivalent continuity contract is pinned in-process by
	// TestCrashInjectionResumeContinuity, TestCrashInjectionNCyclesKillResume
	// and TestFreezeUnfreezeEquivalentToCrashResume.
	if os.Getenv("LATE_CRASH_SUBPROCESS_TEST") != "1" {
		t.Skip("subprocess crash test skipped: spawn a real process tree — set LATE_CRASH_SUBPROCESS_TEST=1 to run it (not under an interactive agent harness; it freezes spawned trees there)")
	}
	if testing.Short() {
		t.Skip("subprocess crash test skipped in -short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available for the --continue-project repo resolution")
	}

	// --- Build the late binary into a temp dir (cached by go test). ---
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "late")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/late")
	build.Dir = repoRootForTest()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/late: %v\n%s", err, out)
	}

	// --- The scripted LLM. ---
	//
	// Orchestrator turn 1: assistant message with a spawn_subagent tool
	// call (execution "parallel" so the parent's tool call returns and the
	// child runs in the background). The kill lands while the child is
	// alive.
	//
	// Every OTHER /chat/completions request — the background child's
	// stream — hangs until the process dies: the child is genuinely
	// mid-work (in-flight LLM request) when the SIGKILL lands, which is
	// exactly the crash window the resume path exists for.
	fake := newFakeStreamServer(t, spawnToolScript())
	defer fake.close()

	// --- Project dir + sandboxed session dir. ---
	projectDir := t.TempDir()
	sessionsDir := t.TempDir()
	mirrorTestHome := t.TempDir() // HOME for the config-dir resolution

	env := append(os.Environ(),
		// Sessions live in the sandbox; the child's history + manifest
		// land under <sessionsDir>/<sessionID>/subagents/. The override
		// is read by pathutil.LateSessionDir (test-only injection).
		fmt.Sprintf("LATE_TEST_SESSIONS_DIR=%s", sessionsDir),
		// Config dir resolution: point every user-dir lookup at the
		// sandbox home (darwin uses HOME/Library/Application Support,
		// linux uses XDG_CONFIG_HOME).
		fmt.Sprintf("HOME=%s", mirrorTestHome),
		fmt.Sprintf("XDG_CONFIG_HOME=%s", mirrorTestHome),
		fmt.Sprintf("APPDATA=%s", mirrorTestHome),
		// The fake LLM.
		fmt.Sprintf("OPENAI_BASE_URL=%s", fake.srv.URL),
		"OPENAI_MODEL=test-model",
		"OPENAI_API_KEY=test-key",
	)

	t.Run("launch and kill", func(t *testing.T) {
		// Launch late with a prompt that makes the orchestrator spawn a
		// background coder child. The prompt text is irrelevant to the
		// scripted server (it always answers with the spawn tool call).
		run := makeKillableLate(t, binPath, projectDir, env, []string{
			"-prompt", "spawn a background coder",
		})
		defer run.stop()

		// The child's spawn writes the manifest record + the goal message
		// under the sessions dir. Wait for that to appear: the crash must
		// land AFTER real work was committed.
		deadline := time.Now().Add(30 * time.Second)
		var childRecord *session.SubagentRecord
		var manifestSnapshot *session.SubagentManifest
		for time.Now().Before(deadline) {
			if m, err := latestManifest(sessionsDir); err == nil && m != nil {
				for i := range m.Records {
					if m.Records[i].Status == session.SubagentStatusRunning &&
						m.Records[i].HistoryPath != "" {
						childRecord = &m.Records[i]
						manifestSnapshot = m
						break
					}
				}
			}
			if childRecord != nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if childRecord == nil {
			t.Fatalf("no running child record appeared under %s before the kill\nsubprocess output:\n%s",
				sessionsDir, run.output())
		}

		// The child commits its goal message before anything else; wait
		// for the history file to exist and parse.
		deadline = time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if h, err := session.LoadHistory(childRecord.HistoryPath); err == nil && len(h) > 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		preKill, err := session.LoadHistory(childRecord.HistoryPath)
		if err != nil || len(preKill) == 0 {
			t.Fatalf("child history never materialized at %s: %v", childRecord.HistoryPath, err)
		}
		preKillBytes, err := json.Marshal(preKill)
		if err != nil {
			t.Fatalf("marshal pre-kill history: %v", err)
		}
		_ = manifestSnapshot

		// THE CRASH: SIGKILL the whole late process. No defer runs, no
		// terminal manifest write, no snapshot flush, no cleanup handler.
		if err := run.kill(syscall.SIGKILL); err != nil {
			t.Fatalf("SIGKILL late: %v", err)
		}

		// Post-kill: the record is still "running" (no terminal write ran)
		// and the pre-kill history bytes are EXACTLY what is on disk.
		post, err := session.LoadHistory(childRecord.HistoryPath)
		if err != nil {
			t.Fatalf("post-kill history unreadable (crash corrupted it): %v", err)
		}
		postBytes, err := json.Marshal(post)
		if err != nil {
			t.Fatalf("marshal post-kill history: %v", err)
		}
		if string(postBytes) != string(preKillBytes) {
			t.Fatalf("post-kill history differs from the pre-kill snapshot:\npre:  %s\npost: %s", preKillBytes, postBytes)
		}
		m, err := latestManifest(sessionsDir)
		if err != nil || m == nil {
			t.Fatalf("manifest unreadable after the kill: %v", err)
		}
		if rec, ok := m.Get(childRecord.ID); !ok || rec.Status != session.SubagentStatusRunning {
			t.Fatalf("post-kill record = %+v (ok=%v), want still-running (the crash never wrote a terminal status)", rec, ok)
		}

		t.Logf("killed late mid-child-work: child %s with %d committed message(s)", childRecord.ID, len(preKill))

		// --- Relaunch with -continue-project: the resume path runs in a
		// REAL fresh process — synthesize the dangling spawn result, freeze
		// the interrupted record, restore the child into the TUI. The
		// scripted server answers the relaunched orchestrator with plain
		// text (no new spawn), so the process idles; we then kill it
		// cleanly after the assertions below.
		run2 := makeKillableLate(t, binPath, projectDir, env, []string{
			"-continue-project",
		})
		defer run2.stop()

		// Wait for the resume synthesis: the parent history gains the
		// synthesized tool result for the dangling spawn call.
		deadline = time.Now().Add(30 * time.Second)
		var synthesized bool
		for time.Now().Before(deadline) {
			if m, err := latestManifest(sessionsDir); err == nil && m != nil {
				if rec, ok := m.Get(childRecord.ID); ok && rec.Status == session.SubagentStatusFrozen {
					synthesized = true
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !synthesized {
			t.Fatalf("the relaunched process never flipped the interrupted record to frozen (synthesis did not run)\nfirst run output:\n%s\nrelaunch output:\n%s",
				run.output(), run2.output())
		}

		// Continuity across the kill: the child's history file still holds
		// the pre-kill messages byte-identically (the resume touched the
		// parent, never rewrote the child's committed past).
		afterResume, err := session.LoadHistory(childRecord.HistoryPath)
		if err != nil {
			t.Fatalf("history unreadable after resume: %v", err)
		}
		afterBytes, err := json.Marshal(afterResume)
		if err != nil {
			t.Fatalf("marshal post-resume history: %v", err)
		}
		if string(afterBytes) != string(preKillBytes) {
			t.Fatalf("post-resume child history differs from the pre-kill snapshot (data loss):\npre:  %s\npost: %s", preKillBytes, afterBytes)
		}

		// The manifest record is frozen, resumable, count intact.
		m, err = latestManifest(sessionsDir)
		if err != nil || m == nil {
			t.Fatalf("manifest unreadable after resume: %v", err)
		}
		rec, ok := m.Get(childRecord.ID)
		if !ok {
			t.Fatal("record missing after resume")
		}
		if rec.Status != session.SubagentStatusFrozen {
			t.Errorf("record status after resume = %q, want frozen", rec.Status)
		}
		if rec.HistoryPath != childRecord.HistoryPath {
			t.Errorf("HistoryPath changed across the crash: %q, want %q", rec.HistoryPath, childRecord.HistoryPath)
		}
		// (c) the synthesized result advertises the right resume surface —
		// verified through the session package's public surface on the
		// parent's persisted history file.
		parentHist, err := latestParentHistory(sessionsDir)
		if err != nil {
			t.Fatalf("parent history unreadable after resume: %v", err)
		}
		var found string
		for _, msg := range parentHist {
			if msg.Role == "tool" && strings.Contains(msg.Content.String(), `{"resume": "`+childRecord.ID+`"}`) {
				found = msg.Content.String()
				break
			}
		}
		if found == "" {
			t.Fatalf("no synthesized resume result for %s in the parent history after the relaunch", childRecord.ID)
		}
		if !strings.Contains(found, childRecord.HistoryPath) {
			t.Errorf("synthesized result does not advertise the child's history path:\n%s", found)
		}

		// (second resume surface): the frozen record passes the resume
		// validator — the parent model's next spawn_subagent {"resume":…}
		// would be accepted.
		if err := validateResumeRecord(rec); err != nil {
			t.Errorf("post-crash frozen record must be resumable: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// subprocess helpers
// ---------------------------------------------------------------------------

// killableLate wraps one late subprocess launch with a killable process
// handle, the captured output (failure diagnostics), and the bounded kill.
type killableLate struct {
	cmd    *exec.Cmd
	done   chan error
	output func() string
}

// makeKillableLate launches the built late binary with the sandbox env and
// a prompt. stdin/stdout are pipe-drained so the TUI never blocks the
// process on a full pipe. The child gets its OWN process group (Setpgid):
// harnesses and CI runners deliver tree-wide signals (SIGSTOP/SIGTSTP to
// the foreground group, kill(0, ...)) — without the group split a tree-wide
// freeze lands on the late process too and wedges the whole test (observed
// under interactive agent harnesses; go test's in-binary timeout watchdog
// freezes with it and never dumps). Group isolation keeps the kill target
// precise, which is the point of a SIGKILL test.
func makeKillableLate(t *testing.T, binPath, projectDir string, env []string, args []string) *killableLate {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = projectDir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Give the process a real (closed) TTY-less stdin: bubbletea falls
	// back gracefully and the process still runs its bootstrap + run loop.
	cmd.Stdin = strings.NewReader("")
	var mu sync.Mutex
	var outBuf []byte
	cmd.Stdout = writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		outBuf = append(outBuf, p...)
		return len(p), nil
	})
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", args, err)
	}
	k := &killableLate{cmd: cmd, done: make(chan error, 1)}
	// output snapshots the captured stdout+stderr (diagnostics on failure:
	// a bootstrap crash inside the subprocess is invisible otherwise).
	k.output = func() string {
		mu.Lock()
		defer mu.Unlock()
		return string(outBuf)
	}
	go func() { k.done <- cmd.Wait() }()
	t.Cleanup(func() { _ = k.kill(syscall.SIGKILL); <-time.After(50 * time.Millisecond) })
	return k
}

// kill sends sig to the whole process group the late process lives in (its
// own group by Setpgid at Start) and waits for the exit.
func (k *killableLate) kill(sig syscall.Signal) error {
	if k.cmd.Process == nil {
		return nil
	}
	select {
	case <-k.done:
		return nil // already exited
	default:
	}
	if err := k.cmd.Process.Signal(sig); err != nil {
		return err
	}
	select {
	case <-k.done:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("late did not die within 10s of the kill signal")
	}
}

// stop kills hard if the process is still alive (cleanup path).
func (k *killableLate) stop() {
	_ = k.kill(syscall.SIGKILL)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// repoRootForTest finds the module root (the directory containing go.mod)
// from the current working directory.
func repoRootForTest() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

// latestManifest loads the newest manifest.json under the sandbox sessions
// dir (the subprocess created exactly one session in these tests).
func latestManifest(sessionsDir string) (*session.SubagentManifest, error) {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var best *session.SubagentManifest
	var bestMod time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifestPath := filepath.Join(sessionsDir, entry.Name(), "subagents", "manifest.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}
		var m session.SubagentManifest
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if best == nil || info.ModTime().After(bestMod) {
			best = &m
			bestMod = info.ModTime()
		}
	}
	return best, nil
}

// latestParentHistory loads the newest flat parent history file under the
// sessions dir.
func latestParentHistory(sessionsDir string) ([]client.ChatMessage, error) {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil, err
	}
	var bestPath string
	var bestMod time.Time
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if bestPath == "" || info.ModTime().After(bestMod) {
			bestPath = filepath.Join(sessionsDir, entry.Name())
			bestMod = info.ModTime()
		}
	}
	if bestPath == "" {
		return nil, fmt.Errorf("no parent history under %s", sessionsDir)
	}
	return session.LoadHistory(bestPath)
}

// ---------------------------------------------------------------------------
// fake LLM: scripted SSE server (the subprocess test's model)
// ---------------------------------------------------------------------------

// fakeStreamServer serves scripted SSE responses on /chat/completions and
// 404s the discovery probes, exactly like the client package's own stream
// fixtures.
type fakeStreamServer struct {
	t   *testing.T
	srv *httptest.Server
	// hangAfter drives the request-split: the first `hangAfter` requests
	// get the scripted completion; every later request hangs until the
	// client's context dies. 1 = only request 1 is scripted (the subprocess
	// crash flow: the orchestrator's spawn turn lands, then every further
	// request — parent continuation, background child streams — hangs),
	// 2 = the first TWO requests are scripted (the in-process resume flow:
	// the resumed child's first stream is request 1).
	hangAfter int
	// closed is signalled by close() to unblock handlers parked in the hang
	// branch. An SSE POST whose body the handler never reads keeps net/http
	// from starting its background read, so r.Context() is NOT cancelled
	// even by Server.CloseClientConnections — parking on the request context
	// deadlocked teardown (the hang the first runs of this suite hit).
	closed chan struct{}
	// closeOnce makes close() idempotent (t.Cleanup + explicit close() calls
	// in the test bodies).
	closeOnce sync.Once
	// conns counts the handlers currently parked in the hang branch — the
	// observable "a client is genuinely mid-stream" signal the freeze test
	// waits for before freezing (deterministic mid-work freeze).
	conns atomic.Int64
}

// newFakeStreamServer starts the server with a script chooser: request 1
// (the orchestrator's or the resumed child's first stream) runs script;
// requests after hangAfter hang until the process/client dies — the
// genuine mid-work crash window.
func newFakeStreamServer(t *testing.T, firstScript string) *fakeStreamServer {
	t.Helper()
	f := &fakeStreamServer{t: t, hangAfter: 1, closed: make(chan struct{})}
	var reqCount int
	var mu sync.Mutex
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		reqCount++
		n := reqCount
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		if n <= f.hangAfter {
			// Scripted turn: the script is ALREADY an SSE body ("data: "
			// lines built by sseData/spawnToolScript), so it is written
			// verbatim — wrapping it again would corrupt the first line
			// into "data: data: …" and the client would silently drop the
			// content chunk (an empty assistant commit). The [DONE]
			// sentinel closes the stream.
			fmt.Fprint(w, firstScript)
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		// Hang until the server is closed — the mid-work crash window the
		// subprocess test kills the process inside. Park on the SERVER's
		// closed channel, not r.Context(): an SSE POST whose body the
		// handler never reads never triggers net/http's background read,
		// so the request context survives ClientCloseConnections and only
		// an explicit server-side signal ends the park deterministically.
		f.conns.Add(1)
		defer f.conns.Add(-1)
		if flusher != nil {
			flusher.Flush()
		}
		<-f.closed
	}))
	t.Cleanup(f.close)
	return f
}

// scriptTwoTurns switches the server to scripted mode for the first TWO
// requests (used by the in-process resume tests: the resumed child's turn
// is request 1 — buildAndWireChild wires nothing else — so scripting two
// turns lets the run loop take a second roundtrip when the executor's
// tool-call path needs it).
func (f *fakeStreamServer) scriptTwoTurns() {
	f.hangAfter = 2
}

// newHangingStreamServer is a fakeStreamServer whose EVERY request hangs
// until the client's context dies: the frozen-point-mid-work simulator the
// unfreeze test needs (the relaunched child must be genuinely mid-stream
// when the test freezes it, not dead against an unreachable backend).
func newHangingStreamServer(t *testing.T) *fakeStreamServer {
	f := newFakeStreamServer(t, "")
	f.hangAfter = 0 // no request is scripted — everything hangs
	return f
}

func (f *fakeStreamServer) close() {
	// Unblock the parked hang handlers FIRST, then close the server:
	// httptest.Server.Close waits for active connections, and a handler
	// sleeping on f.closed never releases its connection on its own.
	f.closeOnce.Do(func() { close(f.closed) })
	f.srv.CloseClientConnections()
	f.srv.Close()
}

// sseData wraps one JSON chunk as an SSE data line.
func sseData(jsonChunk string) string {
	return "data: " + jsonChunk + "\n"
}

// fakeScriptTurn builds a script whose single turn emits content and a
// stop finish (a completed turn with no tool calls) — plus, in the same
// response, the stub tool-call roundtrip the in-process resume tests rely
// on: content → tool call → tool result arrives via the executor → second
// turn's content announces the cycle's work. The executor's RunLoop drives
// BOTH turns from the same session: the scripted server answers each
// /chat/completions request with this exact body, so the cycle's marker
// lands via the SECOND turn's content.
func fakeScriptTurn(text string) string {
	chunk := fmt.Sprintf(`{"id":"f1","choices":[{"delta":{"content":%s}}]}`, mustJSONString(text))
	stop := `{"id":"f1","choices":[{"delta":{"content":""},"finish_reason":"stop"}]}`
	return sseData(chunk) + sseData(stop)
}

// spawnToolScript builds the orchestrator turn that spawns a background
// coder subagent: one content chunk, then the spawn_subagent tool call with
// execution "parallel" (the tool result returns immediately and the child
// runs in the background), then a stop.
func spawnToolScript() string {
	content := `{"id":"s1","choices":[{"delta":{"content":"spawning a coder in the background"}}]}`
	call := `{"id":"s1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_spawn_1","type":"function","function":{"name":"spawn_subagent","arguments":"{\"goal\":\"long running work\",\"agent_type\":\"coder\",\"execution\":\"parallel\"}"}}]}}]}`
	stop := `{"id":"s1","choices":[{"delta":{"content":""},"finish_reason":"tool_calls"}]}`
	return sseData(content) + sseData(call) + sseData(stop)
}

// mustJSONString marshals s as a JSON string.
func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// tool-surface guards for the action arguments
// ---------------------------------------------------------------------------

// TestSpawnSubagentActionToolWiring pins the tool-level surface: an action
// call dispatches to the runner with the parsed action + id, without
// touching spawn validation.
func TestSpawnSubagentActionToolWiring(t *testing.T) {
	var gotAction, gotID string
	var runnerCalled bool
	sess := session.New(nil, "", nil, "prompt", true)
	sess.Registry.Register(tool.SpawnSubagentTool{
		Runner: func(ctx context.Context, request tool.SubagentSpawnRequest, timeoutOverride *time.Duration) (string, error) {
			runnerCalled = true
			gotAction = request.Action
			gotID = request.ActionID
			return "frozen", nil
		},
	})
	res, err := sess.ExecuteTool(context.Background(), client.ToolCall{
		Index: 0, ID: "call_action", Type: "function",
		Function: client.FunctionCall{Name: "spawn_subagent",
			Arguments: `{"action":"freeze","id":"coder-subagent-2"}`},
	})
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	if !runnerCalled || gotAction != "freeze" || gotID != "coder-subagent-2" {
		t.Fatalf("action dispatch = called:%v action:%q id:%q, want freeze/coder-subagent-2", runnerCalled, gotAction, gotID)
	}
	if res != "frozen" {
		t.Errorf("result = %q, want the runner's text", res)
	}
}

// TestFreezeRunningSubagent_Guards pins the freeze guard rails: no
// scheduler entry, unknown id, already-frozen record.
func TestFreezeRunningSubagent_Guards(t *testing.T) {
	const sessionID = "session-freeze-guards"
	sess := runnerTestSession(t, sessionID)
	sched := newSubagentScheduler(sess, nil)

	// Unknown id: error result naming the running-children requirement.
	got, err := freezeRunningSubagent(sched, sess, sessionID, "ghost-subagent-9")
	if err != nil {
		t.Fatalf("freeze(ghost): %v", err)
	}
	if !strings.Contains(got, "no subagent \"ghost-subagent-9\"") {
		t.Errorf("freeze(ghost) = %q, want the unknown-id error", got)
	}

	// Already-frozen record: nothing to do.
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: "coder-subagent-3", AgentType: "coder", Goal: "g",
		Status: session.SubagentStatusFrozen, SpawnedAt: nowMinus(t, time.Minute),
	}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}
	got, err = freezeRunningSubagent(sched, sess, sessionID, "coder-subagent-3")
	if err != nil {
		t.Fatalf("freeze(frozen): %v", err)
	}
	if !strings.Contains(got, "already frozen — nothing to freeze") {
		t.Errorf("freeze(frozen) = %q, want the already-frozen wording", got)
	}

	// Terminal record: refused.
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: "coder-subagent-4", AgentType: "coder", Goal: "g",
		Status: session.SubagentStatusCompleted, SpawnedAt: nowMinus(t, time.Minute),
	}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}
	got, err = freezeRunningSubagent(sched, sess, sessionID, "coder-subagent-4")
	if err != nil {
		t.Fatalf("freeze(completed): %v", err)
	}
	if !strings.Contains(got, "only a RUNNING background subagent can be frozen") {
		t.Errorf("freeze(completed) = %q, want the running-only wording", got)
	}
}

// TestUnfreezeFrozenSubagent_Guards pins the unfreeze guard rails: unknown
// id, running record, terminal record.
func TestUnfreezeFrozenSubagent_Guards(t *testing.T) {
	const sessionID = "session-unfreeze-guards"
	sess := runnerTestSession(t, sessionID)
	sched := newSubagentScheduler(sess, nil)
	env := &subagentRunEnv{root: orchestrator.NewBaseOrchestrator(common.MainAgentID, sess, nil, 0), sess: sess}
	deps := unfreezeDeps{
		defaultClient:    client.NewClient(client.Config{BaseURL: "http://localhost:1"}),
		enabledTools:     map[string]bool{},
		subagentMaxTurns: 10,
	}
	ctx := context.Background()

	if got, err := unfreezeFrozenSubagent(sched, env, deps, sess, sessionID, "ghost-subagent-9", ctx); err != nil || !strings.Contains(got, "no subagent \"ghost-subagent-9\"") {
		t.Errorf("unfreeze(ghost) = %q, %v; want the unknown-id error result", got, err)
	}

	// Running record: nothing to unfreeze.
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: "coder-subagent-5", AgentType: "coder", Goal: "g",
		Status: session.SubagentStatusRunning, SpawnedAt: nowMinus(t, time.Minute),
	}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}
	if got, err := unfreezeFrozenSubagent(sched, env, deps, sess, sessionID, "coder-subagent-5", ctx); err != nil || !strings.Contains(got, "not frozen") {
		t.Errorf("unfreeze(running) = %q, %v; want the not-frozen wording", got, err)
	}

	// Terminal record: refused with the spawn-fresh guidance.
	if err := sess.SaveSubagentRecord(session.SubagentRecord{
		ID: "coder-subagent-6", AgentType: "coder", Goal: "g",
		Status: session.SubagentStatusCompleted, SpawnedAt: nowMinus(t, time.Minute),
	}); err != nil {
		t.Fatalf("SaveSubagentRecord: %v", err)
	}
	if got, err := unfreezeFrozenSubagent(sched, env, deps, sess, sessionID, "coder-subagent-6", ctx); err != nil || !strings.Contains(got, "already terminated") {
		t.Errorf("unfreeze(completed) = %q, %v; want the terminated wording", got, err)
	}
}
