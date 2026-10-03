package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"late/internal/common"
	"late/internal/config"
)

// statusTextChild mimics a restored subagent
// (agent.RestoredSubagentOrchestrator): a read-only historical child whose
// status line ("restored — was interrupted", with the recorded cause) is
// exposed through the interface method the tab handler seeds the TUI state
// from. The tui package cannot import internal/agent (it would be an import
// cycle: agent wires tui.Messenger), so the shape is stubbed here.
type statusTextChild struct {
	mockOrchestrator
	status string
}

func (s *statusTextChild) ID() string                      { return "coder-subagent-0" }
func (s *statusTextChild) StatusText() string              { return s.status }
func (s *statusTextChild) Children() []common.Orchestrator { return nil }

// rootWithChild is a root orchestrator whose Children list carries one
// child, so the tab handler's cycle reaches it.
type rootWithChild struct {
	mockOrchestrator
	child common.Orchestrator
}

func (r *rootWithChild) Children() []common.Orchestrator {
	return []common.Orchestrator{r.child}
}

// TestTabFocusSeedsRestoredStatusText pins the status surface for a restored
// child: tabbing to it must seed the freshly created agent state with the
// orchestrator's own status line — "restored — was interrupted" with the
// manifest cause — instead of the default "Ready", which hides the one fact
// the user needs when browsing a historical child (its transcript is the
// record of an interrupted run, not a live agent).
func TestTabFocusSeedsRestoredStatusText(t *testing.T) {
	child := &statusTextChild{status: "restored — was interrupted (time budget exhausted)"}
	root := &rootWithChild{child: child}

	m := NewModel(root, nil, &config.Config{})
	m.SetSize(80, 24)
	if got := m.GetAgentState(m.Focused.ID()).StatusText; got != "Ready" {
		t.Fatalf("root status = %q, want the default Ready", got)
	}

	// Tab once: focus moves root → child, seeding the restored status.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	next, ok := updated.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want tui.Model", updated)
	}
	if next.Focused.ID() != child.ID() {
		t.Fatalf("focus = %q, want the restored child %q", next.Focused.ID(), child.ID())
	}
	s := next.GetAgentState(next.Focused.ID())
	if s.StatusText != child.status {
		t.Fatalf("restored child status = %q, want %q", s.StatusText, child.status)
	}
	// Tab twice more (child → root → child): the root keeps its own default
	// status, and the child's seeded status is stable — no "Ready" reset and
	// no double-seeding corruption.
	cur := next
	for i := 0; i < 2; i++ {
		updated, _ := cur.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		cur, ok = updated.(Model)
		if !ok {
			t.Fatalf("Update returned %T, want tui.Model", updated)
		}
	}
	final := cur
	if !ok {
		t.Fatalf("Update returned %T, want tui.Model", updated)
	}
	if final.Focused.ID() != child.ID() {
		t.Fatalf("focus = %q, want the restored child %q again", final.Focused.ID(), child.ID())
	}
	if got := final.GetAgentState(final.Focused.ID()).StatusText; got != child.status {
		t.Fatalf("restored child status after a round trip = %q, want %q", got, child.status)
	}
	if got := final.GetAgentState(root.ID()).StatusText; got != "Ready" {
		t.Fatalf("root status after a round trip = %q, want the untouched default", got)
	}
}

// TestTabFocusKeepsLiveStatusOverSeed pins the seed's guard: a state that
// already carries real status text (a live subagent's "Working...", an
// error line) is never overwritten by the orchestrator's interface value —
// the seed only fills the "Ready" default.
func TestTabFocusKeepsLiveStatusOverSeed(t *testing.T) {
	child := &statusTextChild{status: "restored — was interrupted"}
	root := &rootWithChild{child: child}

	m := NewModel(root, nil, &config.Config{})
	m.SetSize(80, 24)
	m.GetAgentState(child.ID()).StatusText = "Error: stream lost"
	m.GetAgentState(child.ID()).State = StateIdle

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	next, ok := updated.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want tui.Model", updated)
	}
	if got := next.GetAgentState(child.ID()).StatusText; got != "Error: stream lost" {
		t.Fatalf("status = %q, want the pre-existing text to survive the seed", got)
	}
}

// TestNewCommandRefusedDuringCompaction pins the /new guard: with a
// compaction run in flight, /new must be refused — the in-flight walk holds
// the session's history lock and rewrites the history it walked, so a reset
// mid-walk would freeze the TUI for the walk's duration and let the
// finished run's report land on the fresh conversation. The refusal is
// carried by the status line, and Root.Reset is not called.
func TestNewCommandRefusedDuringCompaction(t *testing.T) {
	root := &mockOrchestrator{}
	m := NewModel(root, nil, &config.Config{})
	m.SetSize(80, 24)
	m.CompactionRunning = true

	m.Input.SetValue("/new")
	m = pressEnter(t, m)

	if got := root.resetCount; got != 0 {
		t.Fatalf("Root.Reset called %d time(s) during an in-flight compaction, want 0 refusals → no reset", got)
	}
	if got := m.GetAgentState(m.Focused.ID()).StatusText; !strings.Contains(got, "compaction") {
		t.Fatalf("StatusText = %q, want the visible refusal naming the compaction", got)
	}
	if got := m.GetAgentState(m.Focused.ID()).StatusText; strings.Contains(got, "new conversation started") {
		t.Fatal("the refusal must not be swallowed by the success toast")
	}

	// Once the run ends (compactionResultMsg cleared the guard), /new works.
	m.CompactionRunning = false
	m.Input.SetValue("/new")
	m = pressEnter(t, m)
	if got := root.resetCount; got != 1 {
		t.Fatalf("Root.Reset called %d time(s) after the run ended, want exactly 1", got)
	}
}
