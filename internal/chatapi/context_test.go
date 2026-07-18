package chatapi

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/skill"
)

// staticScopes returns a fixed scope set (test double for identity.Service).
type staticScopes struct{ scopes []identity.ScopeRef }

func (s staticScopes) AccessibleScopes(context.Context, string) ([]identity.ScopeRef, error) {
	return s.scopes, nil
}

func TestContextBuilderComposesSkillsAndMemory(t *testing.T) {
	user := identity.User{ID: "u1"}
	scopes := staticScopes{scopes: []identity.ScopeRef{identity.UserScope("u1"), identity.SystemScope()}}

	// Seed a skill and a memory.
	skillStore := skill.NewStore()
	skills := skill.NewEngine(skillStore)
	if _, err := skillStore.Put(context.Background(), skill.Skill{
		Name: "deploy", Scope: identity.UserScope("u1"), Description: "deploy the app",
	}); err != nil {
		t.Fatal(err)
	}
	mem := memory.NewMemPort()
	if _, err := mem.Store(context.Background(), memory.Memory{
		Scope: identity.UserScope("u1"), Kind: memory.KindPreference, Content: "prefers dark mode",
	}); err != nil {
		t.Fatal(err)
	}

	cb := NewContextBuilder("You are nowhere-agent.", scopes, mem, skills)
	out := cb.SystemPrompt(context.Background(), user, "dark mode")

	for _, want := range []string{
		"You are nowhere-agent.",
		"Available skills:",
		"deploy: deploy the app",
		"Relevant memories",
		"prefers dark mode",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("system prompt missing %q\n---\n%s", want, out)
		}
	}
}

func TestContextBuilderOmitsEmptySections(t *testing.T) {
	user := identity.User{ID: "u1"}
	scopes := staticScopes{scopes: []identity.ScopeRef{identity.UserScope("u1")}}
	cb := NewContextBuilder("base", scopes, memory.NewMemPort(), skill.NewEngine(skill.NewStore()))

	out := cb.SystemPrompt(context.Background(), user, "anything")
	if out != "base" {
		t.Errorf("expected only base prompt, got %q", out)
	}
}

func TestContextBuilderSkipsRecallOnEmptyQuery(t *testing.T) {
	user := identity.User{ID: "u1"}
	scopes := staticScopes{scopes: []identity.ScopeRef{identity.UserScope("u1")}}
	mem := memory.NewMemPort()
	mem.Store(context.Background(), memory.Memory{Scope: identity.UserScope("u1"), Kind: memory.KindFact, Content: "fact"})

	cb := NewContextBuilder("base", scopes, mem, nil)
	out := cb.SystemPrompt(context.Background(), user, "")
	if strings.Contains(out, "Relevant memories") {
		t.Errorf("recall should be skipped for empty query, got %q", out)
	}
}

func TestContextBuilderScopeIsolation(t *testing.T) {
	user := identity.User{ID: "u1"}
	// Only u1's scope is accessible; a memory owned by u2 must not leak.
	scopes := staticScopes{scopes: []identity.ScopeRef{identity.UserScope("u1")}}
	mem := memory.NewMemPort()
	mem.Store(context.Background(), memory.Memory{Scope: identity.UserScope("u2"), Kind: memory.KindFact, Content: "u2 secret"})

	cb := NewContextBuilder("base", scopes, mem, nil)
	out := cb.SystemPrompt(context.Background(), user, "secret")
	if strings.Contains(out, "u2 secret") {
		t.Errorf("scope isolation violated, leaked u2 memory: %q", out)
	}
}
