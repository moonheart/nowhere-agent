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
// have text is used. Empty output yields an explicit, non-error marker.
func collapse(msgs []provider.Message) toolruntime.Result {
	if txt := lastAssistantText(msgs); txt != "" {
		return toolruntime.Result{Content: txt}
	}
	return toolruntime.Result{Content: noOutput}
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
