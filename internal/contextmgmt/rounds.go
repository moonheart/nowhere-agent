package contextmgmt

import (
	"nowhere-agent/internal/provider"
)

// round is a group of consecutive messages that must be cut together so a
// tool_use is never separated from its tool_result. A round is typically an
// assistant message (carrying tool_use blocks) followed by the user-role
// message(s) carrying the answering tool_result blocks; a plain user/assistant
// text turn is a single-message round.
//
// The provider API contract requires every tool_use to be answered by a
// tool_result in the immediately following message, so compression must drop
// whole rounds — never a raw message count (design D2).
type round struct {
	start int // index of the first message in the group (inclusive)
	end   int // index one past the last message in the group (exclusive)
}

// groupRounds partitions msgs into cut-safe rounds. It is conservative: any
// message it cannot confidently pair stays adjacent to what follows, so the
// returned groups never split inside a tool exchange.
func groupRounds(msgs []provider.Message) []round {
	var rounds []round
	for i := 0; i < len(msgs); {
		r := round{start: i, end: i + 1}
		m := msgs[i]
		if m.Role == provider.RoleAssistant {
			// Collect the immediately-following tool_result messages that answer
			// this assistant message's tool_use blocks.
			ids := toolUseIDs(m)
			if len(ids) > 0 {
				answered := 0
				for j := i + 1; j < len(msgs) && answered < len(ids); j++ {
					if !isToolResultMsg(msgs[j]) {
						break
					}
					r.end = j + 1
					answered += countAnswered(msgs[j], ids)
				}
			}
		}
		rounds = append(rounds, r)
		i = r.end
	}
	return rounds
}

// toolUseIDs returns the set of tool_use ids in a message.
func toolUseIDs(m provider.Message) map[string]struct{} {
	ids := map[string]struct{}{}
	for _, b := range m.Content {
		if b.Type == provider.BlockToolUse && b.ToolUseID != "" {
			ids[b.ToolUseID] = struct{}{}
		}
	}
	return ids
}

// isToolResultMsg reports whether a message carries at least one tool_result.
func isToolResultMsg(m provider.Message) bool {
	for _, b := range m.Content {
		if b.Type == provider.BlockToolResult {
			return true
		}
	}
	return false
}

// countAnswered returns how many of the wanted tool_use ids this message's
// tool_result blocks answer.
func countAnswered(m provider.Message, want map[string]struct{}) int {
	n := 0
	for _, b := range m.Content {
		if b.Type == provider.BlockToolResult {
			if _, ok := want[b.ToolResultID]; ok {
				n++
			}
		}
	}
	return n
}

// DropOldestRound removes the oldest round from msgs and repairs pairing, so a
// reactive context-overflow retry can shrink the working view without severing
// a tool pair (design D7). Returns the shortened list; if msgs holds a single
// round (nothing safe to drop), it returns msgs unchanged with ok=false.
func DropOldestRound(msgs []provider.Message) (out []provider.Message, ok bool) {
	rounds := groupRounds(msgs)
	if len(rounds) <= 1 {
		return msgs, false
	}
	rest := msgs[rounds[0].end:]
	return EnsurePairing(rest), true
}
