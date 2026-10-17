package main

// In-process contract tests for prompt-exit (-prompt) fail-fast. These
// deliberately avoid running the late binary: prior attempts drove spawned
// subprocesses and hung the test harness, so everything here runs in-process;
// only the pure TUI update logic and the executor RunLoop are exercised.
//
// Covered contracts:
//  1. tui prompt-exit: a terminal root-agent error while PromptExitOn is on
//     sets ExitErr and returns a quit command; non-root errors and the
//     PromptExitOn=false case do neither (internal/tui/update.go's
//     already-touched terminal-error branch).
//  2. executor fail-fast: a run against a dead LLM endpoint (a closed TCP
//     port) surfaces an error from RunLoop well inside the infra retry
//     budget's backoff cost, instead of the historical idle-forever behavior.

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"late/internal/client"
	"late/internal/common"
	lateconfig "late/internal/config"
	"late/internal/executor"
	"late/internal/session"
	"late/internal/tui"
)

// --- Test 1: TUI prompt-exit terminal error handling ---

// stubOrchestratorMin implements common.Orchestrator minimally for TUI model
// construction during these tests. It is intentionally inert: only ID() matters.
type stubOrchestratorMin struct{}

func (s *stubOrchestratorMin) ID() string                               { return "stub-root" }
func (s *stubOrchestratorMin) Submit(string, []string) error            { return nil }
func (s *stubOrchestratorMin) Execute(string) (string, error)           { return "", nil }
func (s *stubOrchestratorMin) Reset() error                             { return nil }
func (s *stubOrchestratorMin) Rewind(int) error                         { return nil }
func (s *stubOrchestratorMin) Cancel()                                  {}
func (s *stubOrchestratorMin) IsStopRequested() bool                    { return false }
func (s *stubOrchestratorMin) Events() <-chan common.Event              { return nil }
func (s *stubOrchestratorMin) History() []client.ChatMessage            { return nil }
func (s *stubOrchestratorMin) Context() context.Context                 { return context.Background() }
func (s *stubOrchestratorMin) Middlewares() []common.ToolMiddleware     { return nil }
func (s *stubOrchestratorMin) SetMiddlewares([]common.ToolMiddleware)   {}
func (s *stubOrchestratorMin) Registry() *common.ToolRegistry           { return nil }
func (s *stubOrchestratorMin) SystemPrompt() string                     { return "" }
func (s *stubOrchestratorMin) ToolDefinitions() []client.ToolDefinition { return nil }
func (s *stubOrchestratorMin) Children() []common.Orchestrator          { return nil }
func (s *stubOrchestratorMin) Parent() common.Orchestrator              { return nil }
func (s *stubOrchestratorMin) SetMaxTurns(int)                          {}
func (s *stubOrchestratorMin) RefreshContextSize(context.Context)       {}
func (s *stubOrchestratorMin) MaxTokens() int                           { return 100 }
func (s *stubOrchestratorMin) SupportsVision() bool                     { return false }
func (s *stubOrchestratorMin) QueuedMessages() []string                 { return nil }
func (s *stubOrchestratorMin) DrainQueuedMessages() []string            { return nil }

func newPromptExitModel(t *testing.T, promptExitOn bool) *tui.Model {
	t.Helper()
	m := tui.NewModel(&stubOrchestratorMin{}, nil, &lateconfig.Config{})
	m.SetSize(80, 24)
	m.PromptExitOn = promptExitOn
	return &m
}

func dispatchTerminalError(t *testing.T, m *tui.Model, agentID string) (quit bool) {
	t.Helper()
	updated, cmd := m.Update(tui.OrchestratorEventMsg{Event: common.StatusEvent{
		ID:     agentID,
		Status: "error",
		Error:  errors.New("stream error: connection refused"),
	}})
	next, ok := updated.(tui.Model)
	if !ok {
		t.Fatalf("Update returned %T, want tui.Model", updated)
	}
	*m = next
	if cmd == nil {
		return false
	}
	// Flatten the produced command the way Bubble Tea would and look for the
	// tea.QuitMsg anywhere in the batch tree.
	var sawQuit bool
	var flatten func(tea.Msg)
	flatten = func(msg tea.Msg) {
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				flatten(c())
			}
			return
		}
		if _, isQuit := msg.(tea.QuitMsg); isQuit {
			sawQuit = true
		}
	}
	flatten(cmd())
	return sawQuit
}

