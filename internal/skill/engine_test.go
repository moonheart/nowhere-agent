package skill

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/sandbox"
)

func TestEngineProgressiveDisclosure(t *testing.T) {
	store := NewStore()
	store.Put(context.Background(), Skill{
		Name: "deploy", Scope: identity.SystemScope(),
		Description: "Deploy the app",
		Body:        "# Deploy\nfull instructions",
		Resources:   map[string]string{"guide.md": "detailed guide"},
		Scripts:     map[string]string{"run.sh": "echo deploying"},
	})
	e := NewEngine(store)
	ctx := context.Background()
	scopes := []identity.ScopeRef{identity.SystemScope()}

	// L0: only name+description.
	l0 := e.LoadL0(ctx, scopes)
	if len(l0) != 1 || l0[0].Name != "deploy" {
		t.Fatalf("L0 = %+v", l0)
	}
	prompt := e.RenderL0Prompt(ctx, scopes)
	if !strings.Contains(prompt, "deploy: Deploy the app") {
		t.Errorf("L0 prompt missing skill: %q", prompt)
	}

	// L1: full body.
	body, ok, _ := e.LoadL1(ctx, "deploy", scopes)
	if !ok || !strings.Contains(body, "full instructions") {
		t.Errorf("L1 body = %q ok=%v", body, ok)
	}

	// L2 resource.
	res, ok, _ := e.LoadL2Resource(ctx, "deploy", "guide.md", scopes)
	if !ok || res != "detailed guide" {
		t.Errorf("L2 resource = %q ok=%v", res, ok)
	}

	// L2 script.
	script, ok, _ := e.LoadL2Script(ctx, "deploy", "run.sh", scopes)
	if !ok || script != "echo deploying" {
		t.Errorf("L2 script = %q ok=%v", script, ok)
	}
}

func TestEngineMissingSkill(t *testing.T) {
	e := NewEngine(NewStore())
	_, ok, _ := e.LoadL1(context.Background(), "nope", []identity.ScopeRef{identity.SystemScope()})
	if ok {
		t.Error("expected not found")
	}
}

func TestScriptToolName(t *testing.T) {
	sb := sandbox.NewMemPort()
	h, _ := sb.Create(context.Background(), "s1", sandbox.Options{})
	tool := NewScriptTool("my-skill", "run.sh", "echo hi", "desc", sb, h)
	if tool.Name() != "skill_my_skill_run_sh" {
		t.Errorf("name = %q", tool.Name())
	}
}

func TestScriptToolCall(t *testing.T) {
	sb := sandbox.NewMemPort()
	h, _ := sb.Create(context.Background(), "s1", sandbox.Options{})
	tool := NewScriptTool("deploy", "run.sh", "echo deploying", "desc", sb, h)
	res, err := tool.Call(context.Background(), map[string]any{"args": "--prod"})
	if err != nil {
		t.Fatal(err)
	}
	// MemPort returns empty stdout with exit 0.
	if res.IsError {
		t.Errorf("unexpected error result: %+v", res)
	}
}
