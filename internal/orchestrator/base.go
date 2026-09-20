package orchestrator

import (
	"context"
	"encoding/base64"
	"fmt"
	"late/internal/client"
	"late/internal/common"
	"late/internal/executor"
	"late/internal/session"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// BaseOrchestrator implements common.Orchestrator and manages an agent's run loop.
type BaseOrchestrator struct {
	id          string
	sess        *session.Session
	middlewares []common.ToolMiddleware
	eventCh     chan common.Event

	mu       sync.RWMutex
	parent   common.Orchestrator
	children []common.Orchestrator

	// childSeq is a monotonic counter for minting child IDs; guarded by mu
	childSeq int

	// Running state tracker
	isRunning   bool
	pendingMsgs []client.ChatMessage
	acc         executor.StreamAccumulator
	ctx         context.Context
	cancel      context.CancelFunc

	// Stop mechanism
	stopCh chan struct{}

	// Max turns configuration
	maxTurns int

	// droppedEvents counts progress events (streaming ContentEvents and
	// transient "thinking" statuses) that were dropped because eventCh was
	// full — i.e. the event consumer (the TUI's event forwarder) stalled.
	// Atomic: incremented from the run-loop goroutine's send sites, swapped
	// and reported by reportDroppedEvents at the end of each turn (and
	// readable from tests).
	droppedEvents atomic.Int64
}

func NewBaseOrchestrator(id string, sess *session.Session, middlewares []common.ToolMiddleware, maxTurns int) *BaseOrchestrator {
	childSeq := 0
	if sess != nil {
		childSeq = sess.SubagentSeq()
	}
	return &BaseOrchestrator{
		id:          id,
		sess:        sess,
		middlewares: middlewares,
		eventCh:     make(chan common.Event, 100),
		ctx:         context.Background(),
		stopCh:      make(chan struct{}),
		maxTurns:    maxTurns,
		childSeq:    childSeq,
	}
}

func (o *BaseOrchestrator) SetMiddlewares(middlewares []common.ToolMiddleware) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.middlewares = middlewares
}

// trySendProgress delivers ev to o.eventCh without ever blocking the caller.
//
// eventCh is buffered (100) and consumed by the TUI's event-forwarding
// goroutine; a stalled or wedged terminal would otherwise block the agent's
// run loop on every send — the same hang class as the executor retry-callback
// fix that already went non-blocking. Progress sends (high-frequency streaming
// ContentEvents and transient "thinking" statuses) are therefore LOSSY by
// design: the authoritative state lives in the session history and stream
// accumulator, and the terminal status events — which are sent separately and
// remain blocking — are what the TUI state machine actually waits on. Drops
// are counted in droppedEvents and reported by reportDroppedEvents.
func (o *BaseOrchestrator) trySendProgress(ev common.Event) {
	select {
	case o.eventCh <- ev:
	default:
		o.droppedEvents.Add(1)
	}
}

// reportDroppedEvents logs how many progress events were silently dropped
// because the event consumer stalled, then resets the counter. It is called
// from onEndTurn so each turn reports only its own drops. Stderr is used
// instead of a field on the final ContentEvent because extending the shared
// event contract (internal/common) is out of scope for this hardening.
func (o *BaseOrchestrator) reportDroppedEvents() {
	if dropped := o.droppedEvents.Swap(0); dropped > 0 {
		fmt.Fprintf(os.Stderr, "late: %d events dropped (consumer stalled)\n", dropped)
	}
}

func (o *BaseOrchestrator) SetContext(ctx context.Context) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ctx = ctx
}

// resetContextIfCancelled makes a completed run context usable again without
// discarding configuration values attached to it (for example, the
// unsupervised-execution flag and TUI input provider).
func (o *BaseOrchestrator) resetContextIfCancelled() {
	if o.ctx.Err() != nil {
		o.ctx = context.WithoutCancel(o.ctx)
	}
}

func (o *BaseOrchestrator) SetMaxTurns(maxTurns int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.maxTurns = maxTurns
}

func (o *BaseOrchestrator) MaxTokens() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.sess.Client().ContextSize()
}

func (o *BaseOrchestrator) SupportsVision() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.sess.Client().SupportsVision()
}

func (o *BaseOrchestrator) RefreshContextSize(ctx context.Context) {
	o.sess.Client().RefreshContextSize(ctx)
}

func (o *BaseOrchestrator) QueuedMessages() []string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	var msgs []string
	for _, m := range o.pendingMsgs {
		msgs = append(msgs, m.Content.String())
	}
	return msgs
}

func (o *BaseOrchestrator) ID() string { return o.id }

