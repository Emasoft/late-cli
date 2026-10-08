package agent

import (
	"context"
	"fmt"

	"late/internal/client"
	"late/internal/common"
	"late/internal/session"
)

// RestoredSubagentStatus is the status text the TUI shows for a restored
// subagent restored as the read-only fallback projection (Phase 3b of
// subagent persistence).
const RestoredSubagentStatus = "restored — was interrupted"

// RestoredResumedStatus is the status text the TUI shows for a subagent
// restored LIVE after an interruption: it continues its task where the
// interruption stopped. The seed is replaced by the run's own status as soon
// as the resumed first turn reports.
const RestoredResumedStatus = "resumed from interruption"

// NewRestoredSubagentOrchestrator builds a READ-ONLY orchestrator stub for a
// subagent that was interrupted by a previous late exit. It is the DEGRADED
// restore path: the normal restore live-resumes the child (the {"resume":
// "<id>"} machinery), so this stub is built only when the live relaunch is
// impossible — no scheduler or model routing, or a history file that cannot
// be reloaded — and exists so the interrupted work stays visible in the TUI
// (tab switching, transcript view) instead of silently dropping from the UI.
//
// Read-only by construction: the registry is empty (no tool can be called),
// and every mutating surface refuses — Execute always errors, Submit/Cancel
// are no-ops, Reset/Rewind report an error — because the restored history is
// the record of what happened before the exit, and this projection cannot
// continue it.
//
// statusText is surfaced through the interface-exposed state text (empty →
// RestoredSubagentStatus), so the resume layer can carry the record's cause
// into the UI ("restored — was interrupted").
//
// The history is loaded from historyPath (LoadHistory); a missing file — a
// record whose run persisted nothing, or persistence disabled — yields an
// empty history and is NOT an error: the child is still listed, with nothing
// to show. A corrupt history file IS reported: the manifest pointed at
// preserved work, and silently hiding it would misrepresent what survived.
func NewRestoredSubagentOrchestrator(id, agentType, goal, historyPath, statusText string) (*RestoredSubagentOrchestrator, error) {
	if id == "" {
		return nil, fmt.Errorf("restored subagent: empty id")
	}
	history, err := session.LoadHistory(historyPath)
	if err != nil {
		return nil, fmt.Errorf("restored subagent %s: %w", id, err)
	}
	if statusText == "" {
		statusText = RestoredSubagentStatus
	}
	return &RestoredSubagentOrchestrator{
		id:         id,
		agentType:  agentType,
		goal:       goal,
		history:    history,
		statusText: statusText,
	}, nil
}

// RestoredSubagentOrchestrator satisfies common.Orchestrator so the TUI can
// treat a persisted, interrupted subagent like any other historical child.
// It holds no session and no client: every method either returns the loaded
// history or refuses — it is a UI projection of the manifest + history file,
// not a live agent.
type RestoredSubagentOrchestrator struct {
	id         string
	agentType  string
	goal       string
	history    []client.ChatMessage
	statusText string
}

// AgentType returns the subagent type from the manifest record ("coder",
// "researcher", ...).
func (o *RestoredSubagentOrchestrator) AgentType() string { return o.agentType }

// Goal returns the delegated task from the manifest record.
func (o *RestoredSubagentOrchestrator) Goal() string { return o.goal }

// StatusText returns the restored-status line the TUI can surface.
func (o *RestoredSubagentOrchestrator) StatusText() string { return o.statusText }

func (o *RestoredSubagentOrchestrator) ID() string { return o.id }

// Execute always fails: this projection cannot continue the interrupted
// work — a live restore (the normal path) constructs the child through
// NewResumedSubagentOrchestrator instead. The wording stays factual about
// what this projection is, without pretending a re-spawn is the remedy (the
// spawn_subagent {"resume": "<id>"} flow is).
func (o *RestoredSubagentOrchestrator) Execute(string) (string, error) {
	return "", fmt.Errorf("subagent %s could not be live-restored and is read-only; continue it with spawn_subagent {\"resume\": %q} (or spawn a fresh agent)", o.id, o.id)
}

