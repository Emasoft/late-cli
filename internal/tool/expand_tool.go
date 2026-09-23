package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ExpandToolName is the registry name of the compaction expand tool. The
// executor also uses it to exempt expand results from re-compaction: the
// tool exists to return full originals, compacting them again would make
// them unreachable.
const ExpandToolName = "expand"

// ExpandStore is the read side of the compaction original-text store
// (implemented by *compaction.Store). The indirection keeps internal/tool
// decoupled from internal/compaction and lets tests inject a fake.
type ExpandStore interface {
	Get(id string) (string, bool)
}

// ExpandTool retrieves the original text of a tool-output segment that
// compaction relocated (compaction-mode "enabled"): compacted results
// contain pointer lines like
//
//	[[elided id=elide-3 lines=12 tokens=310 "first sixty chars …"]]
//
// and calling this tool with such an id returns the full original text.
// It is registered on the main session registry when compaction-mode is
// enabled, and subagents inherit it from the parent registry.
type ExpandTool struct {
	Store ExpandStore
}

func (t ExpandTool) Name() string { return ExpandToolName }

func (t ExpandTool) Description() string {
	return "Retrieve the ORIGINAL text of an elided (compacted) tool-output segment. " +
		"When a large tool result was compacted, history contains pointer lines like " +
		"[[elided id=elide-3 lines=12 tokens=310 \"first sixty chars\"]] instead of the full text. " +
		"Call this tool with that id to fetch the complete original segment."
}

func (t ExpandTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {
				"type": "string",
				"description": "The elided-segment id from an [[elided id=...]] pointer line (e.g. \"elide-3\")."
			}
		},
		"required": ["id"]
	}`)
}

func (t ExpandTool) RequiresConfirmation(args json.RawMessage) bool { return false }

func (t ExpandTool) CallString(args json.RawMessage) string {
	id := getToolParam(args, "id")
	if id == "" {
		return "Retrieving elided segment..."
	}
	return fmt.Sprintf("Retrieving elided segment %s...", truncate(id, 50))
}

func (t ExpandTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid parameters for expand: %w", err)
	}
	id := strings.TrimSpace(params.ID)
	if id == "" {
		return "", fmt.Errorf("id is required (use an id from an [[elided id=...]] pointer line)")
	}
	original, ok := t.Store.Get(id)
	if !ok {
		return "", fmt.Errorf("unknown elided id %q", id)
	}
	return original, nil
}
