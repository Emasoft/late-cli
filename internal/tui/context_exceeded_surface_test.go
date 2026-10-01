package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"late/internal/client"
)

// contextExceededErr mirrors the real production error chain for terminal
// context exhaustion after the executor guard gave up: the client classified
// the provider rejection as *ContextExceededError (sentinel
// ErrContextExceeded) and the executor wrapped it with the compaction
// outcome via "stream error: %w: <summary>".
func contextExceededErr() error {
	return fmt.Errorf("stream error: %w: %s", &client.ContextExceededError{
		Status: &client.StatusError{
			StatusCode: 400,
			Body:       "This model's maximum context length is 8192 tokens",
		},
		Reason: "http 400",
	}, "context limit hit — auto-compacted (saved ~500 tokens, 2 messages compacted)")
}

// TestContextExceededErrorSurface pins the status surface for a terminal
// context-exhaustion failure: the status line carries the compaction
// outcome, the warning toast repeats the full guidance (like the 413
// toast), and — unlike a 413 — NO TUI-side recovery compaction runs (the
// executor guard already did, or could not do, the work).
func TestContextExceededErrorSurface(t *testing.T) {
	runs := 0
	m := newPayloadRecoveryModel(&runs) // CompactionApplies + counting stub

	next, cmd := dispatchErrorEvent(t, m, m.Focused.ID(), contextExceededErr())
	m = next
	s := m.GetAgentState(m.Focused.ID())

	// The status shows the outcome the executor observed.
	if !strings.Contains(s.StatusText, "saved ~500 tokens") {
		t.Errorf("StatusText = %q, want the compaction outcome", s.StatusText)
	}
	if s.Error == nil {
		t.Fatal("the error must be pinned for the transcript card")
	}

	// Drain the produced commands like Bubble Tea would: among them is the
	// guidance toast (8s warning, same duration as the 413 toast).
	var toastText string
	var foundToast bool
	if cmd != nil {
		var msgs []tea.Msg
		collectMsgs(cmd(), &msgs)
		for _, child := range msgs {
			if toast, ok := child.(ToastMsg); ok && toast.Warning &&
				toast.Duration == 8*time.Second {
				foundToast = true
				toastText = toast.Text
			}
		}
	}
	if !foundToast {
		t.Fatal("no guidance toast produced")
	}
	if !strings.Contains(toastText, "jev-compact-context") {
		t.Errorf("toast = %q, want the recovery guidance", toastText)
	}

	// Unlike a 413, no recovery compaction runs and the TUI-side in-flight
	// guard stays untouched: recovery is the executor guard's job, and it
	// already happened (or could not) before this event surfaced.
	if runs != 0 {
		t.Errorf("compaction runner invoked %d times, want 0", runs)
	}
	if m.CompactionRunning {
		t.Error("the TUI-side in-flight guard must stay untouched")
	}
}

// TestTranscriptErrorRendersContextCard pins the transcript branch: the
// typed sentinel renders the dedicated context-limit card carrying the
// compaction outcome, while a generic error keeps the plain prefix.
func TestTranscriptErrorRendersContextCard(t *testing.T) {
	rendered := transcriptError(contextExceededErr())
	if !strings.Contains(rendered, "**Context Limit Exceeded**") {
		t.Errorf("rendered = %q, want the context-limit card", rendered)
	}
	if !strings.Contains(rendered, "saved ~500 tokens") {
		t.Errorf("rendered = %q, want the compaction outcome", rendered)
	}
	// The legacy text match still stands for pre-typing surfaces.
	legacy := transcriptError(fmt.Errorf("stream error: exceeds the available context size"))
	if !strings.Contains(legacy, "start a new session") {
		t.Errorf("rendered = %q, want the legacy card", legacy)
	}
	plain := transcriptError(fmt.Errorf("something else broke"))
	if strings.Contains(plain, "Context Limit Exceeded") {
		t.Errorf("rendered = %q, want the plain error rendering", plain)
	}
}
