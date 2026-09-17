package session

import (
	"late/internal/client"
)

// interruptedToolResultText is the content synthesized for tool calls whose
// execution was interrupted before a result was recorded (e.g. a crash or
// error between the assistant-history commit and the tool-result writes).
const interruptedToolResultText = "(tool execution was interrupted; no result was recorded)"

// SanitizeForRequest returns a request-safe copy of msgs for strict
// OpenAI-compatible endpoints: every assistant message carrying tool_calls is
// closed by one tool result per tool_call_id, synthesizing an interrupted-
// result placeholder when history is missing one. It never mutates the input.
func SanitizeForRequest(msgs []client.ChatMessage) []client.ChatMessage {
	if len(msgs) == 0 {
		return msgs
	}

	sanitized := make([]client.ChatMessage, 0, len(msgs)+2)

	// pendingIDs holds tool-call IDs awaiting a result, in registration order,
	// so synthesized placeholders appear deterministically; pendingSet gives
	// O(1) membership tests. Both are emptied in lockstep.
	pendingIDs := make([]string, 0, 4)
	pendingSet := make(map[string]struct{}, 4)

	// closePending emits one placeholder tool result per still-pending
	// tool-call ID, in registration order, and resets the pending state.
	closePending := func() {
		for _, id := range pendingIDs {
			delete(pendingSet, id)
			sanitized = append(sanitized, client.ChatMessage{
				Role:       "tool",
				ToolCallID: id,
				Content:    client.TextContent(interruptedToolResultText),
			})
		}
		pendingIDs = pendingIDs[:0]
	}

	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			// A new assistant turn implicitly ends the previous tool group
			// for strict servers: close it before appending this message.
			closePending()
			sanitized = append(sanitized, m)
			for _, tc := range m.ToolCalls {
				if tc.ID == "" {
					// Empty IDs cannot be matched by a tool result; skip them.
					continue
				}
				if _, ok := pendingSet[tc.ID]; ok {
					continue
				}
				pendingIDs = append(pendingIDs, tc.ID)
				pendingSet[tc.ID] = struct{}{}
			}
		case "tool":
			if _, ok := pendingSet[m.ToolCallID]; !ok {
				// Dangling tool result with no matching assistant tool_call:
				// poison for strict endpoints, so drop it.
				continue
			}
			delete(pendingSet, m.ToolCallID)
			for i, id := range pendingIDs {
				if id == m.ToolCallID {
					pendingIDs = append(pendingIDs[:i], pendingIDs[i+1:]...)
					break
				}
			}
			sanitized = append(sanitized, m)
		default:
			// system/user/... turns also terminate an open tool group.
			closePending()
			sanitized = append(sanitized, m)
		}
	}
	closePending()

	return sanitized
}
