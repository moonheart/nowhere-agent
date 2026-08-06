package skill

import (
	"context"
	"sort"
	"sync"

	"nowhere-agent/internal/identity"
)

// memStore is an in-memory Reader/Writer for tests (the production store is
// PGStore). It implements the same user>team>system priority resolution.
type memStore struct {
	mu     sync.Mutex
	skills []*Skill
}

func newMemStore() *memStore { return &memStore{} }

func (m *memStore) Put(_ context.Context, sk Skill, _ string) (Skill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, cur := range m.skills {
		if cur.Name == sk.Name && scopeRefEqual(cur.Scope, sk.Scope) {
			cur.Body = sk.Body
			cur.Description = sk.Description
			cur.Resources = sk.Resources
			cur.Scripts = sk.Scripts
			cur.Version++
			return *cur, nil
		}
	}
	sk.ID = sk.Name + "|" + scopeRefKey(sk.Scope)
	sk.Version = 1
	cp := sk
	m.skills = append(m.skills, &cp)
	return sk, nil
}

func (m *memStore) Get(_ context.Context, name string, scopes []identity.ScopeRef) (Skill, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, scope := range scopes {
		for _, cur := range m.skills {
			if cur.Name == name && scopeRefEqual(cur.Scope, scope) {
				return *cur, true, nil
			}
		}
	}
	return Skill{}, false, nil
}

func (m *memStore) List(_ context.Context, scopes []identity.ScopeRef) ([]L0, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	best := map[string]L0{}
	bestRank := map[string]int{}
	for _, cur := range m.skills {
		r := rankOf(cur.Scope, scopes)
		if r < 0 {
			continue
		}
		if cur2, ok := bestRank[cur.Name]; !ok || r < cur2 {
			best[cur.Name] = L0{Name: cur.Name, Description: cur.Description, Scripts: scriptNames(cur.Scripts)}
			bestRank[cur.Name] = r
		}
	}
	out := make([]L0, 0, len(best))
	for _, l := range best {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func rankOf(sc identity.ScopeRef, scopes []identity.ScopeRef) int {
	for i, s := range scopes {
		if scopeRefEqual(s, sc) {
			return i
		}
	}
	return -1
}

func scopeRefEqual(a, b identity.ScopeRef) bool {
	return a.Scope == b.Scope && a.UserID == b.UserID && a.TeamID == b.TeamID
}

func scopeRefKey(sc identity.ScopeRef) string {
	return string(sc.Scope) + "|" + sc.UserID + "|" + sc.TeamID
}
