// Package memory implements the memory capability (design D5): read/write-split
// long-term memory behind MemoryPort. The agent loop reads online (fast,
// cacheable); the dreaming worker is the only writer. Short-term memory is the
// in-context conversation and does NOT go through this port.
package memory

import (
	"context"
	"time"

	"nowhere-agent/internal/identity"
)

// Kind classifies a memory.
type Kind string

const (
	// KindFact is a durable fact about the user/team.
	KindFact Kind = "fact"
	// KindPreference is a user/team preference.
	KindPreference Kind = "preference"
	// KindInsight is a higher-level cross-session pattern (from reflection).
	KindInsight Kind = "insight"
	// KindSummary is a compressed episode summary.
	KindSummary Kind = "summary"
)

// Memory is a unit of long-term memory, scoped for isolation.
type Memory struct {
	ID        string
	Scope     identity.ScopeRef
	Kind      Kind
	Content   string
	// Embedding is the vector for semantic recall (nil until indexed).
	Embedding []float32
	// Deprecated marks a memory superseded during reorganization; it is not
	// recalled but kept for audit until Forgotten.
	Deprecated bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Port is the read/write-split long-term memory interface (design D5).
type Port interface {
	// ---- read side (online, called by the agent loop; must be fast) ----

	// Recall returns memories relevant to the query within the given scopes.
	Recall(ctx context.Context, query string, scopes []identity.ScopeRef, limit int) ([]Memory, error)

	// ---- write side (offline, called ONLY by the dreaming worker) ----

	// Store persists a new memory.
	Store(ctx context.Context, m Memory) (Memory, error)
	// Deprecate marks a memory superseded (excluded from recall).
	Deprecate(ctx context.Context, id string) error
	// Forget permanently deletes a memory (GDPR erasure).
	Forget(ctx context.Context, id string) error
	// ListByScope returns all memories in a scope (dreaming scans with this).
	ListByScope(ctx context.Context, scope identity.ScopeRef) ([]Memory, error)
}
