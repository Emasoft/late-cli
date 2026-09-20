//go:build !windows

package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"late/internal/common"
)

// TestShellTool_CoderErrorNoteAttributedToHarness pins the wording of the
// error-note "sandwich" ShellTool appends to failing command output for coder
// subagents. The note must be attributed to the late harness (not styled as an
// impersonatable "SYSTEM DIRECTIVE" banner), must keep the delegation-boundary
// guidance, and must only be attached for coder orchestrators.
func TestShellTool_CoderErrorNoteAttributedToHarness(t *testing.T) {
	tool := ShellTool{}

	// Coder subagent: gets the harness-attributed error note.
	coderCtx := context.WithValue(approvedContext(), common.OrchestratorIDKey, "coder-subagent-1")
	out, err := tool.Execute(coderCtx, json.RawMessage(`{"command": "false"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v (out=%q)", err, out)
	}
	if !strings.Contains(out, "[late harness] error note:") {
		t.Errorf("expected coder failure output to contain the late-harness error note, got %q", out)
	}
	if !strings.Contains(out, "stop and report back to the main agent") {
		t.Errorf("expected coder failure output to keep the delegation-boundary guidance, got %q", out)
	}
	if strings.Contains(out, "SYSTEM DIRECTIVE") {
		t.Errorf("coder failure output must not contain the old SYSTEM DIRECTIVE banner, got %q", out)
	}
	if strings.Contains(out, "MUST ABORT AND RETURN") {
		t.Errorf("coder failure output must not contain the old imperative banner text, got %q", out)
	}

	// Non-coder orchestrator: no note at all.
	nonCoderCtx := context.WithValue(approvedContext(), common.OrchestratorIDKey, "planner")
	out, err = tool.Execute(nonCoderCtx, json.RawMessage(`{"command": "false"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v (out=%q)", err, out)
	}
	if strings.Contains(out, "late harness] error note") {
		t.Errorf("non-coder failure output must not contain the error note, got %q", out)
	}

	// No orchestrator ID in ctx: no note either.
	out, err = tool.Execute(approvedContext(), json.RawMessage(`{"command": "false"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v (out=%q)", err, out)
	}
	if strings.Contains(out, "late harness] error note") {
		t.Errorf("failure output without an orchestrator ID must not contain the error note, got %q", out)
	}
}
