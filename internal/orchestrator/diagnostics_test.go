package orchestrator

import (
	"fmt"
	"io"
	"late/internal/client"
	"late/internal/session"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureTestStderr runs fn while redirecting os.Stderr to a pipe and returns
// everything written during fn. Used to pin both sink-routed (stderr must
// stay clean) and fallback (stderr must receive the line) diagnostics.
func captureTestStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan []byte, 1)
	go func() {
		buf, _ := io.ReadAll(r)
		done <- buf
	}()
	fn()
	_ = w.Close()
	os.Stderr = orig
	return string(<-done)
}

const wantDroppedLine = "late: %d events dropped (consumer stalled)\n"

// TestReportDroppedEvents_SinkReceivesLineStderrClean: with a diagnostics
// sink installed, reportDroppedEvents routes the exact pre-sink line to the
// sink and writes nothing to os.Stderr.
func TestReportDroppedEvents_SinkReceivesLineStderrClean(t *testing.T) {
	o := NewBaseOrchestrator("diag-test", nil, nil, 0)

	var got []string
	o.SetDiagnostics(func(msg string) { got = append(got, msg) })
	defer o.SetDiagnostics(nil)

	o.droppedEvents.Store(44)
	stderr := captureTestStderr(t, func() {
		o.reportDroppedEvents()
	})

	if len(got) != 1 {
		t.Fatalf("sink received %d messages (%q), want 1", len(got), got)
	}
	if want := fmt.Sprintf(wantDroppedLine, 44); got[0] != want {
		t.Fatalf("sink = %q, want %q", got[0], want)
	}
	if stderr != "" {
		t.Fatalf("os.Stderr received %q with a sink installed, want nothing", stderr)
	}
}

// TestReportDroppedEvents_FallbackWritesStderr: without a sink, the exact
// pre-sink line lands on os.Stderr (pins the CLI/test fallback).
func TestReportDroppedEvents_FallbackWritesStderr(t *testing.T) {
	o := NewBaseOrchestrator("diag-test", nil, nil, 0)

	o.droppedEvents.Store(7)
	stderr := captureTestStderr(t, func() {
		o.reportDroppedEvents()
	})

	if want := fmt.Sprintf(wantDroppedLine, 7); stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}

// TestReportfCallableWhileMuHeld pins the deadlock hardening: reportf and
// SetDiagnostics guard diagnosticsFn with a DEDICATED diagMu (the same
// reasoning as the plugin manager's), not the orchestrator-wide mu. reportf
// must therefore stay callable from code that already holds mu (or may hold
// it in the future) — under the old mu.RLock the nested RLock below deadlocked
// against this goroutine's held write lock.
func TestReportfCallableWhileMuHeld(t *testing.T) {
	o := NewBaseOrchestrator("diag-test", nil, nil, 0)

	var got []string
	o.SetDiagnostics(func(msg string) { got = append(got, msg) })
	defer o.SetDiagnostics(nil)

	o.mu.Lock()
	o.reportf("diagnostic while holding mu\n")
	o.mu.Unlock()

	if len(got) != 1 || got[0] != "diagnostic while holding mu\n" {
		t.Fatalf("sink = %q, want the reportf line routed while mu was held", got)
	}
}

// TestExecute_DroppedEventsReportedThroughSink is the end-to-end pin: a
// stalled event consumer (no reader for eventCh) forces progress-event
// drops during a real turn, and the turn-end report goes to the sink as
// "late: N events dropped (consumer stalled)" — never to os.Stderr. This is
// the TUI-corruption scenario (the notice used to paint raw text over the
// alt-screen), wired the way cmd/late/main.go wires it.
func TestExecute_DroppedEventsReportedThroughSink(t *testing.T) {
	// Execute completes a full turn, whose commit persists the .meta.json
	// sidecar into the global sessions dir — redirect it to the temp dir.
	tmpDir := t.TempDir()
	originalSessionDir := session.SessionDir
	session.SessionDir = func() (string, error) { return tmpDir, nil }
	t.Cleanup(func() { session.SessionDir = originalSessionDir })

	const chunkCount = 150
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < chunkCount; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":\"\"}]}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	historyPath := filepath.Join(tmpDir, "session.json")
	initial := []client.ChatMessage{
		{Role: "user", Content: client.TextContent("goal")},
	}
	c := client.NewClient(client.Config{BaseURL: ts.URL})
	sess := session.New(c, historyPath, initial, "", false)
	o := NewBaseOrchestrator("test", sess, nil, 1)

	var got []string
	o.SetDiagnostics(func(msg string) { got = append(got, msg) })
	defer o.SetDiagnostics(nil)

	// NO consumer for o.eventCh — the drops are the point.
	done := make(chan error, 1)
	go func() {
		_, err := o.Execute("")
		done <- err
	}()

	// Wait until the stream has overflowed the buffer (see
	// TestBaseOrchestrator_ExecuteDoesNotDeadlockWhenEventConsumerStalls).
	deadline := time.Now().Add(15 * time.Second)
	for o.droppedEvents.Load() < 50 {
		if time.Now().After(deadline) {
			t.Fatalf("progress events were never dropped (droppedEvents=%d)", o.droppedEvents.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Resume consumption so the blocking terminal sends can be delivered.
	stopDrain := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			select {
			case <-o.eventCh:
			case <-stopDrain:
				return
			}
		}
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Execute did not return after the event consumer resumed (deadlock)")
	}
	close(stopDrain)
	<-drained

	// The turn's end-of-turn report must have gone through the sink — one
	// line per turn, carrying the reported drop count.
	if len(got) != 1 {
		t.Fatalf("sink received %d messages (%q), want exactly the turn-end dropped-events report", len(got), got)
	}
	if !strings.HasPrefix(got[0], "late: ") || !strings.HasSuffix(got[0], " events dropped (consumer stalled)\n") {
		t.Fatalf("sink = %q, want the \"late: N events dropped (consumer stalled)\" line", got[0])
	}
	var reported int64
	if _, err := fmt.Sscanf(got[0], "late: %d events dropped (consumer stalled)\n", &reported); err != nil {
		t.Fatalf("sink line %q does not parse: %v", got[0], err)
	}
	if reported < 50 {
		t.Fatalf("sink reported %d drops, want >= 50 (150 chunks overflow the 100-slot buffer)", reported)
	}
}
