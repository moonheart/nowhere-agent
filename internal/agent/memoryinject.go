package agent

import (
	"context"

	"nowhere-agent/internal/provider"
)

// MemoryInjector appends newly-surfaced long-term memories to the outgoing view
// as extra messages (incremental injection, capability K / context-mgmt). It
// runs at send time inside Loop.attempt, on a local copy of the view — the
// appended messages are NEVER part of `produced` and never persisted, so the
// durable conversation history stays clean (real user/assistant/tool only) and
// append-only (so the LLM prompt prefix stays byte-stable for caching).
//
// The injector owns the per-session watermark: it returns nil when there is
// nothing new to surface (no message appended, prefix unchanged), and a single
// user-role background-context message when memories were created since the
// last injection.
type MemoryInjector interface {
	// Inject returns extra messages to append to the tail of the outgoing view
	// (after the current user input). Returns nil when there is nothing new.
	Inject(ctx context.Context, sessionID string, view []provider.Message) ([]provider.Message, error)
}