// Submit always fails, mirroring Execute: nothing can be queued onto a
// restored record.
func (o *RestoredSubagentOrchestrator) Submit(string, []string) error {
	return fmt.Errorf("subagent %s could not be live-restored and is read-only; continue it with spawn_subagent {\"resume\": %q} (or spawn a fresh agent)", o.id, o.id)
}

// Reset always fails: resetting would discard the loaded history — the
// preserved work the restore exists to surface.
func (o *RestoredSubagentOrchestrator) Reset() error {
	return fmt.Errorf("subagent %s is restored and read-only; its history cannot be reset", o.id)
}

// Rewind always fails for the same reason as Reset: the loaded history is
// the preserved record.
func (o *RestoredSubagentOrchestrator) Rewind(int) error {
	return fmt.Errorf("subagent %s is restored and read-only; its history cannot be rewound", o.id)
}

// Cancel is a no-op: there is nothing running to cancel.
func (o *RestoredSubagentOrchestrator) Cancel() {}

// IsStopRequested is always false: no run exists.
func (o *RestoredSubagentOrchestrator) IsStopRequested() bool { return false }

// Events returns a CLOSED channel: the TUI's event forwarder
// (ForwardOrchestratorEvents) ranges over this channel, so a nil channel —
// the Go default for "no events" — would block that goroutine forever and
// leak it for every restored child. Closed delivers the zero-value "end of
// events" immediately.
func (o *RestoredSubagentOrchestrator) Events() <-chan common.Event {
	ch := make(chan common.Event)
	close(ch)
	return ch
}

// History returns the restored child's loaded history — what the TUI
// transcript renders.
func (o *RestoredSubagentOrchestrator) History() []client.ChatMessage { return o.history }

// Context returns a background context: no run exists to inherit one.
func (o *RestoredSubagentOrchestrator) Context() context.Context { return context.Background() }

// Middlewares returns nil: no tool ever runs.
func (o *RestoredSubagentOrchestrator) Middlewares() []common.ToolMiddleware { return nil }

// SetMiddlewares is a no-op: read-only.
func (o *RestoredSubagentOrchestrator) SetMiddlewares([]common.ToolMiddleware) {}

// Registry returns nil: no tools. The TUI renders tool badges from the
// history's recorded tool-call names without a registry, and the transcript
// guards the registry lookup with nil checks.
func (o *RestoredSubagentOrchestrator) Registry() *common.ToolRegistry { return nil }

// SystemPrompt returns the delegated task: the goal is the only prompt
// context a restored record preserves.
func (o *RestoredSubagentOrchestrator) SystemPrompt() string { return o.goal }

// ToolDefinitions returns nil: no tools.
func (o *RestoredSubagentOrchestrator) ToolDefinitions() []client.ToolDefinition { return nil }

// Children returns nil: a restored child never runs anything nested.
func (o *RestoredSubagentOrchestrator) Children() []common.Orchestrator { return nil }

// Parent returns nil: the root link is wired by the caller's AddChild, which
// does not set the child's own parent pointer.
func (o *RestoredSubagentOrchestrator) Parent() common.Orchestrator { return nil }

// SetMaxTurns is a no-op: read-only.
func (o *RestoredSubagentOrchestrator) SetMaxTurns(int) {}

// RefreshContextSize is a no-op: there is no client to probe.
func (o *RestoredSubagentOrchestrator) RefreshContextSize(context.Context) {}

// MaxTokens returns 0, the context-size-unknown convention: the TUI renders
// an unlimited/unknown context bar and never divides by the value. A real
// ContextSize would require a live client, which a restored record has not.
func (o *RestoredSubagentOrchestrator) MaxTokens() int { return 0 }

// SupportsVision is false: image attachments cannot be submitted to a
// read-only record anyway.
func (o *RestoredSubagentOrchestrator) SupportsVision() bool { return false }

// QueuedMessages returns nil: nothing can be queued.
func (o *RestoredSubagentOrchestrator) QueuedMessages() []string { return nil }

// DrainQueuedMessages returns nil: nothing can be queued.
func (o *RestoredSubagentOrchestrator) DrainQueuedMessages() []string { return nil }