func (o *BaseOrchestrator) Submit(text string, images []string) error {
	msg := client.ChatMessage{Role: "user", AttachedFiles: images}

	if len(images) == 0 {
		msg.Content = client.TextContent(text)
	} else {
		parts := []client.ContentPart{
			{Type: client.ContentPartText, Text: text},
		}
		supportsVision := o.SupportsVision()
		for _, imgPath := range images {
			data, err := os.ReadFile(imgPath)
			if err != nil {
				return fmt.Errorf("failed to read file %s: %w", imgPath, err)
			}

			mimeType := http.DetectContentType(data)
			// Only attach to LLM content if it's an image AND the model supports vision
			if strings.HasPrefix(mimeType, "image/") && supportsVision {
				encoded := base64.StdEncoding.EncodeToString(data)
				parts = append(parts, client.ContentPart{
					Type: client.ContentPartImageURL,
					ImageURL: &client.ImageURL{
						URL: fmt.Sprintf("data:%s;base64,%s", mimeType, encoded),
					},
				})
			} else {
				// Treat as text if it looks like text or has a common extension.
				// If it's an image but the model doesn't support vision, just include a note.
				content := ""
				isAttachment := true
				if strings.HasPrefix(mimeType, "image/") {
					content = fmt.Sprintf("\nAttached Image: %s (Vision not supported by current model)\n", filepath.Base(imgPath))
				} else {
					content = fmt.Sprintf("\nFilename: %s\nContent: ```\n%s\n```\n", filepath.Base(imgPath), string(data))
				}

				parts = append(parts, client.ContentPart{
					Type:         client.ContentPartText,
					Text:         content,
					IsAttachment: isAttachment,
				})
			}
		}
		msg.Content = client.MessageContent{Parts: parts}
	}

	o.mu.Lock()
	if o.isRunning {
		o.pendingMsgs = append(o.pendingMsgs, msg)
		o.mu.Unlock()
		o.eventCh <- common.MessageQueuedEvent{ID: o.id, Text: text}
		return nil
	}

	o.isRunning = true
	// Clear any old cancellation state so a new run isn't instantly aborted
	o.cancel = nil
	// Reset the base context if it was already cancelled.
	o.resetContextIfCancelled()
	o.mu.Unlock()

	if err := o.sess.AddMessage(msg); err != nil {
		o.mu.Lock()
		o.isRunning = false
		o.mu.Unlock()
		return err
	}

	// Transient status: non-blocking with drop counting. A stalled consumer
	// must not wedge the caller here; the next terminal status (below) is
	// what the TUI state machine relies on.
	o.trySendProgress(common.StatusEvent{ID: o.id, Status: "thinking"})
	// Start the run loop in a background goroutine
	go o.run()
	return nil
}

func (o *BaseOrchestrator) Execute(text string) (string, error) {
	o.mu.Lock()
	if o.isRunning {
		o.mu.Unlock()
		return "", fmt.Errorf("orchestrator is already running")
	}
	o.isRunning = true
	o.resetContextIfCancelled()
	ctx, cancel := context.WithCancel(o.ctx)
	o.cancel = cancel
	o.ctx = ctx // Set the Context for this execution
	o.mu.Unlock()

	defer cancel()

	// Inject orchestrator ID into context for tool interactions
	ctx = context.WithValue(ctx, common.OrchestratorIDKey, o.id)

	if strings.TrimSpace(text) != "" {
		if err := o.sess.AddUserMessage(text); err != nil {
			return "", err
		}
	}

	// Transient status: non-blocking with drop counting (see trySendProgress).
	o.trySendProgress(common.StatusEvent{ID: o.id, Status: "thinking"})
	// The terminal "idle" status MUST be delivered or the TUI hangs in its
	// running state ("Stopping..."), so this send stays blocking even if the
	// consumer is stalled. It fires once, after all work is done.
	defer func() {
		o.mu.Lock()
		o.isRunning = false
		o.pendingMsgs = nil
		o.mu.Unlock()
		o.eventCh <- common.StatusEvent{ID: o.id, Status: "idle"}
	}()

	// Build extra body
	var extraBody map[string]any

	onStartTurn := func() {
		o.RefreshContextSize(ctx)
		o.mu.Lock()
		msgs := o.pendingMsgs
		o.pendingMsgs = nil
		o.acc.Reset()
		o.mu.Unlock()

		for _, msg := range msgs {
			_ = o.sess.AddMessage(msg)
		}

		// Transient per-turn status: non-blocking with drop counting (see
		// trySendProgress).
		o.trySendProgress(common.StatusEvent{ID: o.id, Status: "thinking"})
	}

	onEndTurn := func() {
		o.RefreshContextSize(ctx)
		o.mu.Lock()
		usage := o.acc.Usage
		o.acc.Reset()
		o.mu.Unlock()
		// Turn-boundary signal carrying the turn's Usage: kept BLOCKING. It is
		// low-frequency (once per turn, not per chunk) and is the
		// authoritative end-of-turn marker the TUI uses to finalize the
		// message and recompute token counts; dropping it would leave the
		// rendered transcript incomplete even after the consumer catches up.
		o.eventCh <- common.ContentEvent{ID: o.id, Usage: usage, Completed: true}
		// Report any progress events dropped while the consumer was stalled
		// earlier in this turn (and reset the counter for the next turn).
		o.reportDroppedEvents()
	}

	res, err := executor.RunLoop(
		ctx,
		o.sess,
		o.maxTurns,
		extraBody,
		onStartTurn,
		onEndTurn,
		func(res common.StreamResult) {
			o.mu.Lock()
			o.acc.Append(res)
			accCopy := o.acc
			o.mu.Unlock()

			// High-frequency streaming delta: non-blocking with drop counting
			// (see trySendProgress) — a stalled TUI consumer must never wedge
			// the run loop mid-stream.
			o.trySendProgress(common.ContentEvent{
				ID:               o.id,
				Content:          accCopy.Content,
				ReasoningContent: accCopy.Reasoning,
				ToolCalls:        accCopy.ToolCalls,
				Usage:            accCopy.Usage,
			})
		},
		o.middlewares,
	)

	// Terminal statuses: kept BLOCKING — the TUI hangs in "Stopping..." (or in
	// the running state) if it never receives the run's final status, so these
	// must be delivered even to a stalled consumer.
	if err != nil {
		o.eventCh <- common.StatusEvent{ID: o.id, Status: "error", Error: err}
	} else {
		o.eventCh <- common.StatusEvent{ID: o.id, Status: "closed"}
	}
	return res, err
}

