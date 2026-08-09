package agentdef

import (
	"context"
	"log/slog"
	"sort"

	"nowhere-agent/internal/identity"
)

// VisibleLister supplies the authored definitions visible in a set of scopes
// (PGStore in production, fakes in tests).
type VisibleLister interface {
	ListVisible(ctx context.Context, scopes []identity.ScopeRef) ([]StoredDef, error)
}

// Resolver merges the in-memory built-in definitions (Store) with durable
// authored definitions (a VisibleLister, typically PGStore) into one resolution
// view: built-ins as the base, authored definitions overlaid in caller scope-
// priority order (user > team > system), so an authored def shadows a built-in
// of the same name. It is the spawn path's single definition source.
//
// The lister is consulted per call (definitions are few per scope and the
// query is trivial next to a child agent run), so authored definitions take
// effect without a restart and multi-instance deployments never serve stale
// defs. A lister failure degrades to built-ins only with a logged warning —
// spawns never hard-fail on a store outage.
type Resolver struct {
	base *Store
	pg   VisibleLister
}

// NewResolver creates a Resolver over the built-in store and an optional
// authored-definition lister (nil → built-ins only, e.g. tests/dev without a
// database).
func NewResolver(base *Store, pg VisibleLister) *Resolver {
	return &Resolver{base: base, pg: pg}
}

// Resolve returns the definition for a requested type from the merged view,
// with the same exact-then-normalized matching and candidate-list errors as
// the in-memory Store.
func (r *Resolver) Resolve(ctx context.Context, name string, scopes []identity.ScopeRef) (AgentDef, error) {
	return r.base.resolveFrom(r.merged(ctx, scopes), name)
}

// Available lists the agent type names visible in the given scopes, sorted,
// from the merged view.
func (r *Resolver) Available(ctx context.Context, scopes []identity.ScopeRef) []string {
	return sortedKeys(r.merged(ctx, scopes))
}

// merged computes the resolved definition per name: built-ins as the base,
// then authored definitions overlaid in caller priority order (first/highest
// scope wins per name).
func (r *Resolver) merged(ctx context.Context, scopes []identity.ScopeRef) map[string]AgentDef {
	out := r.base.visibleLocked2()
	if r.pg == nil {
		return out
	}
	defs, err := r.pg.ListVisible(ctx, scopes)
	if err != nil {
		slog.Warn("agentdef: authored-definition store unavailable; resolving built-ins only", "err", err)
		return out
	}
	byScope := map[string][]AgentDef{}
	for _, sd := range defs {
		key := scopeKeyOf(sd.Scope)
		byScope[key] = append(byScope[key], sd.AgentDef)
	}
	seen := map[string]bool{}
	for _, scope := range scopes {
		for _, d := range byScope[scopeKeyOf(scope)] {
			if seen[d.Name] {
				continue
			}
			out[d.Name] = d
			seen[d.Name] = true
		}
	}
	return out
}

// BoundResolver adapts a Resolver to ctx-free resolver interfaces (e.g.
// schedule.DefResolver), capturing the context once.
type BoundResolver struct {
	r   *Resolver
	ctx context.Context
}

// Bound returns a ctx-bound view of the Resolver.
func (r *Resolver) Bound(ctx context.Context) BoundResolver {
	return BoundResolver{r: r, ctx: ctx}
}

// Resolve implements schedule.DefResolver.
func (b BoundResolver) Resolve(name string, scopes []identity.ScopeRef) (AgentDef, error) {
	return b.r.Resolve(b.ctx, name, scopes)
}

func scopeKeyOf(s identity.ScopeRef) string {
	return string(s.Scope) + "/" + s.UserID + "/" + s.TeamID
}

// visibleLocked2 returns a copy of the built-in definitions (the merged view's
// base layer).
func (s *Store) visibleLocked2() map[string]AgentDef {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]AgentDef{}
	for name, d := range s.builtins {
		out[name] = d
	}
	// Authored in-memory defs (tests; Store.Put) behave as the highest-priority
	// layer, matching Store.Resolve's own semantics.
	for _, d := range s.defs {
		out[d.Name] = d
	}
	return out
}

// resolveFrom applies the exact-then-normalized matching against a precomputed
// visibility map (shared by Store.Resolve and Resolver.Resolve).
func (s *Store) resolveFrom(visible map[string]AgentDef, name string) (AgentDef, error) {
	if d, ok := visible[name]; ok {
		return d, nil
	}
	target := normalize(name)
	var matches []string
	for n := range visible {
		if normalize(n) == target {
			matches = append(matches, n)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 1:
		return visible[matches[0]], nil
	case 0:
		return AgentDef{}, errUnknownType(name, visible)
	default:
		return AgentDef{}, errAmbiguousType(name, matches)
	}
}
