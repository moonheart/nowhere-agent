package contextmgmt

import (
	"nowhere-agent/internal/provider"
)

// EnsurePairing normalizes a message list so it satisfies the provider's
// tool_use/tool_result contract before a send (design D3). It is independent of
// compression: a cancelled run or a truncation can also leave the conversation
// with unpaired blocks, and the provider rejects such a request outright.
//
// The repair is conservative and order-preserving:
//   - duplicate tool_use ids are dropped (first occurrence wins);
//   - an assistant message with tool_use blocks that go unanswered by the
//     immediately following tool_result message gets a synthesized
//     `is_error` tool_result message appended ("[Tool use interrupted]");
//   - a tool_result whose tool_use is absent (orphan) is dropped; if that
//     empties a message, a placeholder text block keeps the message non-empty;
//   - duplicate tool_result ids are dropped.
func EnsurePairing(msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, 0, len(msgs))
	seenUse := map[string]struct{}{}
	seenResult := map[string]struct{}{}
	// tool_use ids that still need a result by the time we finish an assistant
	// message; resolved by the next tool_result message or answered synthetically.
	var pending []string

	flushPending := func() {
		if len(pending) == 0 {
			return
		}
		msg := provider.Message{Role: provider.RoleUser}
		for _, id := range pending {
			msg.Content = append(msg.Content, provider.Block{
				Type:         provider.BlockToolResult,
				ToolResultID: id,
				ToolContent:  "[Tool use interrupted]",
				IsError:      true,
			})
			seenResult[id] = struct{}{}
		}
		pending = nil
		out = append(out, msg)
	}

	for _, m := range msgs {
		if m.Role == provider.RoleAssistant {
			// A new assistant turn answers nothing; any previously-pending tool_use
			// from an earlier assistant message must be closed off first.
			flushPending()
			var content []provider.Block
			for _, b := range m.Content {
				if b.Type == provider.BlockToolUse {
					if _, dup := seenUse[b.ToolUseID]; dup {
						continue
					}
					seenUse[b.ToolUseID] = struct{}{}
					pending = append(pending, b.ToolUseID)
				}
				content = append(content, b)
			}
			m.Content = ensureNonEmpty(content)
			out = append(out, m)
			continue
		}

		// User (or other) message: resolve pending tool_use ids with real results,
		// drop orphans/duplicates, then synthesize whatever is still unanswered.
		var content []provider.Block
		for _, b := range m.Content {
			if b.Type == provider.BlockToolResult {
				if _, dup := seenResult[b.ToolResultID]; dup {
					continue
				}
				if !isPending(pending, b.ToolResultID) {
					continue // orphan: no matching tool_use
				}
				seenResult[b.ToolResultID] = struct{}{}
				pending = removePending(pending, b.ToolResultID)
			}
			content = append(content, b)
		}
		m.Content = ensureNonEmpty(content)
		out = append(out, m)
		flushPending()
	}
	flushPending()
	return out
}

// ensureNonEmpty returns content, substituting a placeholder text block when the
// repair stripped everything (a message with no content is itself invalid).
func ensureNonEmpty(content []provider.Block) []provider.Block {
	if len(content) == 0 {
		return []provider.Block{{Type: provider.BlockText, Text: "[Orphaned tool result removed]"}}
	}
	return content
}

func isPending(pending []string, id string) bool {
	for _, p := range pending {
		if p == id {
			return true
		}
	}
	return false
}

func removePending(pending []string, id string) []string {
	out := pending[:0]
	for _, p := range pending {
		if p != id {
			out = append(out, p)
		}
	}
	return out
}