func (o *BaseOrchestrator) run() {
	o.mu.Lock()
	o.resetContextIfCancelled()
	ctx, cancel := context.WithCancel(o.ctx)
	o.cancel = cancel
	o.ctx = ctx // Set the context so Execute/RunLoop can share the cancelable context safely
	o.mu.Unlock()

	defer cancel() // Ensure we don't leak the context when run() finishes

	// Inject orchestrator ID into context for tool interactions
	ctx = context.WithValue(ctx, common.OrchestratorIDKey, o.id)

	for {
		onStartTurn := func() {
			o.RefreshContextSize(ctx)
			o.mu.Lock()
			msgs := o.pendingMsgs
			o.pendingMsgs = nil
			o.acc.Reset()
			o.mu.Unlock()

			for _, msg := range msgs {
				_ = o.sess.AddMessage(msg)
			}

			// Transient per-turn status: non-blocking with drop counting (see
			// trySendProgress).
			o.trySendProgress(common.StatusEvent{ID: o.id, Status: "thinking"})
		}

		onEndTurn := func() {
			o.RefreshContextSize(ctx)
			o.mu.Lock()
			usage := o.acc.Usage
			o.acc.Reset()
			o.mu.Unlock()
			// Turn-boundary signal carrying the turn's Usage: kept BLOCKING
			// (same reasoning as Execute's onEndTurn).
			o.eventCh <- common.ContentEvent{ID: o.id, Usage: usage, Completed: true}
			// Report any progress events dropped while the consumer was
			// stalled earlier in this turn, then reset the counter.
			o.reportDroppedEvents()
		}

		// Build extra body
		var extraBody map[string]any

		_, err := executor.RunLoop(
			ctx,
			o.sess,
			o.maxTurns,
			extraBody,
			onStartTurn,
			onEndTurn,
			func(res common.StreamResult) {
				o.mu.Lock()
				o.acc.Append(res)
				accCopy := o.acc // Copy for event
				o.mu.Unlock()

				// High-frequency streaming delta: non-blocking with drop
				// counting (see trySendProgress) — a stalled TUI consumer
				// must never wedge the run loop mid-stream.
				o.trySendProgress(common.ContentEvent{
					ID:               o.id,
					Content:          accCopy.Content,
					ReasoningContent: accCopy.Reasoning,
					ToolCalls:        accCopy.ToolCalls,
					Usage:            accCopy.Usage,
				})
			},
			o.middlewares,
		)

		// Reset accumulator after finished or ready for next turn
		o.mu.Lock()
		o.acc.Reset()
		hasPending := len(o.pendingMsgs) > 0
		if !hasPending {
			o.isRunning = false
		}
		o.mu.Unlock()

		if err != nil {
			// If the error is about unsupported image input, roll back the user message
			// so it doesn't poison the context for future requests.
			errStr := err.Error()
			if strings.Contains(errStr, "image input is not supported") ||
				strings.Contains(errStr, "image_input") ||
				strings.Contains(errStr, "does not support image") {
				// Remove the last user message from history
				if len(o.sess.History) > 0 && o.sess.History[len(o.sess.History)-1].Role == "user" {
					o.sess.History = o.sess.History[:len(o.sess.History)-1]
				}
				// Terminal status: kept BLOCKING — the TUI must observe the
				// run's final status or it stays wedged in its running state.
				o.eventCh <- common.StatusEvent{ID: o.id, Status: "error", Error: fmt.Errorf("image_unsupported")}
			} else {
				// Terminal status: kept BLOCKING (see above).
				o.eventCh <- common.StatusEvent{ID: o.id, Status: "error", Error: err}
			}
			break
		}

		if !hasPending {
			// Terminal status: kept BLOCKING — the TUI must observe the run's
			// final status or it stays wedged in its running state.
			o.eventCh <- common.StatusEvent{ID: o.id, Status: "idle"}
			break
		}
	}

	// Check if stop was requested and send StopRequestedEvent
	if o.IsStopRequested() {
		// Terminal, fires once: kept BLOCKING so the TUI reliably observes it.
		o.eventCh <- common.StopRequestedEvent{ID: o.id}
	}
}

