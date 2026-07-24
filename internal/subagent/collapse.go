package subagent

import (
	"strings"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// noOutput is returned when a subagent produced no assistant text at all.
const noOutput = "(subagent produced no output)"

// collapse reduces a subagent's produced messages to a single tool result: the
// text of its final assistant message. If the final assistant message has no
// text (it ended on a tool call), the most recent assistant message that does
// have text is used. Empty output yields an explicit, non-error marker. The
// child's renderable content is attached as Nested so the UI can replay the
// sub-conversation (it never goes back to the model).
func collapse(msgs []provider.Message) toolruntime.Result {
	res := toolruntime.Result{Content: noOutput, Nested: contentBlocks(msgs)}
	if txt := lastAssistantText(msgs); txt != "" {
		res.Content = txt
	}
	return res
}

// contentBlocks flattens a subagent's produced messages into the renderable
// block sequence: assistant thinking/text/tool_use blocks and the tool_result
// blocks that answer them. The initial user prompt and any repair messages are
// dropped — they aren't part of what the subagent visibly "did".
func contentBlocks(msgs []provider.Message) []provider.Block {
	var out []provider.Block
	for _, m := range msgs {
		if m.Role != provider.RoleAssistant {
			// Keep tool_result blocks (they pair with a prior tool_use); drop
			// plain user text (the launch prompt, pairing repairs).
			for _, blk := range m.Content {
				if blk.Type == provider.BlockToolResult {
					out = append(out, blk)
				}
			}
			continue
		}
		for _, blk := range m.Content {
			switch blk.Type {
			case provider.BlockThinking, provider.BlockText, provider.BlockToolUse:
				out = append(out, blk)
			}
		}
	}
	return out
}

// lastAssistantText walks messages newest-first and returns the joined text
// blocks of the most recent assistant message that has any text.
func lastAssistantText(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != provider.RoleAssistant {
			continue
		}
		var b strings.Builder
		for _, blk := range msgs[i].Content {
			if blk.Type == provider.BlockText && blk.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(blk.Text)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	return ""
}
