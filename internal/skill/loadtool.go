package skill

import (
	"context"
	"fmt"
	"strings"
	"time"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/toolruntime"
)

// LoadTool is the read-only companion to progressive disclosure (capability-gap
// K3a): it lets the agent load a selected skill's L1 instructions, or one of
// its L2 resource files, into context. It executes NOTHING — unlike ScriptTool
// it never touches the sandbox — so it carries no exec-safety (C17) concern and
// is RiskReadOnly. Script execution is K3b and stays gated on C17.
type LoadTool struct {
	engine *Engine
	scopes []identity.ScopeRef
}

// NewLoadTool creates a load_skill tool resolving skills against the given scopes.
func NewLoadTool(engine *Engine, scopes []identity.ScopeRef) *LoadTool {
	return &LoadTool{engine: engine, scopes: scopes}
}

func (t *LoadTool) Name() string { return "load_skill" }

func (t *LoadTool) Description() string {
	return "Load a skill's instructions into context. Call with a skill name from the " +
		"\"Available skills\" index to read its full SKILL.md body (L1). Pass resource too " +
		"to read one of the skill's referenced files (L2) instead. Read-only: this loads " +
		"text, it does not execute anything."
}

func (t *LoadTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":     map[string]any{"type": "string", "description": "skill name from the Available skills index"},
			"resource": map[string]any{"type": "string", "description": "optional: a resource file of the skill to load instead of its body"},
		},
		"required": []string{"name"},
	}
}

// Risk is read-only: loading a skill returns text and changes nothing.
func (t *LoadTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }

func (t *LoadTool) Timeout() time.Duration { return 15 * time.Second }

// Call resolves the skill against the caller's scopes and returns its body (or
// one resource). A missing skill/resource is a non-error result listing what is
// available, so the model can self-correct.
func (t *LoadTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return toolruntime.Result{Content: `missing required argument "name"`, IsError: true}, nil
	}

	// A specific resource of the skill.
	if resource, _ := args["resource"].(string); resource != "" {
		content, ok, err := t.engine.LoadL2Resource(ctx, name, resource, t.scopes)
		if err != nil {
			return toolruntime.Result{}, err
		}
		if !ok {
			return toolruntime.Result{Content: fmt.Sprintf("skill %q has no resource %q (or the skill is unknown)", name, resource), IsError: true}, nil
		}
		return toolruntime.Result{Content: content}, nil
	}

	// The skill's full instructions (L1), plus its script names so the model
	// knows what it can run with run_skill_script.
	body, ok, err := t.engine.LoadL1(ctx, name, t.scopes)
	if err != nil {
		return toolruntime.Result{}, err
	}
	if !ok {
		return toolruntime.Result{Content: fmt.Sprintf("unknown skill %q — call load_skill with a name from the Available skills index", name), IsError: true}, nil
	}
	if scripts, ok, err := t.engine.ScriptNames(ctx, name, t.scopes); err == nil && ok && len(scripts) > 0 {
		body += fmt.Sprintf("\n\nScripts (run with %s): %s", RunSkillScriptName, strings.Join(scripts, ", "))
	}
	return toolruntime.Result{Content: body}, nil
}
