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

// staticSkills is a test double for the skill Reader the context builder needs:
// a fixed L0 catalog and priority-resolved Get. The production store is PG; the
// builder only reads, so a slice stands in.
type staticSkills struct{ skills []skill.Skill }

func (s staticSkills) Get(_ context.Context, name string, scopes []identity.ScopeRef) (skill.Skill, bool, error) {
	for _, scope := range scopes {
		for _, sk := range s.skills {
			if sk.Name == name && sk.Scope == scope {
				return sk, true, nil
			}
		}
	}
	return skill.Skill{}, false, nil
}

func (s staticSkills) List(_ context.Context, scopes []identity.ScopeRef) ([]skill.L0, error) {
	best := map[string]skill.L0{}
	bestRank := map[string]int{}
	for _, sk := range s.skills {
		r := -1
		for i, sc := range scopes {
			if sc == sk.Scope {
				r = i
				break
			}
		}
		if r < 0 {
			continue
		}
		if cur, ok := bestRank[sk.Name]; !ok || r < cur {
			best[sk.Name] = skill.L0{Name: sk.Name, Description: sk.Description}
			bestRank[sk.Name] = r
		}
	}
	out := make([]skill.L0, 0, len(best))
	for _, l := range best {
		out = append(out, l)
	}
	return out, nil
}

// TestContextBuilderComposesSkillsNotMemory pins the slimmed system prompt: it
// carries base + skills (L0) but NOT recalled memories — those are injected
// incrementally into the message view (capability K / context-mgmt), so the
// system prompt stays byte-stable for prompt caching.
func TestContextBuilderComposesSkillsNotMemory(t *testing.T) {
	user := identity.User{ID: "u1"}
	scopes := staticScopes{scopes: []identity.ScopeRef{identity.UserScope("u1"), identity.SystemScope()}}

	skillStore := staticSkills{skills: []skill.Skill{
		{Name: "deploy", Scope: identity.UserScope("u1"), Description: "deploy the app"},
	}}
	skills := skill.NewEngine(skillStore)
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
	} {
		if !strings.Contains(out, want) {
			t.Errorf("system prompt missing %q\n---\n%s", want, out)
		}
	}
	// Memory must NOT be in the system prompt (it's injected into the view).
	if strings.Contains(out, "prefers dark mode") || strings.Contains(out, "Relevant memories") {
		t.Errorf("memory must not be in the system prompt, got\n---\n%s", out)
	}
}

// TestContextBuilderSystemStableAcrossQueries: with memory excluded, the system
// prompt no longer depends on the query — two different queries yield the same
// byte-stable prefix (the caching invariant this change exists for).
func TestContextBuilderSystemStableAcrossQueries(t *testing.T) {
	user := identity.User{ID: "u1"}
	scopes := staticScopes{scopes: []identity.ScopeRef{identity.UserScope("u1")}}
	mem := memory.NewMemPort()
	mem.Store(context.Background(), memory.Memory{Scope: identity.UserScope("u1"), Kind: memory.KindFact, Content: "fact one"})
	mem.Store(context.Background(), memory.Memory{Scope: identity.UserScope("u1"), Kind: memory.KindFact, Content: "fact two"})

	cb := NewContextBuilder("base", scopes, mem, nil)
	a := cb.SystemPrompt(context.Background(), user, "first query")
	b := cb.SystemPrompt(context.Background(), user, "a totally different query")
	if a != b {
		t.Errorf("system prompt must be byte-stable across queries, got %q vs %q", a, b)
	}
	if a != "base" {
		t.Errorf("expected only base prompt, got %q", a)
	}
}

func TestContextBuilderOmitsEmptySections(t *testing.T) {
	user := identity.User{ID: "u1"}
	scopes := staticScopes{scopes: []identity.ScopeRef{identity.UserScope("u1")}}
	cb := NewContextBuilder("base", scopes, memory.NewMemPort(), skill.NewEngine(staticSkills{}))

	out := cb.SystemPrompt(context.Background(), user, "anything")
	if out != "base" {
		t.Errorf("expected only base prompt, got %q", out)
	}
}

func TestContextBuilderScopeIsolation(t *testing.T) {
	user := identity.User{ID: "u1"}
	// Only u1's scope is accessible; a memory owned by u2 must not leak. (Memory
	// no longer enters the system prompt at all, but the scope resolver must
	// still be consulted only with the caller's scopes.)
	scopes := staticScopes{scopes: []identity.ScopeRef{identity.UserScope("u1")}}
	mem := memory.NewMemPort()
	mem.Store(context.Background(), memory.Memory{Scope: identity.UserScope("u2"), Kind: memory.KindFact, Content: "u2 secret"})

	cb := NewContextBuilder("base", scopes, mem, nil)
	out := cb.SystemPrompt(context.Background(), user, "secret")
	if strings.Contains(out, "u2 secret") {
		t.Errorf("scope isolation violated, leaked u2 memory: %q", out)
	}
}
