package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"late/internal/client"
	"late/internal/common"
	"late/internal/config"
)

// expandedDefaultHistory is a compact conversation covering every visibility
// surface under test: user prompt, assistant reasoning, assistant prose, a
// tool call and its full result.
func expandedDefaultHistory() []client.ChatMessage {
	return []client.ChatMessage{
		{Role: "user", Content: client.TextContent("EXPANDEDPROMPT")},
		{Role: "assistant", ReasoningContent: "EXPANDEDREASONING about the request", Content: client.TextContent("Calling a tool.")},
		{
			Role:    "assistant",
			Content: client.TextContent("Running it now."),
			ToolCalls: []client.ToolCall{{
				ID:       "call-1",
				Type:     "function",
				Function: client.FunctionCall{Name: "bash", Arguments: `{"command":"echo hi"}`},
			}},
		},
		{Role: "tool", ToolCallID: "call-1", Content: client.TextContent("SOMETOOLLINE1\nSOMETOOLLINE2")},
	}
}

func newExpandedDefaultModel(t *testing.T) (*Model, *AppState) {
	t.Helper()
	root := &focusTestOrchestrator{id: common.MainAgentID, history: expandedDefaultHistory()}
	m := NewModel(root, nil, &config.Config{})
	m.SetSize(120, 40)
	renderTestTranscript(&m)
	return &m, m.GetAgentState(root.ID())
}

func TestTranscriptReasoningExpandedByDefault(t *testing.T) {
	m, _ := newExpandedDefaultModel(t)

	plain := ansi.Strip(testTranscriptContent(m))
	if !strings.Contains(plain, "EXPANDEDREASONING about the request") {
		t.Fatalf("reasoning content must be shown (expanded) by default, got:\n%s", plain)
	}
	if !strings.Contains(plain, "· thinking") {
		t.Fatalf("reasoning block should keep its thinking header, got:\n%s", plain)
	}
}

func TestTranscriptToolOutputExpandedByDefault(t *testing.T) {
	m, _ := newExpandedDefaultModel(t)

	plain := ansi.Strip(testTranscriptContent(m))
	// The full output body must be present — not a summary, not dropped.
	for _, want := range []string{"SOMETOOLLINE1", "SOMETOOLLINE2"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("tool output must be shown (expanded) by default; missing %q in:\n%s", want, plain)
		}
	}
	if !strings.Contains(plain, "↳ tool output for call-1") {
		t.Fatalf("tool output block should carry a header naming the answered call, got:\n%s", plain)
	}
	if strings.Contains(plain, "/expand to show") {
		t.Fatalf("expanded-by-default transcript must not show the collapsed summary, got:\n%s", plain)
	}
}

func TestCollapseCommandFoldsToolOutputOnly(t *testing.T) {
	m, _ := newExpandedDefaultModel(t)

	m.Input.SetValue("/collapse")
	*m = pressEnter(t, *m)

	plain := ansi.Strip(testTranscriptContent(m))
	if !strings.Contains(plain, "tool output for call-1") || !strings.Contains(plain, "bytes") {
		t.Fatalf("/collapse should fold tool output to a byte-count summary, got:\n%s", plain)
	}
	if strings.Contains(plain, "SOMETOOLLINE1") {
		t.Fatalf("collapsed tool output must hide the output body, got:\n%s", plain)
	}
	// Reasoning is unaffected: thinking stays expanded in both modes.
	if !strings.Contains(plain, "EXPANDEDREASONING about the request") {
		t.Fatalf("/collapse must not fold reasoning, got:\n%s", plain)
	}

	m.Input.SetValue("/expand")
	*m = pressEnter(t, *m)

	plain = ansi.Strip(testTranscriptContent(m))
	if !strings.Contains(plain, "SOMETOOLLINE1") {
		t.Fatalf("/expand must restore the full tool output, got:\n%s", plain)
	}
}

