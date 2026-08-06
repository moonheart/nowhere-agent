package skill

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/identity"
)

func TestEngineProgressiveDisclosure(t *testing.T) {
	store := newMemStore()
	if _, err := store.Put(context.Background(), Skill{
		Name: "deploy", Scope: identity.SystemScope(),
		Description: "Deploy the app",
		Body:        "# Deploy\nfull instructions",
		Resources:   map[string]string{"guide.md": "detailed guide"},
		Scripts:     map[string]string{"run.sh": "echo deploying"},
	}, "test"); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(store)
	ctx := context.Background()
	scopes := []identity.ScopeRef{identity.SystemScope()}

	// L0: only name+description.
	l0, err := e.LoadL0(ctx, scopes)
	if err != nil {
		t.Fatal(err)
	}
	if len(l0) != 1 || l0[0].Name != "deploy" {
		t.Fatalf("L0 = %+v", l0)
	}
	prompt, err := e.RenderL0Prompt(ctx, scopes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "deploy: Deploy the app") {
		t.Errorf("L0 prompt missing skill: %q", prompt)
	}
	// The L0 entry and prompt surface the skill's script names for discovery.
	if len(l0[0].Scripts) != 1 || l0[0].Scripts[0] != "run.sh" {
		t.Errorf("L0 scripts = %v, want [run.sh]", l0[0].Scripts)
	}
	if !strings.Contains(prompt, "run.sh") {
		t.Errorf("L0 prompt should name the script, got %q", prompt)
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
	e := NewEngine(newMemStore())
	_, ok, _ := e.LoadL1(context.Background(), "nope", []identity.ScopeRef{identity.SystemScope()})
	if ok {
		t.Error("expected not found")
	}
}
