package agent

import (
	"nowhere-agent/internal/provider"
)

// IsRecoverableLength classifies a max_tokens stop as context pressure rather
// than a genuine output-limit stop (change durable-run-accounting, overflow
// recovery). The classification compares the response's actual output usage
// against the intended output cap — the caller-supplied maxTokens before any
// context clamping — so a context-clamped request that returns a handful of
// reasoning tokens against a large intent classifies recoverable, while an
// explicit cap fully used is a genuine stop. No context-percentage heuristics.
//
// desiredMaxOutput <= 0 (no cap configured) or a nil usage (the adapter
// reported nothing) cannot be classified and returns false, preserving the
// legacy truncation-error behavior.
func IsRecoverableLength(usage *provider.Usage, desiredMaxOutput int) bool {
	if desiredMaxOutput <= 0 {
		return false
	}
	if usage == nil {
		return false
	}
	// Reaching the intended cap is a genuine output-limit stop.
	if usage.OutputTokens >= desiredMaxOutput {
		return false
	}
	// Stopped below the intended cap: context pressure or provider-side
	// truncation — recoverable.
	return true
}
