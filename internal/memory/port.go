// Package memory implements the memory capability (design D5): read/write-split
// long-term memory behind MemoryPort. The agent loop reads online (fast,
// cacheable); the dreaming worker is the only writer. Short-term memory is the
// in-context conversation and does NOT go through this port.
package memory

import (
	"context"
	"errors"
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

	// RecallSince returns non-deprecated memories in scope created after `since`,
	// optionally restricted to `kinds` (empty = all kinds), ranked by relevance to
	// `query` (empty query = most recent first). It is the incremental-injection
	// read: `since` is the session's memory_injected_at watermark; the zero time
	// means no lower bound (first-turn full recall). The recall_memory tool also
	// uses it with a zero `since` for full relevance recall of chosen kinds.
	RecallSince(ctx context.Context, since time.Time, query string, scopes []identity.ScopeRef, kinds []Kind, limit int) ([]Memory, error)

	// ---- write side (offline, called ONLY by the dreaming worker) ----

	// Store persists a new memory.
	Store(ctx context.Context, m Memory) (Memory, error)
	// Update revises an existing memory's content in place, keeping its id and
	// CreatedAt and bumping UpdatedAt. It returns ErrNotFound when no memory has
	// that id.
	//
	// It CLEARS the stored embedding. An embedding describes the text it was
	// derived from; leaving it attached to rewritten content would make
	// RecallVector rank the memory by what it used to say. Callers that hold an
	// embedder re-index after updating.
	Update(ctx context.Context, id, content string) error
	// Deprecate marks a memory superseded (excluded from recall).
	Deprecate(ctx context.Context, id string) error
	// Forget permanently deletes a memory (GDPR erasure).
	Forget(ctx context.Context, id string) error
	// PurgeDeprecated permanently deletes memories deprecated before the cutoff,
	// returning how many were removed. Deprecation is reversible by design, so
	// something has to close the window — without this the store grows without
	// bound in rows nothing can ever recall.
	PurgeDeprecated(ctx context.Context, before time.Time) (int, error)
	// ListByScope returns all memories in a scope (dreaming scans with this).
	ListByScope(ctx context.Context, scope identity.ScopeRef) ([]Memory, error)
	// GetByID returns one memory, or ErrNotFound. It exists so a caller about
	// to deprecate or forget can check the memory's SCOPE against its own
	// entitlement first: Deprecate and Forget take a bare id, so without this
	// a team administrator holding a valid uuid could reach into another
	// team's memories.
	GetByID(ctx context.Context, id string) (Memory, error)
}

// ErrNotFound is returned by GetByID when no memory has that id.
var ErrNotFound = errors.New("memory not found")