func (o *BaseOrchestrator) Events() <-chan common.Event {
	return o.eventCh
}

func (o *BaseOrchestrator) Cancel() {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.cancel != nil {
		o.cancel()
	}

	select {
	case o.stopCh <- struct{}{}:
		// Signal sent
	default:
		// Already signaled, ignore
	}
}

func (o *BaseOrchestrator) IsStopRequested() bool {
	select {
	case <-o.stopCh:
		return true
	default:
		return false
	}
}

func (o *BaseOrchestrator) History() []client.ChatMessage {
	return o.sess.History
}

func (o *BaseOrchestrator) Session() *session.Session {
	return o.sess
}

func (o *BaseOrchestrator) SystemPrompt() string {
	return o.sess.SystemPrompt()
}

func (o *BaseOrchestrator) ToolDefinitions() []client.ToolDefinition {
	return o.sess.GetToolDefinitions()
}

func (o *BaseOrchestrator) Context() context.Context {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.ctx
}

func (o *BaseOrchestrator) Middlewares() []common.ToolMiddleware {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.middlewares
}

func (o *BaseOrchestrator) Registry() *common.ToolRegistry {
	return o.sess.Registry
}

func (o *BaseOrchestrator) Children() []common.Orchestrator {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make([]common.Orchestrator, len(o.children))
	copy(out, o.children)
	return out
}

func (o *BaseOrchestrator) Parent() common.Orchestrator {
	return o.parent
}

func (o *BaseOrchestrator) Reset() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.sess.StartNewConversation(); err != nil {
		return err
	}
	for _, registeredTool := range o.sess.Registry.All() {
		if resetter, ok := registeredTool.(common.ConversationResetter); ok {
			resetter.ResetConversationState()
		}
	}
	return nil
}

func (o *BaseOrchestrator) Rewind(index int) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if index < 0 || index >= len(o.sess.History) {
		return fmt.Errorf("invalid history index")
	}
	o.sess.History = o.sess.History[:index]
	if o.sess.HistoryPath != "" {
		if err := session.SaveHistory(o.sess.HistoryPath, o.sess.History); err != nil {
			return err
		}
		return o.sess.UpdateSessionMetadata()
	}
	return nil
}

// NextChildID atomically reserves and mints the next child ID under o.mu. The
// counter is shared across agent types (e.g., `researcher-subagent-0`, then
// `coder-subagent-1`), matching the legacy `len(children)` numbering scheme.
func (o *BaseOrchestrator) NextChildID(agentType string) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	id := fmt.Sprintf("%s-subagent-%d", agentType, o.childSeq)
	if err := o.sess.UpdateSubagentSeq(o.childSeq + 1); err != nil {
		return "", fmt.Errorf("failed to reserve child ID: %w", err)
	}
	o.childSeq++
	return id, nil
}

func (o *BaseOrchestrator) AddChild(child common.Orchestrator) {
	o.mu.Lock()
	o.children = append(o.children, child)
	o.mu.Unlock()

	// ChildAddedEvent MUST be sent BLOCKING (no select/default): the TUI's
	// ForwardOrchestratorEvents only spawns the child's event-forwarding
	// goroutine when it receives this event, so dropping it would silently
	// orphan the child's entire event stream. It has a guaranteed consumer by
	// construction, and AddChild runs on the (single) subagent-runner
	// goroutine, so a brief block here only backpressures child creation
	// until the parent's consumer catches up.
	o.eventCh <- common.ChildAddedEvent{
		ParentID: o.id,
		Child:    child,
	}
}
