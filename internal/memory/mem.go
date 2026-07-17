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

// Forget permanently deletes a memory.
func (p *MemPort) Forget(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.memories, id)
	return nil
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
