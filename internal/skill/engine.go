package skill

import (
	"context"
	"fmt"
	"strings"

	"nowhere-agent/internal/identity"
)

// Reader is the read surface the Engine needs from a skill store: priority
// resolution by name, and the L0 catalog. PGStore implements it.
type Reader interface {
	Get(ctx context.Context, name string, scopes []identity.ScopeRef) (Skill, bool, error)
	List(ctx context.Context, scopes []identity.ScopeRef) ([]L0, error)
}

// Writer is the write surface LoadDir and the management API use to persist a
// skill (creating a new version). PGStore implements it.
type Writer interface {
	Put(ctx context.Context, sk Skill, createdBy string) (Skill, error)
}

// Engine loads skills with progressive disclosure (design D7):
//
//	L0: name + one-line description, always resident
//	L1: full SKILL.md body, loaded when selected
//	L2: resources/scripts, loaded on demand; scripts run in the sandbox
type Engine struct {
	store Reader
}

// NewEngine creates an Engine over a Reader.
func NewEngine(store Reader) *Engine {
	return &Engine{store: store}
}

// LoadL0 returns the always-resident catalog for the given scopes.
func (e *Engine) LoadL0(ctx context.Context, scopes []identity.ScopeRef) ([]L0, error) {
	return e.store.List(ctx, scopes)
}

// RenderL0Prompt renders the L0 catalog as compact prompt text (~one line per skill).
func (e *Engine) RenderL0Prompt(ctx context.Context, scopes []identity.ScopeRef) (string, error) {
	l0, err := e.store.List(ctx, scopes)
	if err != nil {
		return "", err
	}
	if len(l0) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("Available skills:\n")
	for _, s := range l0 {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
		if len(s.Scripts) > 0 {
			fmt.Fprintf(&b, "  scripts: %s (run with run_skill_script)\n", strings.Join(s.Scripts, ", "))
		}
	}
	return b.String(), nil
}

// LoadL1 returns the full body of a selected skill (priority-resolved).
func (e *Engine) LoadL1(ctx context.Context, name string, scopes []identity.ScopeRef) (string, bool, error) {
	sk, ok, err := e.store.Get(ctx, name, scopes)
	if err != nil || !ok {
		return "", ok, err
	}
	return sk.Body, true, nil
}

// LoadL2Resource returns a referenced resource of a skill.
func (e *Engine) LoadL2Resource(ctx context.Context, name, resource string, scopes []identity.ScopeRef) (string, bool, error) {
	sk, ok, err := e.store.Get(ctx, name, scopes)
	if err != nil || !ok {
		return "", ok, err
	}
	content, ok := sk.Resources[resource]
	return content, ok, nil
}

// LoadL2Script returns a script of a skill for sandbox execution.
func (e *Engine) LoadL2Script(ctx context.Context, name, script string, scopes []identity.ScopeRef) (string, bool, error) {
	sk, ok, err := e.store.Get(ctx, name, scopes)
	if err != nil || !ok {
		return "", ok, err
	}
	content, ok := sk.Scripts[script]
	return content, ok, nil
}

// ScriptNames returns the sorted script names of a skill, for surfacing in the
// load_skill body so the model knows what run_skill_script can execute.
func (e *Engine) ScriptNames(ctx context.Context, name string, scopes []identity.ScopeRef) ([]string, bool, error) {
	sk, ok, err := e.store.Get(ctx, name, scopes)
	if err != nil || !ok {
		return nil, ok, err
	}
	return scriptNames(sk.Scripts), true, nil
}
