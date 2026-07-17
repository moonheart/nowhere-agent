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
	// KeepRecent is how many recent messages are always preserved verbatim.
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

// Compress reduces history when over threshold: older messages are summarized
// into a single system-anchored summary message and recent messages are kept
// verbatim (sliding-window). If under threshold, history is returned unchanged.
func Compress(history []provider.Message, p Policy, c Compressor) ([]provider.Message, error) {
	if !ShouldCompress(history, p) {
		return history, nil
	}
	keep := p.KeepRecent
	if keep < 0 {
		keep = 0
	}
	if keep >= len(history) {
		return history, nil
	}

	dropped := history[:len(history)-keep]
	recent := history[len(history)-keep:]

	summary, err := c.Summarize(dropped)
	if err != nil {
		return nil, err
	}

	summaryMsg := provider.TextMessage(provider.RoleUser,
		"[Earlier conversation summarized]\n"+summary)
	out := make([]provider.Message, 0, keep+1)
	out = append(out, summaryMsg)
	out = append(out, recent...)
	return out, nil
}
