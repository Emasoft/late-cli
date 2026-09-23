package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"late/internal/compaction"
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

// ExpandTool retrieves the original text of a tool-output run that
// compaction relocated (compaction-mode "enabled"): compacted results
// contain pointer lines like
//
//	[[elided id=r:1a2b3c4d lines=12-40 tokens=310 "first 120 chars of the run …"]]
//
// (content-addressed ids, r:<8 hex>) — or, from older builds,
//
//	[[elided id=elide-3 lines=12 tokens=310 "first sixty chars …"]]
//
// and calling this tool with such an id returns the full original text.
// Passing a whole pointer line instead of the bare id works too: the id is
// parsed out of it. It is registered on the main session registry when
// compaction-mode is enabled, and subagents inherit it from the parent
// registry.
type ExpandTool struct {
	Store ExpandStore
}

func (t ExpandTool) Name() string { return ExpandToolName }

func (t ExpandTool) Description() string {
	return "Retrieve the ORIGINAL text of an elided (compacted) tool-output run. " +
		"When a large tool result was compacted, history contains pointer lines like " +
		"[[elided id=r:1a2b3c4d lines=12-40 tokens=310 \"first 120 chars of the run\"]] " +
		"(content-addressed id, r: plus 8 hex chars; legacy builds minted elide-N ids) " +
		"instead of the full text. " +
		"Call this tool with that id — or with the whole pointer line — to fetch the complete original."
}

func (t ExpandTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {
				"type": "string",
				"description": "The elided-run id from an [[elided id=...]] pointer line (content id like \"r:1a2b3c4d\", or a legacy \"elide-3\"); a whole pointer line is also accepted."
			}
		},
		"required": ["id"]
	}`)
}

func (t ExpandTool) RequiresConfirmation(args json.RawMessage) bool { return false }

func (t ExpandTool) CallString(args json.RawMessage) string {
	id := expandID(args)
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
	id := expandIDFromArg(params.ID)
	if id == "" {
		return "", fmt.Errorf("id is required (use an id from an [[elided id=...]] pointer line)")
	}
	original, ok := t.Store.Get(id)
	if !ok {
		return "", fmt.Errorf("unknown elided id %q", id)
	}
	return original, nil
}

// expandID extracts the id argument from raw tool arguments for the
// progress string; unknown shapes yield "".
func expandID(args json.RawMessage) string {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return ""
	}
	return expandIDFromArg(params.ID)
}

// expandIDFromArg normalizes the id argument: surrounding whitespace is
// trimmed, and a whole [[elided …]] pointer line is accepted in place of the
// bare id — its id is parsed out with the shared pointer parser, so content
// ids and legacy counter ids both work.
func expandIDFromArg(arg string) string {
	arg = strings.TrimSpace(arg)
	if strings.Contains(arg, "[[elided") {
		if p, ok := compaction.ParsePointer(arg); ok {
			return p.ID
		}
	}
	return arg
}
