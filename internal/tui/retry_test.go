package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"late/internal/client"
	"late/internal/common"
)

// TestRetryEventKeepsAgentThinking covers the RetryEvent dispatch: the status
// bar announces the retry, the agent stays thinking (spinner keeps running),
// the failed attempt's partial output and render caches are dropped, and a
// pinned error is left untouched until a successful turn clears it.
func TestRetryEventKeepsAgentThinking(t *testing.T) {
	m, s := newViewportBenchmarkModel(nil)

	sentinel := errors.New("previous failure")
	s.Error = sentinel
	s.State = StateStreaming
	s.StreamingState = common.ContentEvent{ID: m.Focused.ID(), Content: "partial attempt output"}
	s.StreamingStyledCache = "styled cache"
	s.StreamingChunkCount = 7

	updated, _ := m.Update(OrchestratorEventMsg{Event: common.RetryEvent{
		ID:          m.Focused.ID(),
		Attempt:     2,
		MaxAttempts: 5,
		Delay:       1500 * time.Millisecond,
		Err:         errors.New("connection reset by peer"),
	}})
	*m = updated.(Model)
	s = m.GetAgentState(m.Focused.ID())

	if !strings.Contains(s.StatusText, "retrying") {
		t.Fatalf("StatusText = %q, want it to mention retrying", s.StatusText)
	}
	if !strings.Contains(s.StatusText, "attempt 2/5") {
		t.Fatalf("StatusText = %q, want it to contain attempt 2/5", s.StatusText)
	}
	if !strings.Contains(s.StatusText, "1.5s") {
		t.Fatalf("StatusText = %q, want the delay truncated to 1.5s", s.StatusText)
	}
	if s.State != StateThinking {
		t.Fatalf("State = %v, want StateThinking", s.State)
	}
	if s.StreamingStyledCache != "" || s.StreamingChunkCount != 0 {
		t.Fatalf("streaming render cache not cleared (cache=%q, chunks=%d)", s.StreamingStyledCache, s.StreamingChunkCount)
	}
	if s.StreamingState.Content != "" {
		t.Fatalf("failed attempt's partial text survived: %q", s.StreamingState.Content)
	}
	if s.RetryVerb != retryVerbConnectionLost {
		t.Fatalf("RetryVerb = %q, want %q after an infra failure", s.RetryVerb, retryVerbConnectionLost)
	}
	if s.Error == nil || s.Error != sentinel {
		t.Fatalf("Error = %v, want the sentinel %v to survive the retry", s.Error, sentinel)
	}
}

// TestRetryEventHTTP400NamesTheRejection covers failure-class honesty: when the
// underlying stream error is an HTTP 400 from the API (the request body was
// rejected, not the connection lost), the status bar says so instead of
// claiming the connection dropped.
func TestRetryEventHTTP400NamesTheRejection(t *testing.T) {
	m, _ := newViewportBenchmarkModel(nil)

	updated, _ := m.Update(OrchestratorEventMsg{Event: common.RetryEvent{
		ID:          m.Focused.ID(),
		Attempt:     1,
		MaxAttempts: 3,
		Delay:       750 * time.Millisecond,
		Err:         fmt.Errorf("stream error: %w", &client.StatusError{StatusCode: 400, Body: "read body failed"}),
	}})
	*m = updated.(Model)
	s := m.GetAgentState(m.Focused.ID())

	if !strings.Contains(s.StatusText, "request rejected by the API") {
		t.Fatalf("StatusText = %q, want it to name the API rejection", s.StatusText)
	}
	if strings.Contains(s.StatusText, "connection lost") {
		t.Fatalf("StatusText = %q, must not claim a lost connection for an HTTP 400", s.StatusText)
	}
	if !strings.Contains(s.StatusText, "attempt 1/3") {
		t.Fatalf("StatusText = %q, want it to contain attempt 1/3", s.StatusText)
	}
}

