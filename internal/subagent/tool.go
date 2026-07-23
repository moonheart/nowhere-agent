package subagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// ToolName is the model-facing name of the spawn tool.
const ToolName = "spawn_agent"

// defaultMaxDepth bounds subagent nesting when unset.
const defaultMaxDepth = 3

// defaultTimeout bounds a single subagent run.
const defaultTimeout = 5 * time.Minute

// LoopFactory builds a child agent.Loop for a resolved definition at the given
// nesting depth. The server supplies it (closing over the provider adapter,
// base model, and compression config) so the subagent package needs no wiring
// dependency. The child's system prompt comes from def.System and its model
// from def.Model (falling back to the parent's); the tool registry is set by
// the SpawnTool via Loop.WithTools, not by the factory.
type LoopFactory func(ctx context.Context, def agentdef.AgentDef, depth int) *agent.Loop

// SpawnTool launches a child agent to handle a delegated task and returns the
// child's final assistant text as the tool result. It implements
// toolruntime.Tool.
type SpawnTool struct {
	store    *agentdef.Store
	parent   *toolruntime.Registry // the run's registry; child pools are scoped views of it
	factory  LoopFactory
	scopes   []identity.ScopeRef
	maxDepth int
	timeout  time.Duration
}

// NewSpawnTool creates a spawn tool over a definition store, the run's tool
// registry, and a child-loop factory. maxDepth <= 0 uses the default. scopes
// default to system scope (v1 resolves built-in + system definitions; per-user
// authored definitions are future work).
func NewSpawnTool(store *agentdef.Store, parent *toolruntime.Registry, factory LoopFactory, maxDepth int, scopes ...identity.ScopeRef) *SpawnTool {
	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}
	if len(scopes) == 0 {
		scopes = []identity.ScopeRef{identity.SystemScope()}
	}
	return &SpawnTool{
		store:    store,
		parent:   parent,
		factory:  factory,
		scopes:   scopes,
		maxDepth: maxDepth,
		timeout:  defaultTimeout,
	}
}

// Name identifies the tool.
func (t *SpawnTool) Name() string { return ToolName }

// Description tells the model what the tool does.
func (t *SpawnTool) Description() string {
	return "Launch a subagent to handle a delegated task autonomously in an isolated context. " +
		"The subagent runs its own agent loop with a scoped set of tools and returns a single result to you. " +
		"It does NOT see this conversation, so write a self-contained prompt: state the goal, the relevant context, " +
		"and whether you want research or changes. Omit subagent_type to use the general-purpose agent. " +
		"To run subagents in parallel, emit multiple spawn_agent calls in one turn."
}

// Schema is the JSON Schema for the tool input.
func (t *SpawnTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"description": map[string]any{
				"type":        "string",
				"description": "A short (3-5 word) description of the task",
			},
			"prompt": map[string]any{
				"type":        "string",
				"description": "The self-contained task for the subagent to perform",
			},
			"subagent_type": map[string]any{
				"type":        "string",
				"description": "Which agent type to launch; omit for general-purpose",
			},
		},
		"required": []string{"prompt"},
	}
}

// Risk classifies the spawn itself as read-only — launching a child makes no
// external change. The child's own tools carry their own risk classification.
func (t *SpawnTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }

// Timeout bounds a single subagent run.
func (t *SpawnTool) Timeout() time.Duration { return t.timeout }

// Call resolves the agent definition, builds a scoped child loop, runs it over
// a prompt-only context, and returns the collapsed result. Failures (unknown
// type, empty prompt, depth limit, child error) are returned as error results
// so the parent model can self-correct rather than crashing the run.
func (t *SpawnTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	depth := depthFrom(ctx)
	// Call-level depth guard (belt to the tool-exclusion suspenders): a spawn at
	// or beyond the cap would create a child past the maximum depth.
	if depth >= t.maxDepth {
		return errf("subagent nesting limit reached (max depth %d) — complete the task directly instead of spawning another agent", t.maxDepth), nil
	}

	prompt, _ := args["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return errf("prompt is required"), nil
	}

	typ, _ := args["subagent_type"].(string)
	if strings.TrimSpace(typ) == "" {
		typ = agentdef.GeneralPurpose
	}
	def, err := t.store.Resolve(typ, t.scopes)
	if err != nil {
		return errf("%v", err), nil
	}

	childDepth := depth + 1

	// Build the child's scoped tool pool: the def's allow-list (plus any skill
	// script tools) minus its deny-list, and minus the spawn tool once the child
	// would sit at the maximum depth (so it cannot spawn further).
	allow := def.Tools
	if !def.Wildcard() {
		if extra := skillToolNames(t.parent, def.Skills); len(extra) > 0 {
			allow = append(append([]string{}, allow...), extra...)
		}
	}
	var exclude []string
	if childDepth >= t.maxDepth {
		exclude = []string{ToolName}
	}
	scoped := t.parent.Scoped(allow, def.DisallowedTools, exclude...)

	child := t.factory(ctx, def, childDepth).WithTools(scoped)
	childCtx := withDepth(ctx, childDepth)
	history := []provider.Message{provider.TextMessage(provider.RoleUser, prompt)}

	msgs, runErr := child.Run(childCtx, history, discardEmitter{})
	if runErr != nil {
		// Surface any partial output the subagent produced before failing.
		res := collapse(msgs)
		res.IsError = true
		if res.Content == noOutput {
			res.Content = fmt.Sprintf("subagent failed: %v", runErr)
		} else {
			res.Content = fmt.Sprintf("subagent failed: %v\n\npartial output:\n%s", runErr, res.Content)
		}
		return res, nil
	}
	return collapse(msgs), nil
}

// errf builds an error tool result from a formatted message.
func errf(format string, a ...any) toolruntime.Result {
	return toolruntime.Result{Content: fmt.Sprintf(format, a...), IsError: true}
}

// discardEmitter drops all child loop events. v1 subagents are black-box: only
// the collapsed final result reaches the parent. (A forwarding emitter feeding
// the web activity feed is a documented future enhancement.)
type discardEmitter struct{}

func (discardEmitter) Emit(context.Context, agent.EventKind, any) error { return nil }
