// Package contextmgmt implements context-management (design D11): online
// compression of the in-context short-term memory so a session stays within
// the model's context budget. It is distinct from offline dreaming: it governs
// only the current session's context and never writes long-term memory.
package contextmgmt

import (
	"nowhere-agent/internal/provider"
)

// Policy configures when and how to compress.
type Policy struct {
	// MaxTokens is the context budget for the window.
	MaxTokens int
	// Threshold is the fraction of MaxTokens at which compression triggers
	// (e.g. 0.8). Compression runs when estimated tokens exceed it.
	Threshold float64
	// KeepRecent is how many recent rounds are always preserved verbatim. A
	// round is an assistant message plus the tool_result answers to its tool_use
	// blocks (design D2): keeping whole rounds — not a raw message count — is
	// what guarantees a tool_use is never severed from its tool_result.
	KeepRecent int
}

// DefaultPolicy returns a sensible default.
func DefaultPolicy() Policy {
	return Policy{MaxTokens: 100_000, Threshold: 0.8, KeepRecent: 6}
}

// estimateTokens approximates token count for a message set (~4 chars/token).
func estimateTokens(msgs []provider.Message) int {
	total := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			total += len(b.Text) + len(b.Thinking) + len(b.ToolContent)
			for k, v := range b.ToolInput {
				total += len(k) + 8
				_ = v
			}
		}
	}
	return total / 4
}

// EstimateTokens exposes the approximate token count of a message set, so the
// agent loop can size its working view against the model's context window
// (design D5) using the same heuristic compression triggers on.
func EstimateTokens(msgs []provider.Message) int {
	return estimateTokens(msgs)
}

// ShouldCompress reports whether the history exceeds the trigger threshold.
func ShouldCompress(history []provider.Message, p Policy) bool {
	if p.MaxTokens <= 0 || p.Threshold <= 0 {
		return false
	}
	return estimateTokens(history) > int(float64(p.MaxTokens)*p.Threshold)
}

// Compressor summarizes dropped history. Implemented by an LLM caller or a
// simple heuristic. It must NOT write to long-term memory.
type Compressor interface {
	Summarize(dropped []provider.Message) (string, error)
}

// Compress reduces history when over threshold: older rounds are summarized
// into a single system-anchored summary message and the most recent KeepRecent
// rounds are kept verbatim (sliding window over rounds, not messages). The
// result is passed through EnsurePairing so the split can never leave an
// unpaired tool_use/tool_result. If under threshold, history is returned
// unchanged.
func Compress(history []provider.Message, p Policy, c Compressor) ([]provider.Message, error) {
	if !ShouldCompress(history, p) {
		return history, nil
	}
	keep := p.KeepRecent
	if keep < 0 {
		keep = 0
	}

	rounds := groupRounds(history)
	if keep >= len(rounds) {
		return history, nil
	}

	// Split on a round boundary: everything before the first kept round is
	// summarized, the kept rounds pass through verbatim.
	splitIdx := rounds[len(rounds)-keep].start
	if keep == 0 {
		splitIdx = len(history)
	}
	dropped := history[:splitIdx]
	recent := history[splitIdx:]

	summary, err := c.Summarize(dropped)
	if err != nil {
		return nil, err
	}

	summaryMsg := provider.TextMessage(provider.RoleUser,
		"[Earlier conversation summarized]\n"+summary)
	out := make([]provider.Message, 0, len(recent)+1)
	out = append(out, summaryMsg)
	out = append(out, recent...)
	return EnsurePairing(out), nil
}
