package memory

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"nowhere-agent/internal/identity"
)

// MemPort is an in-memory Port. Recall does cosine similarity over embeddings
// when present, falling back to keyword overlap. Suitable for tests and for
// developing consumers before the Postgres+vector backend is wired.
type MemPort struct {
	mu       sync.Mutex
	memories map[string]*Memory
}

// NewMemPort creates an empty in-memory Port.
func NewMemPort() *MemPort {
	return &MemPort{memories: map[string]*Memory{}}
}

// Store persists a memory, assigning ID and timestamps.
func (p *MemPort) Store(_ context.Context, m Memory) (Memory, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m.ID == "" {
		m.ID = uuid.NewString()
	}
	now := time.Now()
	m.CreatedAt = now
	m.UpdatedAt = now
	p.memories[m.ID] = &m
	return m, nil
}

// Backdate sets a stored memory's CreatedAt (test helper): incremental-recall
// tests use it to order memories against a watermark deterministically, since
// Store would otherwise stamp now and tie the watermark's clock tick.
func (p *MemPort) Backdate(id string, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.memories[id]; ok {
		m.CreatedAt = at
	}
}

// Recall returns non-deprecated memories in scope, ranked by relevance.
func (p *MemPort) Recall(_ context.Context, query string, scopes []identity.ScopeRef, limit int) ([]Memory, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if limit <= 0 {
		limit = 10
	}
	type scored struct {
		m     Memory
		score float64
	}
	var results []scored
	for _, m := range p.memories {
		if m.Deprecated || !scopeIn(m.Scope, scopes) {
			continue
		}
		s := relevance(query, *m)
		// A non-empty query must actually match (PGPort's ts_rank > 0 floor):
		// without it, a query with no keyword overlap would return an arbitrary
		// page of unrelated memories as "matches".
		if strings.TrimSpace(query) != "" && s <= 0 {
			continue
		}
		results = append(results, scored{m: *m, score: s})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })
	if len(results) > limit {
		results = results[:limit]
	}
	out := make([]Memory, 0, len(results))
	for _, r := range results {
		out = append(out, r.m)
	}
	return out, nil
}

// RecallSince returns non-deprecated in-scope memories created after `since`,
// optionally filtered to `kinds`, ranked by relevance (or recency when the
// query is empty). A zero `since` disables the time lower bound.
func (p *MemPort) RecallSince(_ context.Context, since time.Time, query string, scopes []identity.ScopeRef, kinds []Kind, limit int) ([]Memory, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if limit <= 0 {
		limit = 10
	}
	type scored struct {
		m     Memory
		score float64
	}
	var results []scored
	for _, m := range p.memories {
		if m.Deprecated || !scopeIn(m.Scope, scopes) {
			continue
		}
		if !since.IsZero() && !m.CreatedAt.After(since) {
			continue
		}
		if len(kinds) > 0 && !kindIn(m.Kind, kinds) {
			continue
		}
		s := relevance(query, *m)
		// Same zero-relevance floor as PGPort: with a query, a memory with no
		// keyword overlap must not surface as a match.
		if strings.TrimSpace(query) != "" && s <= 0 {
			continue
		}
		results = append(results, scored{m: *m, score: s})
	}
	// With a query: relevance, ties broken by recency. Without: pure recency.
	sort.Slice(results, func(i, j int) bool {
		if query == "" || results[i].score == results[j].score {
			return results[i].m.CreatedAt.After(results[j].m.CreatedAt)
		}
		return results[i].score > results[j].score
	})
	if len(results) > limit {
		results = results[:limit]
	}
	out := make([]Memory, 0, len(results))
	for _, r := range results {
		out = append(out, r.m)
	}
	return out, nil
}

// kindIn reports whether k is among the allowed kinds.
func kindIn(k Kind, allowed []Kind) bool {
	for _, a := range allowed {
		if k == a {
			return true
		}
	}
	return false
}

// Deprecate marks a memory superseded.
func (p *MemPort) Deprecate(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.memories[id]; ok {
		m.Deprecated = true
		m.UpdatedAt = time.Now()
	}
	return nil
}

// Update rewrites a memory's content in place, clearing its embedding (which
// described the old text) and bumping UpdatedAt. Id and CreatedAt are kept.
func (p *MemPort) Update(_ context.Context, id, content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.memories[id]
	if !ok {
		return ErrNotFound
	}
	m.Content = content
	m.Embedding = nil
	m.UpdatedAt = time.Now()
	return nil
}

// Forget permanently deletes a memory.
func (p *MemPort) Forget(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.memories, id)
	return nil
}

// PurgeDeprecated deletes memories deprecated before the cutoff. As in PGPort,
// UpdatedAt dates the deprecation.
func (p *MemPort) PurgeDeprecated(_ context.Context, before time.Time) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for id, m := range p.memories {
		if m.Deprecated && m.UpdatedAt.Before(before) {
			delete(p.memories, id)
			n++
		}
	}
	return n, nil
}

// ListByScope returns all memories (incl. deprecated) in a scope.
func (p *MemPort) ListByScope(_ context.Context, scope identity.ScopeRef) ([]Memory, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Memory
	for _, m := range p.memories {
		if scopeEqual(m.Scope, scope) {
			out = append(out, *m)
		}
	}
	return out, nil
}

// GetByID returns one memory, or ErrNotFound.
func (p *MemPort) GetByID(_ context.Context, id string) (Memory, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.memories[id]
	if !ok {
		return Memory{}, ErrNotFound
	}
	return *m, nil
}

// scopeIn reports whether target is among the allowed scopes.
func scopeIn(target identity.ScopeRef, allowed []identity.ScopeRef) bool {
	for _, a := range allowed {
		if scopeEqual(target, a) {
			return true
		}
	}
	return false
}

func scopeEqual(a, b identity.ScopeRef) bool {
	return a.Scope == b.Scope && a.UserID == b.UserID && a.TeamID == b.TeamID
}

// relevance scores a memory against a query: cosine over embeddings if both
// present, else keyword overlap.
func relevance(query string, m Memory) float64 {
	if len(m.Embedding) > 0 {
		// Without a query embedding here, fall through to keyword for MemPort.
	}
	return keywordScore(query, m.Content)
}

func keywordScore(query, content string) float64 {
	q := strings.Fields(strings.ToLower(query))
	c := strings.ToLower(content)
	if len(q) == 0 {
		return 0
	}
	hits := 0
	for _, w := range q {
		if strings.Contains(c, w) {
			hits++
		}
	}
	return float64(hits) / float64(len(q))
}
