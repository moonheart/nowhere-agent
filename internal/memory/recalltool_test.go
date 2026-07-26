package memory

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/identity"
)

func TestRecallToolDefaultsToSummaryInsight(t *testing.T) {
	p := NewMemPort()
	scope := identity.UserScope("u1")
	for _, m := range []Memory{
		{Scope: scope, Kind: KindFact, Content: "golang fact"},
		{Scope: scope, Kind: KindSummary, Content: "golang summary"},
		{Scope: scope, Kind: KindInsight, Content: "golang insight"},
	} {
		if _, err := p.Store(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	tool := NewRecallTool(p, []identity.ScopeRef{scope})
	res, err := tool.Call(context.Background(), map[string]any{"query": "golang"})
	if err != nil {
		t.Fatal(err)
	}
	// Default kinds = summary + insight, NOT fact.
	if !strings.Contains(res.Content, "golang summary") || !strings.Contains(res.Content, "golang insight") {
		t.Errorf("default recall missing summary/insight: %q", res.Content)
	}
	if strings.Contains(res.Content, "golang fact") {
		t.Errorf("fact must not be in the default recall: %q", res.Content)
	}
}

func TestRecallToolRespectsExplicitKinds(t *testing.T) {
	p := NewMemPort()
	scope := identity.UserScope("u1")
	p.Store(context.Background(), Memory{Scope: scope, Kind: KindFact, Content: "golang fact"})
	p.Store(context.Background(), Memory{Scope: scope, Kind: KindSummary, Content: "golang summary"})
	tool := NewRecallTool(p, []identity.ScopeRef{scope})

	res, err := tool.Call(context.Background(), map[string]any{
		"query": "golang",
		"kinds": []any{"fact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "golang fact") {
		t.Errorf("explicit fact kind missing: %q", res.Content)
	}
	if strings.Contains(res.Content, "golang summary") {
		t.Errorf("summary must be excluded by explicit kinds: %q", res.Content)
	}
}

func TestRecallToolEmptyResultFriendly(t *testing.T) {
	p := NewMemPort()
	tool := NewRecallTool(p, []identity.ScopeRef{identity.UserScope("u1")})
	res, err := tool.Call(context.Background(), map[string]any{"query": "nothing here"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("empty result should be a friendly non-error, got %+v", res)
	}
	if !strings.Contains(res.Content, "no memories found") {
		t.Errorf("empty result should say so, got %q", res.Content)
	}
}

func TestRecallToolScopeIsolation(t *testing.T) {
	p := NewMemPort()
	p.Store(context.Background(), Memory{Scope: identity.UserScope("u2"), Kind: KindSummary, Content: "u2 secret summary"})
	tool := NewRecallTool(p, []identity.ScopeRef{identity.UserScope("u1")})
	res, err := tool.Call(context.Background(), map[string]any{"query": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "u2 secret") {
		t.Errorf("scope isolation violated: %q", res.Content)
	}
}

func TestRecallToolRequiresQuery(t *testing.T) {
	tool := NewRecallTool(NewMemPort(), nil)
	res, _ := tool.Call(context.Background(), map[string]any{})
	if !res.IsError {
		t.Error("missing query should be an error result")
	}
}