func TestCollapseToggleDefaultsExpandedForNewAgentState(t *testing.T) {
	m, _ := newExpandedDefaultModel(t)
	if m.GetAgentState(m.Focused.ID()).Transcript.toolOutputsCollapsed {
		t.Fatal("a fresh agent state must start expanded (toolOutputsCollapsed = false)")
	}
	// Toggle then reset via /new-style state: a brand-new state never inherits
	// the collapsed flag from another agent.
	m.ToggleToolOutputCollapse(true)
	if !m.GetAgentState(m.Focused.ID()).Transcript.toolOutputsCollapsed {
		t.Fatal("toggled state must record collapsed = true")
	}
	fresh := &AppState{}
	if fresh.Transcript.toolOutputsCollapsed {
		t.Fatal("zero-value AppState must be expanded")
	}
}

func TestCollapseToggleSkipsWhenStateUnchanged(t *testing.T) {
	m, s := newExpandedDefaultModel(t)
	s.Transcript.cache = map[string][]string{"k": {"v"}}
	m.ToggleToolOutputCollapse(false) // already expanded: no-op
	if s.Transcript.cache == nil {
		t.Fatal("no-op toggle must not discard the render cache")
	}
	m.ToggleToolOutputCollapse(true)
	if !s.Transcript.toolOutputsCollapsed {
		t.Fatal("toggle to collapsed must record the flag")
	}
	if s.Transcript.cache != nil {
		t.Fatal("toggling collapse must discard the block cache so blocks re-derive")
	}
	if !s.Transcript.dirty {
		t.Fatal("toggling collapse must mark the transcript dirty")
	}
}

func TestTranscriptCommandListed(t *testing.T) {
	foundCollapse, foundExpand := false, false
	for _, cmd := range AvailableCommands {
		if cmd.Name == "/collapse" {
			foundCollapse = true
			if cmd.Description == "" {
				t.Fatal("/collapse should carry a description for the help view")
			}
		}
		if cmd.Name == "/expand" {
			foundExpand = true
			if cmd.Description == "" {
				t.Fatal("/expand should carry a description for the help view")
			}
		}
	}
	if !foundCollapse || !foundExpand {
		t.Fatal("/collapse and /expand must be listed in AvailableCommands")
	}
}

// statusTextChild-like helpers are in restored_child_test.go; this file uses
// restoredStubSubmitter below.

func newChildIndicatorModel(t *testing.T) (*Model, *AppState, *AppState) {
	t.Helper()
	root := &focusTestOrchestrator{id: common.MainAgentID}
	child := &focusTestOrchestrator{id: "coder-subagent-0", parent: root}
	root.children = []common.Orchestrator{child}
	m := NewModel(root, nil, &config.Config{})
	m.SetSize(120, 30)
	return &m, m.GetAgentState(root.ID()), m.GetAgentState(child.ID())
}

func TestStatusBarLiveChildIndicatorWhileChildRuns(t *testing.T) {
	m, _, childState := newChildIndicatorModel(t)

	// No indicator while the child is idle.
	if plain := ansi.Strip(m.statusBarView()); strings.Contains(plain, "coder #0") {
		t.Fatalf("idle child must not produce an activity indicator: %q", plain)
	}

	// Thinking: the parent status names the child and its activity kind.
	childState.State = StateThinking
	plain := ansi.Strip(m.statusBarView())
	if !strings.Contains(plain, "coder #0: thinking") {
		t.Fatalf("status bar should show a live thinking indicator for the busy child, got %q", plain)
	}

	// Streaming a tool call: the indicator names the tool.
	childState.State = StateStreaming
	childState.StreamingState = common.ContentEvent{
		ID:        "coder-subagent-0",
		ToolCalls: []client.ToolCall{{Function: client.FunctionCall{Name: "bash"}}},
	}
	plain = ansi.Strip(m.statusBarView())
	if !strings.Contains(plain, "coder #0: tool bash") {
		t.Fatalf("status bar should show the running tool in the child indicator, got %q", plain)
	}

	// Focusing the child itself removes the indicator (its own state is on
	// display); focusing elsewhere restores it.
	m.Focused = m.Root.Children()[0]
	if plain := ansi.Strip(m.statusBarView()); strings.Contains(plain, "coder #0: ") {
		t.Fatalf("focused child must not be summarized as a background indicator, got %q", plain)
	}
}

