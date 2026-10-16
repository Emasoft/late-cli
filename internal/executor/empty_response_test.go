package executor

// Tests for the empty-response retry tier in RunLoop (executor.go): a stream
// that completes with NO content, NO reasoning, and NO tool calls (stalled or
// truncated SSE, silent server close) is re-requested instead of committing an
// empty assistant message and ending the run with an empty result, and fails
// with a typed error once MaxEmptyResponseRounds is exhausted.
//
// Driven end-to-end against an in-process httptest SSE server, mirroring
// stream_retry_integration_test.go.

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

)

// serveDoneOnly writes a well-formed but completely empty SSE stream: only the
// [DONE] sentinel, no chunks at all.
func serveDoneOnly(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte("data: [DONE]\n"))
}

// TestRunLoopRetriesEmptyStreams asserts tier (a): two consecutive empty
// streams followed by a successful one — RunLoop returns the content, fires
// exactly two RetryEvents on the dedicated empty tier, and commits NO empty
// assistant message to history.
func TestRunLoopRetriesEmptyStreams(t *testing.T) {
	var emptyServed atomic.Int64
	rs := newRetryServer(t, func(w http.ResponseWriter, r *http.Request) {
		if emptyServed.Add(1) <= 2 {
			serveDoneOnly(w)
			return
		}
		serveSSE(w)
	})

	sess := newRetryTestSession(t, rs.server.URL)
	onRetry, retryEvents := retryCollector(t)

	ctx, cancel := runLoopCtx(3, 30*time.Second)
	defer cancel()

	res, err := RunLoop(ctx, sess, 1, nil, nil, nil, nil, onRetry, nil, nil)
	if err != nil {
		t.Fatalf("RunLoop returned error after empty-stream retries: %v", err)
	}
	if res != "Hello world" {
		t.Errorf("RunLoop result = %q, want %q", res, "Hello world")
	}

	if got := rs.postCount(); got != 3 {
		t.Errorf("server got %d POSTs, want 3 (two empty streams + one success)", got)
	}

	events := retryEvents()
	if len(events) != 2 {
		t.Fatalf("got %d RetryEvents, want exactly 2: %+v", len(events), events)
	}
	for i, ev := range events {
		if ev.Attempt != i+1 {
			t.Errorf("events[%d].Attempt = %d, want %d", i, ev.Attempt, i+1)
		}
		if ev.MaxAttempts != MaxEmptyResponseRounds {
			t.Errorf("events[%d].MaxAttempts = %d, want %d (dedicated empty tier)", i, ev.MaxAttempts, MaxEmptyResponseRounds)
		}
		if ev.Delay <= 0 {
			t.Errorf("events[%d].Delay = %v, want > 0", i, ev.Delay)
		}
		if ev.Err == nil || !strings.Contains(ev.Err.Error(), "empty response from model") {
			t.Errorf("events[%d].Err = %v, want the empty-response description", i, ev.Err)
		}
	}

	// History keeps only the seeded user message and the successful
	// assistant reply — no empty assistant messages committed for the
	// failed attempts.
	if len(sess.History) != 2 {
		t.Fatalf("history length = %d, want 2 (user + successful assistant); committed: %+v", len(sess.History), sess.History)
	}
	if got := sess.History[len(sess.History)-1].Content.String(); got != "Hello world" {
		t.Errorf("last history content = %q, want %q", got, "Hello world")
	}
	for i, msg := range sess.History {
		if msg.Role == "assistant" && msg.Content.String() == "" {
			t.Errorf("history[%d] is an EMPTY assistant message, which must never be committed", i)
		}
	}
}