// TestPromptExitTerminalErrorOnRootAgentQuits pins the quit contract: when
// prompt-exit mode is on and the ROOT agent reports a terminal error, the
// update must record ExitErr AND return a quit command.
func TestPromptExitTerminalErrorOnRootAgentQuits(t *testing.T) {
	m := newPromptExitModel(t, true)
	if !dispatchTerminalError(t, m, m.Root.ID()) {
		t.Fatal("prompt-exit terminal root error must produce a quit command")
	}
	if m.ExitErr == nil {
		t.Fatal("ExitErr is nil; the terminal root error must be recorded for cmd/late/main.go's stderr print + exit 1")
	}
	if !strings.Contains(m.ExitErr.Error(), "connection refused") {
		t.Errorf("ExitErr = %v, want the underlying stream error", m.ExitErr)
	}
}

// TestPromptExitOffDoesNotQuit pins that without prompt-exit mode the same
// terminal error neither records ExitErr nor quits (interactive mode keeps
// running for the user to read the error).
func TestPromptExitOffDoesNotQuit(t *testing.T) {
	m := newPromptExitModel(t, false)
	if dispatchTerminalError(t, m, m.Root.ID()) {
		t.Fatal("interactive mode must not quit on terminal error")
	}
	if m.ExitErr != nil {
		t.Errorf("ExitErr = %v, want nil when PromptExitOn is false", m.ExitErr)
	}
}

// TestPromptExitSubagentErrorDoesNotQuit pins that a background subagent's
// failure does not trigger the prompt-exit quit: only the root agent's own
// turn ending in error means the one-shot run failed.
func TestPromptExitSubagentErrorDoesNotQuit(t *testing.T) {
	m := newPromptExitModel(t, true)
	if dispatchTerminalError(t, m, "stub-subagent-not-root") {
		t.Fatal("subagent error must not quit in prompt-exit mode")
	}
	if m.ExitErr != nil {
		t.Errorf("ExitErr = %v, want nil for a non-root agent error", m.ExitErr)
	}
}

// --- Test 2: executor fail-fast against a dead endpoint ---

// TestRunLoopDeadEndpointFailsFast pins the in-process fail-fast contract:
// with a full infra retry budget available, a run whose endpoint is a closed
// TCP port must return an error promptly (first dial failure fails the turn
// via isStreamDialFailure + everSucceeded gating) instead of burning the
// jittered backoff budget. All in-process: RunLoop -> session ->
// client -> a dial that gets connection-refused instantly.
func TestRunLoopDeadEndpointFailsFast(t *testing.T) {
	// A guaranteed-dead endpoint: bind a port, then close it. Connecting to
	// it fails within milliseconds with ECONNREFUSED on loopback.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listener: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	c := client.NewClient(client.Config{BaseURL: "http://127.0.0.1:" + strconv.Itoa(port) + "/v1"})
	sess := session.New(c, "", []client.ChatMessage{
		{Role: "user", Content: client.TextContent("hello")},
	}, "", false)

	// A healthy-looking infra budget: if the fail-fast gate were missing,
	// the jittered backoff over 6 attempts would blow well past the 20s
	// deadline, so this test fails exactly when the old bug reappears.
	ctx, cancel := context.WithTimeout(
		context.WithValue(context.Background(), common.MaxStreamRetriesKey, 6),
		20*time.Second,
	)
	defer cancel()

	var retries []common.RetryEvent
	start := time.Now()
	_, runErr := executor.RunLoop(ctx, sess, 1, nil, nil, nil, nil, func(ev common.RetryEvent) {
		retries = append(retries, ev)
	}, nil, nil)
	elapsed := time.Since(start)

	if runErr == nil {
		t.Fatal("RunLoop against a dead endpoint returned no error; fail-fast contract broken")
	}
	if elapsed >= 20*time.Second {
		t.Fatalf("RunLoop took %s against a dead endpoint — dial failures must surface before the retry budget", elapsed)
	}
	if len(retries) > 1 {
		t.Errorf("got %d retry events for an immediate dial failure: %+v", len(retries), retries)
	}
}

var digits = "0123456789"