// TestThinkingClearsErrorAndToastsRecovery covers recovery: the start of a
// successful turn clears a pinned error box and, when the agent had been
// retrying, fires the "connection restored" toast through the existing
// ToastMsg pipeline (including its 3s expiry).
func TestThinkingClearsErrorAndToastsRecovery(t *testing.T) {
	m, s := newViewportBenchmarkModel(nil)

	s.Error = errors.New("stale failure")
	s.RetryVerb = retryVerbConnectionLost
	m.ToastMessage = ""
	m.ToastWarning = true

	updated, cmd := m.Update(OrchestratorEventMsg{Event: common.StatusEvent{ID: m.Focused.ID(), Status: "thinking"}})
	*m = updated.(Model)
	s = m.GetAgentState(m.Focused.ID())

	if s.Error != nil {
		t.Fatalf("Error = %v, want nil once a new turn starts", s.Error)
	}
	if s.RetryVerb != "" {
		t.Fatalf("RetryVerb = %q, want it cleared after recovery", s.RetryVerb)
	}
	if cmd == nil {
		t.Fatal("expected a command delivering the restored toast")
	}

	// Run the returned command and feed every produced message back through
	// Update, exactly as Bubble Tea would. The frame tick message is skipped:
	// it only coalesces presentation.
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, child := range batch {
			childMsg := child()
			if _, isFrame := childMsg.(transcriptFrameMsg); isFrame {
				continue
			}
			updated, _ := m.Update(childMsg)
			*m = updated.(Model)
		}
	} else {
		updated, _ := m.Update(msg)
		*m = updated.(Model)
	}

	if m.ToastMessage != "connection restored" {
		t.Fatalf("ToastMessage = %q, want %q", m.ToastMessage, "connection restored")
	}
	if m.ToastWarning {
		t.Fatal("restored toast must be success-style, not warning")
	}
	if m.ToastExpireTime <= time.Now().UnixMilli() {
		t.Fatalf("ToastExpireTime = %d, want a future expiry (~3s)", m.ToastExpireTime)
	}
}

// TestRecoveryToastMatchesFailureClass covers the 400-class recovery: when the
// retried failure was an HTTP 400 from the API (the request body was rejected,
// not the connection lost), the recovery toast must announce the request was
// accepted after the retry instead of claiming the connection was restored.
func TestRecoveryToastMatchesFailureClass(t *testing.T) {
	m, _ := newViewportBenchmarkModel(nil)

	updated, _ := m.Update(OrchestratorEventMsg{Event: common.RetryEvent{
		ID:          m.Focused.ID(),
		Attempt:     1,
		MaxAttempts: 3,
		Delay:       750 * time.Millisecond,
		Err:         fmt.Errorf("stream error: %w", &client.StatusError{StatusCode: 400, Body: "read body failed"}),
	}})
	*m = updated.(Model)
	s := m.GetAgentState(m.Focused.ID())

	if s.RetryVerb != retryVerbRejectedByAPI {
		t.Fatalf("RetryVerb = %q, want %q after an HTTP 400", s.RetryVerb, retryVerbRejectedByAPI)
	}

	m.ToastMessage = ""
	updated, cmd := m.Update(OrchestratorEventMsg{Event: common.StatusEvent{ID: m.Focused.ID(), Status: "thinking"}})
	*m = updated.(Model)
	s = m.GetAgentState(m.Focused.ID())

	if cmd == nil {
		t.Fatal("expected a command delivering the recovered toast")
	}
	if s.RetryVerb != "" {
		t.Fatalf("RetryVerb = %q, want it cleared after recovery", s.RetryVerb)
	}

	// Run the returned command and feed every produced message back through
	// Update, exactly as Bubble Tea would. The frame tick message is skipped:
	// it only coalesces presentation.
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, child := range batch {
			childMsg := child()
			if _, isFrame := childMsg.(transcriptFrameMsg); isFrame {
				continue
			}
			updated, _ := m.Update(childMsg)
			*m = updated.(Model)
		}
	} else {
		updated, _ := m.Update(msg)
		*m = updated.(Model)
	}

	if !strings.Contains(m.ToastMessage, "request accepted after retry") {
		t.Fatalf("ToastMessage = %q, want it to mention the accepted retry", m.ToastMessage)
	}
	if strings.Contains(m.ToastMessage, "connection restored") {
		t.Fatalf("ToastMessage = %q, must not claim the connection was restored for an HTTP 400", m.ToastMessage)
	}
}

// TestThinkingWithoutRetryNoToast: a plain new turn (no retry in flight)
// still clears a pinned error box but must not fire the restored toast.
func TestThinkingWithoutRetryNoToast(t *testing.T) {
	m, s := newViewportBenchmarkModel(nil)

	s.Error = errors.New("stale failure")
	s.RetryVerb = ""
	m.ToastMessage = ""

	updated, _ := m.Update(OrchestratorEventMsg{Event: common.StatusEvent{ID: m.Focused.ID(), Status: "thinking"}})
	*m = updated.(Model)
	s = m.GetAgentState(m.Focused.ID())

	if s.Error != nil {
		t.Fatalf("Error = %v, want nil once a new turn starts", s.Error)
	}
	if s.RetryVerb != "" {
		t.Fatal("RetryVerb must stay clear")
	}
	if m.ToastMessage != "" {
		t.Fatalf("ToastMessage = %q, want no toast without a retry", m.ToastMessage)
	}
}
