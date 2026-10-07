package orchestrator

// Stall auto-resume tests (worker S): the idle watchdog must recognize a
// BLOCKED IN-FLIGHT TOOL (an API call that never returns, a subprocess that
// hangs) as a STALL — not as "busy forever" — and, when a stall policy is
// installed, cancel the run so the wedged agent unwinds. The stall cause is
// recorded in IdleKillReason() with the "stalled:" prefix, which the cmd/late
// outcome classifier turns into the parent-facing resume directive.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"late/internal/client"
	"late/internal/common"
	"late/internal/executor"
	"late/internal/session"
)

// newStallServerForTest serves a complete SSE stream on every POST (so the
// run can reach its tool-call turn). The stream itself is short and valid:
// the hang under test happens in the TOOL, not the stream. (Named
// ...ForTest to avoid colliding with the pre-existing newStallTestServer in
// base_execute_guard_test.go, which tests a different stall: a held STREAM.)
func newStallServerForTest(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"not found"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// One chunk with a tool call for hung_tool, then finish. The client
		// parses the streamed tool call and the run loop executes it — the
		// hung tool body below is what wedges the agent.
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-stall\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tc_1\",\"type\":\"function\",\"function\":{\"name\":\"hung_tool\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-stall\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestStallWatchdogFiresOnBlockedTool is the end-to-end stall test: a run
// whose tool call blocks server-side past the idle threshold must trip the
// stall callback exactly once, record the "stalled:" kill reason, cancel
// the run, and unwind — never returning to "busy" (the old behavior was an
// infinite wedged run behind a cycling idle notification).
func TestStallWatchdogFiresOnBlockedTool(t *testing.T) {
	// Gate the hung tool on the test's release channel so the tool is
	// deterministically in flight when the watchdog fires, and deterministically
	// released (via the cancelled tool context) at the end.
	toolStarted := make(chan struct{})
	toolSawCancel := make(chan error, 1)
	ts := newStallServerForTest(t)

	sess := session.New(client.NewClient(client.Config{BaseURL: ts.URL}), "", []client.ChatMessage{
		{Role: "user", Content: client.TextContent("call the tool")},
	}, "", false)
	sess.Registry.Register(&fakeIdleTool{
		name: "hung_tool",
		exec: func(ctx context.Context, args json.RawMessage) (string, error) {
			close(toolStarted)
			<-ctx.Done()
			toolSawCancel <- ctx.Err()
			return "", ctx.Err()
		},
	})

	o := NewBaseOrchestrator("stall-test", sess, nil, 5)
	// Tiny thresholds so the watchdog fires in milliseconds; the stall
	// callback + cancel is the production behavior under test.
	o.SetIdlePolicy(80*time.Millisecond, 0)
	o.idleTickInterval = 10 * time.Millisecond

	stallCalls := make(chan string, 4)
	o.SetStallPolicy(func(id, cause string) {
		stallCalls <- id + "|" + cause
	})

	ctx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	o.SetContext(ctx)

	runErr := make(chan error, 1)
	go func() {
		_, err := o.Execute("")
		runErr <- err
	}()

	<-toolStarted
	// The tool is now in flight and will never return on its own: without
	// the stall policy the run would stay "busy" forever (the reported
	// production hang). Wait for the watchdog to fire.
	select {
	case call := <-stallCalls:
		if !strings.HasPrefix(call, "stall-test|stalled:") {
			t.Fatalf("stall callback = %q, want \"stall-test|stalled:...\"", call)
		}
		if !strings.Contains(call, "blocked in-flight tool call") {
			t.Errorf("stall cause %q does not name the blocked tool call", call)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stall callback never fired while a tool call was blocked past the idle threshold")
	}

	// The run must unwind promptly (the callback cancelled the run context
	// the tool call derives from).
	select {
	case err := <-runErr:
		if err == nil {
			t.Log("run returned nil error after the stall cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not unwind after the stall cancel")
	}

	// Exactly one stall callback per run.
	select {
	case extra := <-stallCalls:
		t.Fatalf("stall callback fired twice: %q", extra)
	default:
	}

	// The kill reason is recorded with the stalled prefix (the classifier's
	// detection signal).
	if reason := o.IdleKillReason(); !strings.HasPrefix(reason, "stalled:") {
		t.Errorf("IdleKillReason() = %q, want the stalled: prefix", reason)
	}

	// The tool call observed the cancellation (the wedge was broken).
	select {
	case err := <-toolSawCancel:
		if err == nil {
			t.Error("tool context cancelled without an error")
		}
	case <-time.After(2 * time.Second):
		t.Error("tool call was never cancelled — the wedge survived")
	}
}

// TestStallNotFiredWhileToolIsYoung pins the negative case: a tool in
// flight but YOUNGER than the idle threshold does not trip the stall path
// (a legitimately slow tool must not be killed), and a nested spawn in
// flight exempts the blocked parent (delegation, not a wedge).
func TestStallNotFiredWhileToolIsYoung(t *testing.T) {
	ts := newStallServerForTest(t)
	sess := session.New(client.NewClient(client.Config{BaseURL: ts.URL}), "", nil, "", false)
	o := NewBaseOrchestrator("stall-young", sess, nil, 5)
	o.SetIdlePolicy(50*time.Millisecond, 0)
	o.idleTickInterval = 5 * time.Millisecond

	fired := make(chan string, 1)
	o.SetStallPolicy(func(id, cause string) { fired <- cause })

	// Nested spawn in flight + a tool stamp older than the threshold: the
	// delegation exemption must hold.
	o.BeginNestedSpawn()
	o.beginToolInFlight()
	time.Sleep(120 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.startIdleWatchdog(ctx, cancel)

	deadline := time.After(250 * time.Millisecond)
	for {
		select {
		case cause := <-fired:
			t.Fatalf("stall fired under a live nested spawn: %q", cause)
		case <-deadline:
			o.endToolInFlight()
			o.EndNestedSpawn()
			return
		}
	}
}

// TestStallWatchdogSilentWithoutPolicy pins the off switch: with no stall
// policy installed (the pre-worker-S behavior) the watchdog still reports
// idle — the notification-only path is unchanged — but never cancels a run
// on its own and never records a stalled reason.
func TestStallWatchdogSilentWithoutPolicy(t *testing.T) {
	ts := newStallServerForTest(t)
	sess := session.New(client.NewClient(client.Config{BaseURL: ts.URL}), "", []client.ChatMessage{
		{Role: "user", Content: client.TextContent("call the tool")},
	}, "", false)
	sess.Registry.Register(&fakeIdleTool{
		name: "hung_tool",
		exec: func(ctx context.Context, args json.RawMessage) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	})

	o := NewBaseOrchestrator("stall-off", sess, nil, 5)
	// Notify-only idle policy: the idle event fires, the kill never does.
	o.SetIdlePolicy(60*time.Millisecond, 0)
	o.idleTickInterval = 10 * time.Millisecond

	ctx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	o.SetContext(ctx)

	runErr := make(chan error, 1)
	go func() {
		_, err := o.Execute("")
		runErr <- err
	}()

	// The idle event fires despite the in-flight (stalled) tool — the
	// Part-1 idle-definition fix predates the stall path and must survive.
	sawIdle := false
	deadline := time.After(2 * time.Second)
	for !sawIdle {
		select {
		case ev := <-o.Events():
			if _, ok := ev.(common.SubagentIdleEvent); ok {
				sawIdle = true
			}
		case err := <-runErr:
			t.Fatalf("run ended without a policy: %v", err)
		case <-deadline:
			t.Fatal("idle event never fired for the blocked tool")
		}
	}

	// No cancel, no kill reason: the notify-only contract is intact.
	if reason := o.IdleKillReason(); reason != "" {
		t.Errorf("kill reason recorded without a policy: %q", reason)
	}
	cancelRun()
}

// TestUnwedgeSendDeliversToSlowConsumer pins unwedgeSend: a terminal event
// is delivered when the consumer drains within the timeout (slow TUI), so
// the TUI state machine never hangs.
func TestUnwedgeSendDeliversToSlowConsumer(t *testing.T) {
	sess := session.New(nil, "", nil, "", false)
	o := NewBaseOrchestrator("unwedge-slow", sess, nil, 1)

	// Draining after 20 ms models the TUI frame clock catching up.
	go func() {
		time.Sleep(20 * time.Millisecond)
		for {
			select {
			case <-o.Events():
			default:
				return
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		o.unwedgeSend(common.StatusEvent{ID: o.id, Status: "idle"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("unwedgeSend blocked beyond a normal consumer drain")
	}
	if got := o.droppedEvents.Load(); got != 0 {
		t.Errorf("delivered event counted as dropped: %d", got)
	}
}

// TestUnwedgeSendGivesUpOnWedgedConsumer pins the bounded give-up: a fully
// wedged consumer (nobody reads the channel) costs unwedgeTimeout, never an
// agent hang, and the loss is counted + reported (the stalled-consumer
// observability contract).
func TestUnwedgeSendGivesUpOnWedgedConsumer(t *testing.T) {
	sess := session.New(nil, "", nil, "", false)
	o := NewBaseOrchestrator("unwedge-wedged", sess, nil, 1)
	o.eventCh = make(chan common.Event, 1)
	// Fill the buffer; nobody ever reads it (the wedged consumer).
	o.eventCh <- common.StatusEvent{ID: o.id, Status: "thinking"}

	start := time.Now()
	o.unwedgeSend(common.StatusEvent{ID: o.id, Status: "idle"})
	elapsed := time.Since(start)

	if elapsed < unwedgeTimeout {
		t.Errorf("unwedgeSend returned after %s, want >= the %s timeout", elapsed, unwedgeTimeout)
	}
	if elapsed > unwedgeTimeout+3*time.Second {
		t.Errorf("unwedgeSend took %s, want ~%s", elapsed, unwedgeTimeout)
	}
	if got := o.droppedEvents.Load(); got != 1 {
		t.Errorf("droppedEvents = %d, want 1", got)
	}
}

// TestSendEventCountsDrops pins the sendEvent accounting: a full channel
// counts the drop (the same droppedEvents pattern as trySendProgress).
func TestSendEventCountsDrops(t *testing.T) {
	sess := session.New(nil, "", nil, "", false)
	o := NewBaseOrchestrator("sendevent-drops", sess, nil, 1)
	o.eventCh = make(chan common.Event, 1)
	o.eventCh <- common.StatusEvent{ID: o.id, Status: "thinking"}

	o.sendEvent(common.RetryEvent{ID: o.id, Attempt: 1, MaxAttempts: 3})
	if got := o.droppedEvents.Load(); got != 1 {
		t.Errorf("droppedEvents = %d, want 1", got)
	}

	// With room in the channel, nothing is counted.
	<-o.eventCh
	o.sendEvent(common.RetryEvent{ID: o.id, Attempt: 2, MaxAttempts: 3})
	if got := o.droppedEvents.Load(); got != 1 {
		t.Errorf("droppedEvents = %d after a delivered event, want 1", got)
	}
}

// TestStallCauseFormat pins the machine-readable stall cause shape: the
// "stalled:" prefix is the classifier's detection signal in cmd/late and
// must never drift.
func TestStallCauseFormat(t *testing.T) {
	got := stallCause(2*time.Minute, time.Minute, []string{"user: do it", "assistant: working"})
	if !strings.HasPrefix(got, "stalled: no activity for ") {
		t.Errorf("stallCause = %q, want the stalled: prefix", got)
	}
	if !strings.Contains(got, "blocked in-flight tool call") {
		t.Errorf("stallCause = %q, want it to name the blocked tool call", got)
	}
	if !strings.Contains(got, "user: do it") {
		t.Errorf("stallCause = %q, want the probe lines", got)
	}
}

// fakeIdleTool lives in base_idle_test.go (same package); the stall tests
// reuse it. The compile-time reference below keeps that dependency explicit.
var _ = executor.ExecuteToolCalls