// TestRunLoopEmptyStreamsTerminal asserts tier (b): four consecutive empty
// streams with the dedicated tier enabled — RunLoop must fail with the typed
// empty-response error, and history must contain NO empty assistant message.
func TestRunLoopEmptyStreamsTerminal(t *testing.T) {
	rs := newRetryServer(t, func(w http.ResponseWriter, r *http.Request) {
		serveDoneOnly(w)
	})

	sess := newRetryTestSession(t, rs.server.URL)
	onRetry, retryEvents := retryCollector(t)

	ctx, cancel := runLoopCtx(3, 30*time.Second)
	defer cancel()

	_, err := RunLoop(ctx, sess, 5, nil, nil, nil, nil, onRetry, nil, nil)
	if err == nil {
		t.Fatal("RunLoop returned nil error after only-empty streams, want the typed empty-response error")
	}
	if !strings.Contains(err.Error(), "consecutive empty responses") {
		t.Errorf("error = %v, want the typed empty-response description", err)
	}

	// 1 initial attempt + MaxEmptyResponseRounds retries.
	if got := rs.postCount(); got != 1+MaxEmptyResponseRounds {
		t.Errorf("server got %d POSTs, want %d (initial + %d empty-tier retries)", got, 1+MaxEmptyResponseRounds, MaxEmptyResponseRounds)
	}

	events := retryEvents()
	if len(events) != MaxEmptyResponseRounds {
		t.Fatalf("got %d RetryEvents, want %d: %+v", len(events), MaxEmptyResponseRounds, events)
	}

	// Only the seeded user message is committed; no assistant message.
	if len(sess.History) != 1 {
		t.Fatalf("history length = %d, want 1 (seeded user msg only); committed: %+v", len(sess.History), sess.History)
	}
	if sess.History[0].Role != "user" {
		t.Errorf("history[0].Role = %q, want user (an empty assistant message must never be committed)", sess.History[0].Role)
	}
}

// TestRunLoopGlobalDisableSilencesEmptyTier pins the contract: with the global
// stream-retry budget disabled, a single empty stream must fail the turn
// immediately with the typed error instead of re-requesting.
func TestRunLoopGlobalDisableSilencesEmptyTier(t *testing.T) {
	rs := newRetryServer(t, func(w http.ResponseWriter, r *http.Request) {
		serveDoneOnly(w)
	})

	sess := newRetryTestSession(t, rs.server.URL)

	ctx, cancel := runLoopCtx(0, 15*time.Second)
	defer cancel()

	_, err := RunLoop(ctx, sess, 5, nil, nil, nil, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "consecutive empty responses") {
		t.Fatalf("error = %v, want the typed empty-response description with retries disabled", err)
	}
	if got := rs.postCount(); got != 1 {
		t.Errorf("server got %d POSTs, want 1 (disabled budget must not re-request)", got)
	}
}

// TestRunLoopNormalTurnUnaffected asserts tier (c): a normal, non-empty first
// turn behaves byte-identically to today — success, no RetryEvents, one
// committed assistant message.
func TestRunLoopNormalTurnUnaffected(t *testing.T) {
	rs := newRetryServer(t, func(w http.ResponseWriter, r *http.Request) {
		serveSSE(w)
	})

	sess := newRetryTestSession(t, rs.server.URL)
	onRetry, retryEvents := retryCollector(t)

	ctx, cancel := runLoopCtx(3, 15*time.Second)
	defer cancel()

	res, err := RunLoop(ctx, sess, 1, nil, nil, nil, nil, onRetry, nil, nil)
	if err != nil {
		t.Fatalf("RunLoop returned error on a normal turn: %v", err)
	}
	if res != "Hello world" {
		t.Errorf("RunLoop result = %q, want %q", res, "Hello world")
	}

	if got := rs.postCount(); got != 1 {
		t.Errorf("server got %d POSTs, want 1", got)
	}
	if events := retryEvents(); len(events) != 0 {
		t.Errorf("got %d RetryEvents on a clean turn, want 0: %+v", len(events), events)
	}

	if len(sess.History) != 2 {
		t.Fatalf("history length = %d, want 2 (user + assistant)", len(sess.History))
	}
	last := sess.History[len(sess.History)-1]
	if last.Role != "assistant" || last.Content.String() != "Hello world" {
		t.Errorf("last history = (%q, %q), want (assistant, Hello world)", last.Role, last.Content.String())
	}
}