func TestLiveChildIndicatorIgnoredForParentEvents(t *testing.T) {
	// The indicator reads per-agent state; make sure a plain parent state map
	// does not trip it: root focused, no other agent — nothing to show beyond
	// the normal bar.
	m := newStatusBarModel(t, &focusTestOrchestrator{id: common.MainAgentID})
	m.GetAgentState(m.Focused.ID()).State = StateThinking
	plain := ansi.Strip(m.statusBarView())
	if strings.Contains(plain, ": thinking") {
		t.Fatalf("the focused agent's own state must not render as a child indicator: %q", plain)
	}
}

// restoredStubSubmitter mimics agent.RestoredSubagentOrchestrator: the
// read-only record refuses every Submit (the tui package cannot import
// internal/agent, so the shape is stubbed) and exposes the same
// StatusText() string the submit path uses for structural detection.
type restoredStubSubmitter struct {
	mockOrchestrator
	id     string
	status string
}

func (s *restoredStubSubmitter) ID() string { return s.id }

func (s *restoredStubSubmitter) StatusText() string { return s.status }

func (s *restoredStubSubmitter) Submit(string, []string) error {
	return errRestoredReadOnly
}

var errRestoredReadOnly = errorRestoredReadOnly{}

type errorRestoredReadOnly struct{}

func (errorRestoredReadOnly) Error() string { return "subagent restoredStub is restored and read-only" }

func TestSubmitOnRestoredTabRoutesToParent(t *testing.T) {
	// Restore semantics: the root carries a read-only restored child, and the
	// user has tabbed to it (focused). Submitting there must land on the
	// PARENT, never on the stub — the parent can continue the child through
	// the sanctioned spawn_subagent resume flow.
	root := &focusTestOrchestrator{id: common.MainAgentID}
	child := &restoredStubSubmitter{id: "coder-subagent-0", status: "restored — was interrupted"}
	root.children = []common.Orchestrator{child}
	m := NewModel(root, nil, &config.Config{})
	m.SetSize(120, 30)
	m.Focused = child

	updated, _ := m.submitMessage("continue the task")
	next := updated

	if root.submittedText != "continue the task" {
		t.Fatalf("submission on a restored tab must be routed to the parent; root got %q", root.submittedText)
	}
	if child.submittedText != "" {
		t.Fatal("the read-only restored stub must never receive a Submit")
	}
	if next.Err != nil {
		t.Fatalf("routed submission must not surface the read-only refusal: %v", next.Err)
	}
	if state := next.GetAgentState(root.ID()); state.State != StateThinking {
		t.Fatalf("parent state should be thinking after the routed submission, got %v", state.State)
	}
}

func TestSubmitOnLiveFocusedTabUnchanged(t *testing.T) {
	// A live focused agent (no StatusText() string) still receives its own
	// submissions — the reroute is only for restored read-only records.
	root := &focusTestOrchestrator{id: common.MainAgentID}
	child := &focusTestOrchestrator{id: "coder-subagent-0", parent: root}
	root.children = []common.Orchestrator{child}
	m := NewModel(root, nil, &config.Config{})
	m.SetSize(120, 30)
	m.Focused = child

	updated, _ := m.submitMessage("hello child")
	next := updated

	if child.submittedText != "hello child" {
		t.Fatalf("live child should receive its own submissions; got %q", child.submittedText)
	}
	if root.submittedText != "" {
		t.Fatalf("parent must not receive the child's submission; root got %q", root.submittedText)
	}
	if next.Err != nil {
		t.Fatalf("plain live submission must not error: %v", next.Err)
	}
}
