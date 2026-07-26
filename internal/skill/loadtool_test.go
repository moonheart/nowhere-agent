package skill

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/toolruntime"
)

func seededEngine(t *testing.T) *Engine {
	t.Helper()
	st := NewStore()
	if _, err := st.Put(context.Background(), Skill{
		Name:        "review",
		Description: "Code review helper",
		Body:        "Review the diff carefully.",
		Scope:       identity.SystemScope(),
		Resources:   map[string]string{"checklist.md": "- tests\n- lint"},
	}); err != nil {
		t.Fatal(err)
	}
	return NewEngine(st)
}

// TestLoadToolIsReadOnly pins the safety contract: the tool is classified
// read-only (no exec, unlike ScriptTool) and advertises a non-empty schema.
func TestLoadToolIsReadOnly(t *testing.T) {
	tool := NewLoadTool(seededEngine(t), []identity.ScopeRef{identity.SystemScope()})
	if tool.Risk() != toolruntime.RiskReadOnly {
		t.Errorf("load_skill risk = %q, want read_only (it must not execute)", tool.Risk())
	}
	if tool.Name() != "load_skill" {
		t.Errorf("name = %q", tool.Name())
	}
	if tool.Schema()["type"] != "object" {
		t.Errorf("schema type = %v", tool.Schema()["type"])
	}
}

// TestLoadToolLoadsBodyAndResource: a name returns the L1 body; name+resource
// returns the L2 resource.
func TestLoadToolLoadsBodyAndResource(t *testing.T) {
	ctx := context.Background()
	tool := NewLoadTool(seededEngine(t), []identity.ScopeRef{identity.SystemScope()})

	body, err := tool.Call(ctx, map[string]any{"name": "review"})
	if err != nil {
		t.Fatal(err)
	}
	if body.IsError || body.Content != "Review the diff carefully." {
		t.Errorf("body = %+v", body)
	}

	res, err := tool.Call(ctx, map[string]any{"name": "review", "resource": "checklist.md"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.Content != "- tests\n- lint" {
		t.Errorf("resource = %+v", res)
	}
}

// TestLoadToolUnknownNameAndResource: missing skill/resource are non-crashing
// error results so the model can self-correct; a missing name arg is an error.
func TestLoadToolUnknownNameAndResource(t *testing.T) {
	ctx := context.Background()
	tool := NewLoadTool(seededEngine(t), []identity.ScopeRef{identity.SystemScope()})

	noName, _ := tool.Call(ctx, map[string]any{})
	if !noName.IsError || !strings.Contains(noName.Content, "name") {
		t.Errorf("missing name should be an error result, got %+v", noName)
	}

	unknown, _ := tool.Call(ctx, map[string]any{"name": "nope"})
	if !unknown.IsError || !strings.Contains(unknown.Content, "unknown skill") {
		t.Errorf("unknown skill should be an error result, got %+v", unknown)
	}

	noRes, _ := tool.Call(ctx, map[string]any{"name": "review", "resource": "missing.txt"})
	if !noRes.IsError || !strings.Contains(noRes.Content, "no resource") {
		t.Errorf("unknown resource should be an error result, got %+v", noRes)
	}
}

// TestLoadToolRespectsScope: a skill outside the caller's scopes is invisible.
func TestLoadToolRespectsScope(t *testing.T) {
	st := NewStore()
	_, _ = st.Put(context.Background(), Skill{
		Name: "secret", Body: "only for user1", Scope: identity.UserScope("user1"),
	})
	// A different user resolves only their own + system scope.
	tool := NewLoadTool(NewEngine(st), []identity.ScopeRef{identity.UserScope("user2"), identity.SystemScope()})
	res, _ := tool.Call(context.Background(), map[string]any{"name": "secret"})
	if !res.IsError {
		t.Errorf("skill in another user's scope must be invisible, got %+v", res)
	}
}
