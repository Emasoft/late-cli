package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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
	if !s.WasRetrying {
		t.Fatal("WasRetrying not set by RetryEvent")
	}
	if s.Error == nil || s.Error != sentinel {
		t.Fatalf("Error = %v, want the sentinel %v to survive the retry", s.Error, sentinel)
	}
}

// TestThinkingClearsErrorAndToastsRecovery covers recovery: the start of a
// successful turn clears a pinned error box and, when the agent had been
// retrying, fires the "connection restored" toast through the existing
// ToastMsg pipeline (including its 3s expiry).
func TestThinkingClearsErrorAndToastsRecovery(t *testing.T) {
	m, s := newViewportBenchmarkModel(nil)

	s.Error = errors.New("stale failure")
	s.WasRetrying = true
	m.ToastMessage = ""
	m.ToastWarning = true

	updated, cmd := m.Update(OrchestratorEventMsg{Event: common.StatusEvent{ID: m.Focused.ID(), Status: "thinking"}})
	*m = updated.(Model)
	s = m.GetAgentState(m.Focused.ID())

	if s.Error != nil {
		t.Fatalf("Error = %v, want nil once a new turn starts", s.Error)
	}
	if s.WasRetrying {
		t.Fatal("WasRetrying still set after recovery")
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

// TestThinkingWithoutRetryNoToast: a plain new turn (no retry in flight)
// still clears a pinned error box but must not fire the restored toast.
func TestThinkingWithoutRetryNoToast(t *testing.T) {
	m, s := newViewportBenchmarkModel(nil)

	s.Error = errors.New("stale failure")
	s.WasRetrying = false
	m.ToastMessage = ""

	updated, _ := m.Update(OrchestratorEventMsg{Event: common.StatusEvent{ID: m.Focused.ID(), Status: "thinking"}})
	*m = updated.(Model)
	s = m.GetAgentState(m.Focused.ID())

	if s.Error != nil {
		t.Fatalf("Error = %v, want nil once a new turn starts", s.Error)
	}
	if s.WasRetrying {
		t.Fatal("WasRetrying must stay clear")
	}
	if m.ToastMessage != "" {
		t.Fatalf("ToastMessage = %q, want no toast without a retry", m.ToastMessage)
	}
}
