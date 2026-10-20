package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

var errRunStub = errors.New("runner stub")

// TestSpawnToolCarriesToolCallID: Execute copies the executor-stamped
// tool-call ID from its context into the spawn request (SpawnCallID), and
// model-supplied argument JSON cannot inject one (the context value wins).
func TestSpawnToolCarriesToolCallID(t *testing.T) {
	var gotSpawnCallID string
	runner := func(ctx context.Context, request SubagentSpawnRequest, _ *time.Duration) (string, error) {
		gotSpawnCallID = request.SpawnCallID
		return "", errRunStub
	}

	ctx := WithToolCallID(context.Background(), "call-42")
	args := json.RawMessage(`{"goal":"g","agent_type":"coder"}`)
	_, _ = SpawnSubagentTool{Runner: runner}.Execute(ctx, args)
	if gotSpawnCallID != "call-42" {
		t.Errorf("SpawnCallID = %q, want call-42", gotSpawnCallID)
	}

	// Injection attempt: args carry a spawn_call_id; with no context stamp
	// the request must stay empty (json:"-" ignores the field).
	gotSpawnCallID = ""
	args2 := json.RawMessage(`{"goal":"g","agent_type":"coder","spawn_call_id":"forged"}`)
	_, _ = SpawnSubagentTool{Runner: runner}.Execute(context.Background(), args2)
	if gotSpawnCallID != "" {
		t.Errorf("arg-injected SpawnCallID = %q, want empty (json:\"-\")", gotSpawnCallID)
	}

	// But a plain context stamp reads back empty elsewhere.
	if id := ToolCallIDFromContext(context.Background()); id != "" {
		t.Errorf("ToolCallIDFromContext(plain ctx) = %q, want empty", id)
	}
}
