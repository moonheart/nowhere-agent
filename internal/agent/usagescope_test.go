package agent

import (
	"context"
	"testing"
	"time"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// TestUsageMWFoldsDescendantUsageAtRoot pins that the root run's terminal
// KindUsage covers the whole run tree: descendant (subagent) usage folded into
// the scope is added to the root's own total.
func TestUsageMWFoldsDescendantUsageAtRoot(t *testing.T) {
	scope := &UsageScope{root: true}
	scope.Add(provider.Usage{InputTokens: 100, OutputTokens: 40, CacheReadTokens: 9})
	emit := &usageEmitter{}
	state := &RunState{Emit: emit, Usage: provider.Usage{InputTokens: 12, OutputTokens: 7}}

	if err := (&UsageMW{}).AfterRun(WithUsageScope(context.Background(), scope), state); err != nil {
		t.Fatal(err)
	}
	if emit.usage == nil {
		t.Fatal("no KindUsage emitted")
	}
	if *emit.usage != (provider.Usage{InputTokens: 112, OutputTokens: 47, CacheReadTokens: 9}) {
		t.Fatalf("root usage = %+v, want own+descendant total", *emit.usage)
	}
}

// TestUsageMWEmitOnlyOwnBelowRoot pins that a non-root run (a subagent child,
// which inherits its ancestor's scope) emits only its own usage — the root
// does the folding, so nothing is counted twice.
func TestUsageMWEmitOnlyOwnBelowRoot(t *testing.T) {
	scope := &UsageScope{} // root=false: installed by an ancestor run
	scope.Add(provider.Usage{InputTokens: 100, OutputTokens: 40})
	emit := &usageEmitter{}
	state := &RunState{Emit: emit, Usage: provider.Usage{InputTokens: 12, OutputTokens: 7}}

	if err := (&UsageMW{}).AfterRun(WithUsageScope(context.Background(), scope), state); err != nil {
		t.Fatal(err)
	}
	if emit.usage == nil || *emit.usage != (provider.Usage{InputTokens: 12, OutputTokens: 7}) {
		t.Fatalf("non-root usage = %+v, want own usage only", emit.usage)
	}
}

// TestUsageMWRootWithZeroOwnStillReportsDescendants pins that a root whose own
// calls used zero tokens still emits when descendants consumed some.
func TestUsageMWRootWithZeroOwnStillReportsDescendants(t *testing.T) {
	scope := &UsageScope{root: true}
	scope.Add(provider.Usage{InputTokens: 3, OutputTokens: 2})
	emit := &usageEmitter{}
	state := &RunState{Emit: emit}

	if err := (&UsageMW{}).AfterRun(WithUsageScope(context.Background(), scope), state); err != nil {
		t.Fatal(err)
	}
	if emit.usage == nil || *emit.usage != (provider.Usage{InputTokens: 3, OutputTokens: 2}) {
		t.Fatalf("usage = %+v, want descendant total", emit.usage)
	}
}

// TestLoopRunInstallsRootUsageScope pins that Run installs a root scope when
// the incoming ctx has none, so tools dispatched inside the run (spawn_agent)
// can fold descendant usage into it.
func TestLoopRunInstallsRootUsageScope(t *testing.T) {
	var seen *UsageScope
	probe := &scopeProbeTool{fn: func(ctx context.Context) { seen = UsageScopeFrom(ctx) }}
	reg := toolruntime.NewRegistry()
	reg.Register(probe)

	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("t1", "scope_probe", `{}`),
		textResponse("done"),
	}}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})
	if _, err := loop.Run(context.Background(), nil, &memEmitter{}); err != nil {
		t.Fatal(err)
	}
	if seen == nil {
		t.Fatal("no usage scope on the run ctx")
	}
	if !seen.root {
		t.Fatal("scope installed by Run must be the root scope")
	}
}

// TestLoopRunKeepsAncestorScope pins that a nested run (ctx already carries a
// scope) does NOT install its own: descendant usage rolls up to the ancestor's
// root, and the nested run is not marked root.
func TestLoopRunKeepsAncestorScope(t *testing.T) {
	parent := &UsageScope{root: true}
	var seen *UsageScope
	probe := &scopeProbeTool{fn: func(ctx context.Context) { seen = UsageScopeFrom(ctx) }}
	reg := toolruntime.NewRegistry()
	reg.Register(probe)

	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("t1", "scope_probe", `{}`),
		textResponse("done"),
	}}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})
	if _, err := loop.Run(WithUsageScope(context.Background(), parent), nil, &memEmitter{}); err != nil {
		t.Fatal(err)
	}
	if seen != parent {
		t.Fatalf("nested run must inherit the ancestor scope, got %p want %p", seen, parent)
	}
}

type scopeProbeTool struct {
	fn func(context.Context)
}

func (scopeProbeTool) Name() string           { return "scope_probe" }
func (scopeProbeTool) Description() string    { return "" }
func (scopeProbeTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (scopeProbeTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (scopeProbeTool) Timeout() time.Duration { return 0 }
func (t *scopeProbeTool) Call(ctx context.Context, _ map[string]any) (toolruntime.Result, error) {
	t.fn(ctx)
	return toolruntime.Result{Content: "ok"}, nil
}
