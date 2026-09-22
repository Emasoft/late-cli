package orchestrator

// Regression test for the permanent orchestrator hang after a terminal error
// with messages queued mid-run.
//
// The bug: run()'s loop tail resets isRunning only when !hasPending — an
// intentional in-loop reset that keeps the flag set between turns of a
// CONTINUING run — but the error classification and the break happen AFTER
// that reset. A terminal error (e.g. the HTTP 400 "read body failed" path)
// therefore exited the loop with isRunning still TRUE and pendingMsgs still
// queued, and nothing after the loop reset the flag. Every later Submit then
// hit the `if o.isRunning` branch, was silently appended to pendingMsgs, and
// was consumed by a run that would never start: the orchestrator hung
// permanently.
//
// The scenario is deterministic — no real network and no reliance on
// scheduling:
//
//  1. When POST #1 arrives at the server, RunLoop's turn-1 onStartTurn has
//     ALREADY consumed pendingMsgs (RunLoop calls it before the stream
//     request). The server signals the test at that moment, so a message
//     submitted afterwards is deterministically QUEUED for the next run,
//     never consumed by the crashing one.
//  2. POST #1 sleeps 300 ms inside the stream request, then answers the
//     bad-body 400 ("read body failed"). The bad-body budget is pinned to
//     one retry (MaxBadBodyRetriesKey=1) so the crashed run uses exactly two
//     POSTs and a single full-jitter backoff of at most 500 ms; the retry
//     POST is answered immediately to bound wall time.
//  3. Submit #2 inside that window → MessageQueuedEvent + one pendingMsg.
//  4. The run terminates with the terminal error StatusEvent. THE FIX under
//     test: run() must clear isRunning after the loop. The test polls the
//     field directly (same package) — this is exactly the pre-fix failure
//     point.
//  5. Submit #3 must start a NEW run: the server answers a valid SSE stream
//     ("recovered"), and the recovery run's onStartTurn consumes the
//     preserved pendingMsg ("queued while running") into history.
//
// Pre-fix observed behavior (post-loop reset temporarily removed): the
// isRunning poll fatal'd — the flag stayed true after the terminal error —
// and Submit #3 never started a run (no thinking event after the error, no
// third POST, no idle terminal event; the message was silently queued into
// the dead run).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"late/internal/client"
	"late/internal/common"
	"late/internal/session"
)

// newHangRegressionServer serves the hang scenario: the first POST to
// */chat/completions signals firstPostArrived, holds the request for 300 ms,
// then answers the transient-looking bad-body 400; the bad-body retry POST
// answers the same 400 immediately (budget pinned to 1 in the test ctx); every
// later POST gets a complete SSE stream carrying "recovered" (the recovery
// run). Discovery probes (GET /props, GET /v1/models) answer 404.
func newHangRegressionServer(t *testing.T, firstPostArrived chan struct{}) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var posts atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch n := posts.Add(1); {
		case n == 1:
			// Turn 1 is now inside the stream request: its onStartTurn has
			// already consumed pendingMsgs, so everything submitted from here
			// on is deterministically queued for the NEXT run.
			firstPostArrived <- struct{}{}
			// Window for the test to submit a message while "running".
			time.Sleep(300 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"The request is invalid: read body failed.","type":"server_error"}}`)
		case n == 2:
			// Bad-body retry: same rejection, answered immediately so the
			// single jittered backoff (≤500 ms) bounds the wall time.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"The request is invalid: read body failed.","type":"server_error"}}`)
		default:
			// Recovery run: a complete stream with only "recovered".
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: "+`{"id":"2","choices":[{"index":0,"delta":{"content":"recovered"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		}
	}))
	t.Cleanup(ts.Close)
	return ts, &posts
}

// newHangRegressionOrchestrator builds a BaseOrchestrator over an in-memory
// session bound to the test server, with a ctx pinning the bad-body retry
// budget to 1 (initial attempt + exactly one retry for the crashed run).
func newHangRegressionOrchestrator(t *testing.T, baseURL string) *BaseOrchestrator {
	t.Helper()
	c := client.NewClient(client.Config{BaseURL: baseURL})
	sess := session.New(c, "", nil, "", false)
	o := NewBaseOrchestrator("test-orch-hang", sess, nil, 5)
	ctx, cancel := context.WithTimeout(
		context.WithValue(context.Background(), common.MaxBadBodyRetriesKey, 1),
		15*time.Second,
	)
	t.Cleanup(cancel)
	o.SetContext(ctx)
	return o
}

