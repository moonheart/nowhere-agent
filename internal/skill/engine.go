package skill

import (
	"context"
	"fmt"
	"strings"

	"nowhere-agent/internal/identity"
)

// Engine loads skills with progressive disclosure (design D7):
//   L0: name + one-line description, always resident
//   L1: full SKILL.md body, loaded when selected
//   L2: resources/scripts, loaded on demand; scripts run in the sandbox
type Engine struct {
	store  *Store
}

// NewEngine creates an Engine over a Store.
func NewEngine(store *Store) *Engine {
	return &Engine{store: store}
}

// LoadL0 returns the always-resident catalog for the given scopes.
func (e *Engine) LoadL0(ctx context.Context, scopes []identity.ScopeRef) []L0 {
	return e.store.List(ctx, scopes)
}

// RenderL0Prompt renders the L0 catalog as compact prompt text (~one line per skill).
func (e *Engine) RenderL0Prompt(ctx context.Context, scopes []identity.ScopeRef) string {
	l0 := e.store.List(ctx, scopes)
	if len(l0) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Available skills:\n")
	for _, s := range l0 {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	return b.String()
}

// LoadL1 returns the full body of a selected skill (priority-resolved).
func (e *Engine) LoadL1(ctx context.Context, name string, scopes []identity.ScopeRef) (string, bool, error) {
	sk, ok := e.store.Get(ctx, name, scopes)
	if !ok {
		return "", false, nil
	}
	return sk.Body, true, nil
}

// LoadL2Resource returns a referenced resource of a skill.
func (e *Engine) LoadL2Resource(ctx context.Context, name, resource string, scopes []identity.ScopeRef) (string, bool, error) {
	sk, ok := e.store.Get(ctx, name, scopes)
	if !ok {
		return "", false, nil
	}
	content, ok := sk.Resources[resource]
	return content, ok, nil
}

// LoadL2Script returns a script of a skill for sandbox execution.
func (e *Engine) LoadL2Script(ctx context.Context, name, script string, scopes []identity.ScopeRef) (string, bool, error) {
	sk, ok := e.store.Get(ctx, name, scopes)
	if !ok {
		return "", false, nil
	}
	content, ok := sk.Scripts[script]
	return content, ok, nil
}
