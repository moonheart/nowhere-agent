package skill

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"nowhere-agent/internal/identity"
)

// Store holds skills across scopes and resolves them with priority override
// (user > team > system) and versioning.
type Store struct {
	mu     sync.Mutex
	skills map[string]*Skill // id
}

// NewStore creates an empty Store.
func NewStore() *Store {
	return &Store{skills: map[string]*Skill{}}
}

// Put inserts or updates a skill. New skills start at Version 1; updates to an
// existing (name, scope) increment the version and flag dependents for review.
func (s *Store) Put(_ context.Context, sk Skill) (Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Find existing skill with same name+scope.
	var existing *Skill
	for _, cur := range s.skills {
		if cur.Name == sk.Name && scopeEqual(cur.Scope, sk.Scope) {
			existing = cur
			break
		}
	}

	now := time.Now()
	if existing == nil {
		sk.ID = uuid.NewString()
		sk.Version = 1
		sk.CreatedAt = now
		sk.UpdatedAt = now
		s.skills[sk.ID] = &sk
	} else {
		existing.Body = sk.Body
		existing.Description = sk.Description
		existing.Resources = sk.Resources
		existing.Scripts = sk.Scripts
		existing.Version++
		existing.UpdatedAt = now
		sk = *existing
		// Flag any overrides of this skill for review (D16).
		s.flagOverridesForReviewLocked(sk.Name, sk.Scope, sk.Version)
	}
	return sk, nil
}

// flagOverridesForReviewLocked marks higher-scope overrides of (name, updated
// scope) as needing review. Caller holds s.mu.
func (s *Store) flagOverridesForReviewLocked(name string, updated identity.ScopeRef, newVersion int) {
	for _, cur := range s.skills {
		if cur.Name != name || scopeEqual(cur.Scope, updated) {
			continue
		}
		// A higher-priority scope overriding the updated one.
		if overrides(cur.Scope, updated) && cur.OverridesVersion < newVersion {
			cur.NeedsReview = true
		}
	}
}

// Get resolves a skill by name with scope priority: user > team > system.
// The scopes are searched in the caller-provided priority order.
func (s *Store) Get(_ context.Context, name string, scopes []identity.ScopeRef) (Skill, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Iterate scopes in priority order; first match wins.
	for _, scope := range scopes {
		var best *Skill
		for _, cur := range s.skills {
			if cur.Name == name && scopeEqual(cur.Scope, scope) {
				if best == nil || cur.Version > best.Version {
					best = cur
				}
			}
		}
		if best != nil {
			return *best, true
		}
	}
	return Skill{}, false
}

// List returns L0 metadata for all skills visible in the given scopes,
// deduplicated by name with priority override applied.
func (s *Store) List(_ context.Context, scopes []identity.ScopeRef) []L0 {
	s.mu.Lock()
	defer s.mu.Unlock()
	resolved := map[string]Skill{}
	for _, scope := range scopes {
		for _, cur := range s.skills {
			if !scopeEqual(cur.Scope, scope) {
				continue
			}
			if existing, ok := resolved[cur.Name]; !ok || cur.Version > existing.Version {
				resolved[cur.Name] = *cur
			}
		}
	}
	out := make([]L0, 0, len(resolved))
	for _, sk := range resolved {
		out = append(out, L0{Name: sk.Name, Description: sk.Description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Delete removes a skill by id.
func (s *Store) Delete(_ context.Context, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.skills, id)
}

// overrides reports whether scope a has higher priority than scope b
// (user > team > system).
func overrides(a, b identity.ScopeRef) bool {
	rank := func(s identity.Scope) int {
		switch s {
		case identity.ScopeUser:
			return 3
		case identity.ScopeTeam:
			return 2
		case identity.ScopeSystem:
			return 1
		}
		return 0
	}
	return rank(a.Scope) > rank(b.Scope)
}

func scopeEqual(a, b identity.ScopeRef) bool {
	return a.Scope == b.Scope && a.UserID == b.UserID && a.TeamID == b.TeamID
}
