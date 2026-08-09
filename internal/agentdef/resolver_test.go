package agentdef

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nowhere-agent/internal/identity"
)

// fakeLister mimics PGStore.ListVisible: only defs exactly matching one of the
// requested scopes are visible.
type fakeLister struct {
	defs []StoredDef
	err  error
}

func (f fakeLister) ListVisible(_ context.Context, scopes []identity.ScopeRef) ([]StoredDef, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []StoredDef
	for _, d := range f.defs {
		for _, sc := range scopes {
			if scopeEqual(d.Scope, sc) {
				out = append(out, d)
				break
			}
		}
	}
	return out, nil
}

func stored(d AgentDef) StoredDef { return StoredDef{AgentDef: d} }

// TestResolverScopeOverride: user-scope PG def beats team beats system beats
// the built-in of the same name.
func TestResolverScopeOverride(t *testing.T) {
	base := NewStore()
	pg := fakeLister{defs: []StoredDef{
		stored(AgentDef{Name: GeneralPurpose, System: "team version", Scope: identity.TeamScope("t1")}),
		stored(AgentDef{Name: GeneralPurpose, System: "user version", Scope: identity.UserScope("u1")}),
	}}
	r := NewResolver(base, pg)
	scopes := []identity.ScopeRef{identity.UserScope("u1"), identity.TeamScope("t1"), identity.SystemScope()}

	d, err := r.Resolve(context.Background(), GeneralPurpose, scopes)
	if err != nil {
		t.Fatal(err)
	}
	if d.System != "user version" {
		t.Fatalf("user scope must win, got %q", d.System)
	}

	d, err = r.Resolve(context.Background(), GeneralPurpose, []identity.ScopeRef{identity.TeamScope("t1"), identity.SystemScope()})
	if err != nil {
		t.Fatal(err)
	}
	if d.System != "team version" {
		t.Fatalf("team scope must win without the user def, got %q", d.System)
	}

	d, err = r.Resolve(context.Background(), GeneralPurpose, []identity.ScopeRef{identity.SystemScope()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.System, "subagent of nowhere-agent") {
		t.Fatalf("built-in must remain when no authored def is visible, got %q", d.System)
	}
}

// TestResolverAuthoredShadowsBuiltin: an authored system-scope def shadows the
// built-in for its scope, while other scopes still see the built-in.
func TestResolverAuthoredShadowsBuiltin(t *testing.T) {
	pg := fakeLister{defs: []StoredDef{
		stored(AgentDef{Name: GeneralPurpose, System: "authored system version", Scope: identity.SystemScope()}),
	}}
	r := NewResolver(NewStore(), pg)

	d, err := r.Resolve(context.Background(), GeneralPurpose, []identity.ScopeRef{identity.SystemScope()})
	if err != nil {
		t.Fatal(err)
	}
	if d.System != "authored system version" {
		t.Fatalf("authored def must shadow the built-in, got %q", d.System)
	}
}

// TestResolverStoreOutageDegradesToBuiltins pins the degradation contract: a
// lister failure resolves built-ins only, never an error.
func TestResolverStoreOutageDegradesToBuiltins(t *testing.T) {
	pg := fakeLister{
		defs: []StoredDef{stored(AgentDef{Name: "authored", System: "x", Scope: identity.SystemScope()})},
		err:  errors.New("db down"),
	}
	r := NewResolver(NewStore(), pg)

	if _, err := r.Resolve(context.Background(), "authored", []identity.ScopeRef{identity.SystemScope()}); err == nil {
		t.Fatal("authored def must be invisible during an outage")
	}
	if _, err := r.Resolve(context.Background(), GeneralPurpose, []identity.ScopeRef{identity.SystemScope()}); err != nil {
		t.Fatalf("built-in must still resolve during an outage: %v", err)
	}
	if got := r.Available(context.Background(), []identity.ScopeRef{identity.SystemScope()}); len(got) != 1 || got[0] != GeneralPurpose {
		t.Fatalf("available during outage = %v, want built-ins only", got)
	}
}

// TestResolverNormalizedMatchAcrossPG: normalized matching applies to authored
// definitions too.
func TestResolverNormalizedMatchAcrossPG(t *testing.T) {
	pg := fakeLister{defs: []StoredDef{
		stored(AgentDef{Name: "code-reviewer", System: "x", Scope: identity.SystemScope()}),
	}}
	r := NewResolver(NewStore(), pg)

	d, err := r.Resolve(context.Background(), "Code Reviewer", []identity.ScopeRef{identity.SystemScope()})
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "code-reviewer" {
		t.Fatalf("normalized match: %+v", d)
	}
}

// TestResolverAvailableMerges: Available lists built-ins plus authored names.
func TestResolverAvailableMerges(t *testing.T) {
	pg := fakeLister{defs: []StoredDef{
		stored(AgentDef{Name: "zzz-custom", System: "x", Scope: identity.UserScope("u1")}),
	}}
	r := NewResolver(NewStore(), pg)

	got := r.Available(context.Background(), []identity.ScopeRef{identity.UserScope("u1"), identity.SystemScope()})
	if len(got) != 2 || got[0] != GeneralPurpose || got[1] != "zzz-custom" {
		t.Fatalf("available = %v", got)
	}
}