// TestTerminalErrorClearsIsRunningAndPreservesQueued drives the full hang
// scenario through the public Submit flow: a terminal 400 kills a run that
// had a message queued mid-run, and the orchestrator must come back to life
// for the next Submit while preserving the queued message.
func TestTerminalErrorClearsIsRunningAndPreservesQueued(t *testing.T) {
	firstPostArrived := make(chan struct{}, 1)
	ts, posts := newHangRegressionServer(t, firstPostArrived)
	o := newHangRegressionOrchestrator(t, ts.URL)

	// Collector: the single reader of o.Events(). Records every event (for
	// post-run assertions) and gates the test's two phases: phase 1 ends at
	// the crashed run's terminal "error" status, phase 2 at the recovery
	// run's terminal "idle" status.
	errTerminal := make(chan common.StatusEvent, 1)
	idleTerminal := make(chan common.StatusEvent, 1)
	stopCollect := make(chan struct{})
	defer close(stopCollect)
	var (
		logMu   sync.Mutex
		logged  []common.Event
		phase1  bool
		collect = func(ev common.Event) {
			logMu.Lock()
			logged = append(logged, ev)
			logMu.Unlock()
			if se, ok := ev.(common.StatusEvent); ok {
				switch {
				case se.Status == "error" && !phase1:
					phase1 = true
					select {
					case errTerminal <- se:
					default:
					}
				case se.Status == "idle" && phase1:
					select {
					case idleTerminal <- se:
					default:
					}
				}
			}
		}
	)
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		for {
			select {
			case <-stopCollect:
				return
			case ev := <-o.Events():
				collect(ev)
			}
		}
	}()

	// --- Phase 1: the crash, with a message queued mid-run ---
	if err := o.Submit("first task", nil); err != nil {
		t.Fatalf("Submit #1 returned error: %v", err)
	}
	// Deterministic queue window: wait until turn 1 is inside the stream
	// request (onStartTurn already ran, so this message cannot be consumed by
	// the crashing run), then submit the message that must be queued.
	select {
	case <-firstPostArrived:
	case <-time.After(10 * time.Second):
		t.Fatal("server never received the first POST; the run did not start")
	}
	if err := o.Submit("queued while running", nil); err != nil {
		t.Fatalf("Submit #2 returned error: %v", err)
	}

	var errEv common.StatusEvent
	select {
	case errEv = <-errTerminal:
	case <-time.After(10 * time.Second):
		t.Fatal("no terminal error StatusEvent within 10s; the 400 run did not terminate")
	}
	if errEv.Error == nil || !strings.Contains(errEv.Error.Error(), "400") || !strings.Contains(errEv.Error.Error(), "read body failed") {
		t.Errorf("terminal error StatusEvent = %v, want the HTTP 400 read-body-failed rejection", errEv.Error)
	}

	// THE FIX under test: after the terminal error the orchestrator must be
	// idle again. Pre-fix, run() exited with isRunning still true (the loop
	// tail only clears it when !hasPending and a message was queued), so this
	// poll fatal'd — and every later Submit was silently queued forever.
	deadline := time.Now().Add(2 * time.Second)
	for {
		o.mu.RLock()
		running := o.isRunning
		o.mu.RUnlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isRunning still true 2s after the terminal error StatusEvent: run() left the orchestrator permanently 'running', so every later Submit would be silently queued (the reported hang)")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// The queued message must survive the crash: pendingMsgs is preserved for
	// the next run's first turn (onStartTurn), never dropped.
	o.mu.RLock()
	pending := append([]client.ChatMessage(nil), o.pendingMsgs...)
	o.mu.RUnlock()
	if len(pending) != 1 || pending[0].Content.String() != "queued while running" {
		t.Fatalf("pendingMsgs after the crash = %v, want exactly the queued %q", pending, "queued while running")
	}

	// --- Phase 2: the recovery — a NEW run must start ---
	if err := o.Submit("after the crash", nil); err != nil {
		t.Fatalf("Submit #3 returned error: %v", err)
	}
	select {
	case <-idleTerminal:
	case <-time.After(10 * time.Second):
		t.Fatal("no terminal idle StatusEvent within 10s after Submit #3: the recovery run never started — Submit #3 was silently queued into the dead run")
	}

	// Server traffic: the crashed run made exactly 2 POSTs (initial + the one
	// pinned bad-body retry); the recovery run added the 3rd. Pre-fix there
	// were only 2 — Submit #3 never reached the API.
	if got := posts.Load(); got != 3 {
		t.Errorf("server received %d POSTs, want 3 (crashed run: initial + bad-body retry; recovery run: 1) — a missing 3rd POST means the recovery run never started", got)
	}

	// New-run evidence: at least one "thinking" StatusEvent must arrive AFTER
	// the terminal error (Submit #3's run start + the recovery turn's
	// onStartTurn). Pre-fix: zero — Submit #3 was silently queued.
	logMu.Lock()
	events := append([]common.Event(nil), logged...)
	logMu.Unlock()
	errIdx, thinkingAfter := -1, 0
	queuedEvent := false
	for i, ev := range events {
		switch e := ev.(type) {
		case common.StatusEvent:
			if e.Status == "error" && errIdx == -1 {
				errIdx = i
				continue
			}
			if e.Status == "thinking" && errIdx != -1 {
				thinkingAfter++
			}
		case common.MessageQueuedEvent:
			if e.Text == "queued while running" {
				queuedEvent = true
			}
		}
	}
	if errIdx == -1 {
		t.Fatal("no error StatusEvent recorded by the collector")
	}
	if !queuedEvent {
		t.Errorf("no MessageQueuedEvent for %q: Submit #2 did not take the isRunning queue branch", "queued while running")
	}
	if thinkingAfter == 0 {
		t.Error("no thinking StatusEvent after the terminal error: Submit #3 did not start a new run (silently queued into the dead run)")
	}

	// History: the recovery run's onStartTurn must have consumed the preserved
	// queued message, and the recovery response must be committed.
	var (
		hasQueued    bool
		hasAfter     bool
		hasRecovered bool
	)
	for _, m := range o.History() {
		content := m.Content.String()
		switch {
		case m.Role == "user" && content == "queued while running":
			hasQueued = true
		case m.Role == "user" && content == "after the crash":
			hasAfter = true
		case m.Role == "assistant" && strings.Contains(content, "recovered"):
			hasRecovered = true
		}
	}
	if !hasQueued {
		t.Errorf("history does not contain the queued %q message: pendingMsgs were not preserved through the crash and consumed by the recovery run's onStartTurn", "queued while running")
	}
	if !hasAfter {
		t.Errorf("history does not contain the %q message: Submit #3 never took the normal (new-run) path", "after the crash")
	}
	if !hasRecovered {
		t.Errorf("history does not contain the assistant recovery response %q: the recovery run never completed", "recovered")
	}
	if got := o.QueuedMessages(); len(got) != 0 {
		t.Errorf("pendingMsgs not fully consumed after the recovery run: %q", got)
	}
}
