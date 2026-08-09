package agentdef

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"nowhere-agent/internal/identity"
)

// Store holds subagent definitions across scopes and resolves a requested type
// with priority override (user > team > system), falling back to the built-in
// definitions. It mirrors the skill store's scope model.
type Store struct {
	mu       sync.Mutex
	defs     []AgentDef          // authored, scoped definitions
	builtins map[string]AgentDef // shipped in code (lowest priority)
}

// NewStore creates a Store seeded with the built-in definitions.
func NewStore() *Store {
	b := map[string]AgentDef{}
	for _, d := range Builtins() {
		b[d.Name] = d
	}
	return &Store{builtins: b}
}

// Put inserts or replaces a definition at its scope (by name+scope).
func (s *Store) Put(d AgentDef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, cur := range s.defs {
		if cur.Name == d.Name && scopeEqual(cur.Scope, d.Scope) {
			s.defs[i] = d
			return
		}
	}
	s.defs = append(s.defs, d)
}

// Resolve returns the definition for a requested type. It matches exactly
// first, then by normalized form (case-insensitive, ignoring spaces, dashes,
// and underscores). An ambiguous normalized match, or no match, returns an
// error whose message lists the candidates / available types.
//
// Scopes are searched in the caller-provided priority order (highest first);
// authored definitions override built-ins.
func (s *Store) Resolve(name string, scopes []identity.ScopeRef) (AgentDef, error) {
	s.mu.Lock()
	visible := s.visibleLocked(scopes)
	s.mu.Unlock()
	return s.resolveFrom(visible, name)
}

// Available lists the agent type names visible in the given scopes, sorted.
func (s *Store) Available(scopes []identity.ScopeRef) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sortedKeys(s.visibleLocked(scopes))
}

// visibleLocked computes the resolved definition per name: built-ins as the
// base, then authored scoped definitions overlaid in caller priority order
// (first/highest scope wins per name). Caller holds s.mu.
func (s *Store) visibleLocked(scopes []identity.ScopeRef) map[string]AgentDef {
	out := map[string]AgentDef{}
	for name, d := range s.builtins {
		out[name] = d
	}
	seen := map[string]bool{}
	for _, scope := range scopes {
		for _, d := range s.defs {
			if !scopeEqual(d.Scope, scope) || seen[d.Name] {
				continue
			}
			out[d.Name] = d
			seen[d.Name] = true
		}
	}
	return out
}

func sortedKeys(m map[string]AgentDef) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// normalize folds a type name for tolerant matching: lower-case, dropping
// spaces, dashes, and underscores.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func errUnknownType(name string, visible map[string]AgentDef) error {
	return fmt.Errorf("unknown agent type %q; available: %s", name, strings.Join(sortedKeys(visible), ", "))
}

func errAmbiguousType(name string, matches []string) error {
	return fmt.Errorf("ambiguous agent type %q — matches %s; use an exact name", name, strings.Join(matches, ", "))
}

func scopeEqual(a, b identity.ScopeRef) bool {
	return a.Scope == b.Scope && a.UserID == b.UserID && a.TeamID == b.TeamID
}
