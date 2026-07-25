package subagent

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
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

// Fan-out budget defaults: bound the total subagent runs and concurrency per
// top-level run so a single prompt can't trigger a fork/cost bomb.
const (
	defaultMaxTotal      = 32
	defaultMaxConcurrent = 8
)

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
	// maxTotal caps the total subagent runs across the whole run tree; spawned
	// counts them. sem caps concurrent child runs. One SpawnTool instance is
	// shared by every nested subagent in a run, so this budget is per-run.
	maxTotal int
	spawned  atomic.Int64
	sem      chan struct{}
}

// NewSpawnTool creates a spawn tool over a definition store, the run's tool
// registry, and a child-loop factory. maxDepth <= 0 uses the default. scopes
// default to system scope (v1 resolves built-in + system definitions; per-user
// authored definitions are future work). Fan-out is bounded by default budgets;
// override with WithBudget.
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
		maxTotal: defaultMaxTotal,
		sem:      make(chan struct{}, defaultMaxConcurrent),
	}
}

// WithBudget bounds subagent fan-out for the whole run tree: maxTotal caps the
// total number of subagent runs, maxConcurrent caps how many run at once.
// Non-positive values keep the defaults. Call once at setup, before any spawn.
func (t *SpawnTool) WithBudget(maxTotal, maxConcurrent int) *SpawnTool {
	if maxTotal > 0 {
		t.maxTotal = maxTotal
	}
	if maxConcurrent > 0 {
		t.sem = make(chan struct{}, maxConcurrent)
	}
	return t
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

	// Per-run total-spawn budget (this tool instance is shared across the whole
	// run tree): bound cost and prevent a fork/cost bomb from unbounded fan-out.
	if int(t.spawned.Add(1)) > t.maxTotal {
		return errf("subagent budget exhausted (at most %d subagent runs per request) — complete the task with fewer agents", t.maxTotal), nil
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

	// Forward the child's progress to the run panel when a sink is installed
	// (runtime path); otherwise the child runs black-box. Only the collapsed
	// result reaches the parent either way. The call id tags every signal so the
	// UI nests this child's streamed output under the right spawn_agent card.
	callID := toolruntime.CallIDFrom(ctx)
	var emitter agent.Emitter = discardEmitter{}
	if sink := sinkFrom(ctx); sink != nil {
		sink(Activity{AgentType: def.Name, Depth: childDepth, Phase: "start", ToolCallID: callID})
		emitter = activityEmitter{sink: sink, agentType: def.Name, depth: childDepth, toolCallID: callID}
	}

	// Bound concurrent child runs to limit goroutine/CPU/memory pressure under
	// wide fan-out; wait for a slot or bail out if the run is cancelled.
	select {
	case t.sem <- struct{}{}:
		defer func() { <-t.sem }()
	case <-ctx.Done():
		return errf("cancelled while waiting for a subagent slot"), nil
	}

	msgs, runErr := child.Run(childCtx, history, emitter)
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

// discardEmitter drops all child loop events. It is the fallback when no
// activity sink is installed (no runtime, e.g. tests/dev): the child stays fully
// black-box and only its collapsed result reaches the parent. When a sink is
// present the tool uses activityEmitter instead (see activity.go).
type discardEmitter struct{}

func (discardEmitter) Emit(context.Context, agent.EventKind, any) error { return nil }
