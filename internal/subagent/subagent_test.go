package subagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/skill"
	"nowhere-agent/internal/toolruntime"
)

// --- fake providers -------------------------------------------------------

// echoProvider yields one text block on every Stream call (stateless).
type echoProvider struct{ text string }

func (echoProvider) Name() string { return "echo" }
func (p echoProvider) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	evs := textEvents(p.text)
	ch := make(chan provider.Event, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// scriptProvider yields canned event sequences, one per Stream call.
type scriptProvider struct {
	mu     sync.Mutex
	script [][]provider.Event
	calls  int
}

func (p *scriptProvider) Name() string { return "script" }
func (p *scriptProvider) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls >= len(p.script) {
		return nil, errors.New("no more scripted responses")
	}
	evs := p.script[p.calls]
	p.calls++
	ch := make(chan provider.Event, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// blockingProvider never yields; its channel closes only when ctx is cancelled,
// so the child loop blocks until cancellation.
type blockingProvider struct{}

func (blockingProvider) Name() string { return "block" }
func (blockingProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func textEvents(text string) []provider.Event {
	return []provider.Event{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: text},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn, Usage: &provider.Usage{InputTokens: 1, OutputTokens: 1}},
	}
}

func toolUseEvents(id, name, jsonArgs string) []provider.Event {
	return []provider.Event{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: map[string]any{}}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: jsonArgs},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop, StopReason: provider.StopToolUse},
	}
}

func childCfg() agent.Config { return agent.Config{Model: "m", MaxTokens: 256, MaxIterations: 5} }

// funcTool is a Tool that records invocation, for scoping tests.
type funcTool struct {
	name string
	fn   func()
}

func (f funcTool) Name() string         { return f.name }
func (funcTool) Description() string    { return "" }
func (funcTool) Schema() map[string]any { return map[string]any{} }
func (funcTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (funcTool) Timeout() time.Duration { return 0 }
func (f funcTool) Call(context.Context, map[string]any) (toolruntime.Result, error) {
	if f.fn != nil {
		f.fn()
	}
	return toolruntime.Result{Content: "ok"}, nil
}

// --- tests ----------------------------------------------------------------

func TestSpawnBasic(t *testing.T) {
	store := agentdef.NewStore()
	reg := toolruntime.NewRegistry()
	factory := func(context.Context, agentdef.AgentDef, int) *agent.Loop {
		return agent.New(echoProvider{"child result"}, toolruntime.NewRegistry(), childCfg())
	}
	tool := NewSpawnTool(store, reg, factory, 3)
	reg.Register(tool)

	res, err := tool.Call(context.Background(), map[string]any{"prompt": "do x"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError || res.Content != "child result" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestSpawnUnknownType(t *testing.T) {
	tool := NewSpawnTool(agentdef.NewStore(), toolruntime.NewRegistry(),
		func(context.Context, agentdef.AgentDef, int) *agent.Loop {
			return agent.New(echoProvider{"x"}, toolruntime.NewRegistry(), childCfg())
		}, 3)

	res, _ := tool.Call(context.Background(), map[string]any{"prompt": "x", "subagent_type": "nope"})
	if !res.IsError || !strings.Contains(res.Content, "available") {
		t.Fatalf("expected unknown-type error, got %+v", res)
	}
}

func TestSpawnEmptyPrompt(t *testing.T) {
	tool := NewSpawnTool(agentdef.NewStore(), toolruntime.NewRegistry(),
		func(context.Context, agentdef.AgentDef, int) *agent.Loop {
			return agent.New(echoProvider{"x"}, toolruntime.NewRegistry(), childCfg())
		}, 3)

	res, _ := tool.Call(context.Background(), map[string]any{"prompt": "   "})
	if !res.IsError || !strings.Contains(res.Content, "prompt is required") {
		t.Fatalf("expected empty-prompt error, got %+v", res)
	}
}

func TestSpawnDepthGuardDirect(t *testing.T) {
	tool := NewSpawnTool(agentdef.NewStore(), toolruntime.NewRegistry(),
		func(context.Context, agentdef.AgentDef, int) *agent.Loop {
			return agent.New(echoProvider{"x"}, toolruntime.NewRegistry(), childCfg())
		}, 2)

	// A run already at the maximum depth must not spawn.
	res, _ := tool.Call(withDepth(context.Background(), 2), map[string]any{"prompt": "x"})
	if !res.IsError || !strings.Contains(res.Content, "nesting limit") {
		t.Fatalf("expected depth-limit error, got %+v", res)
	}
}

func TestSpawnDepthIncrements(t *testing.T) {
	var gotDepth int
	factory := func(_ context.Context, _ agentdef.AgentDef, depth int) *agent.Loop {
		gotDepth = depth
		return agent.New(echoProvider{"ok"}, toolruntime.NewRegistry(), childCfg())
	}
	tool := NewSpawnTool(agentdef.NewStore(), toolruntime.NewRegistry(), factory, 5)

	if _, err := tool.Call(context.Background(), map[string]any{"prompt": "x"}); err != nil {
		t.Fatal(err)
	}
	if gotDepth != 1 {
		t.Fatalf("top-level spawn should run child at depth 1, got %d", gotDepth)
	}
	if _, err := tool.Call(withDepth(context.Background(), 1), map[string]any{"prompt": "x"}); err != nil {
		t.Fatal(err)
	}
	if gotDepth != 2 {
		t.Fatalf("spawn at depth 1 should run child at depth 2, got %d", gotDepth)
	}
}

func TestSpawnNesting(t *testing.T) {
	store := agentdef.NewStore()
	reg := toolruntime.NewRegistry()
	providers := map[int]provider.Adapter{
		1: &scriptProvider{script: [][]provider.Event{
			toolUseEvents("t1", ToolName, `{"prompt":"sub-task"}`),
			textEvents("child done"),
		}},
		2: echoProvider{"grandchild result"},
	}
	factory := func(_ context.Context, _ agentdef.AgentDef, depth int) *agent.Loop {
		return agent.New(providers[depth], toolruntime.NewRegistry(), childCfg())
	}
	tool := NewSpawnTool(store, reg, factory, 3)
	reg.Register(tool)

	res, err := tool.Call(context.Background(), map[string]any{"prompt": "top"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// The parent sees only the top child's final text, not the grandchild's.
	if res.IsError || res.Content != "child done" {
		t.Fatalf("nesting collapse: %+v", res)
	}
}

func TestSpawnAllowListScoping(t *testing.T) {
	store := agentdef.NewStore()
	store.Put(agentdef.AgentDef{Name: "narrow", WhenToUse: "d", Tools: []string{"read_file"}, Scope: identity.SystemScope()})

	called := false
	reg := toolruntime.NewRegistry()
	reg.Register(funcTool{name: "read_file"})
	reg.Register(funcTool{name: "tracker", fn: func() { called = true }})

	// The child tries to call a tool outside its allow-list, then answers.
	childProv := &scriptProvider{script: [][]provider.Event{
		toolUseEvents("t1", "tracker", `{}`),
		textEvents("done"),
	}}
	factory := func(context.Context, agentdef.AgentDef, int) *agent.Loop {
		return agent.New(childProv, toolruntime.NewRegistry(), childCfg())
	}
	tool := NewSpawnTool(store, reg, factory, 3)
	reg.Register(tool)

	res, err := tool.Call(context.Background(), map[string]any{"prompt": "x", "subagent_type": "narrow"})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatalf("tool outside allow-list was invoked")
	}
	if res.Content != "done" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestSkillToolNames(t *testing.T) {
	reg := toolruntime.NewRegistry()
	reg.Register(funcTool{name: skill.RunSkillScriptName})
	reg.Register(funcTool{name: "read_file"})

	// A definition that declares any skill gains the single fixed script runner.
	got := skillToolNames(reg, []string{"lint"})
	if len(got) != 1 || got[0] != skill.RunSkillScriptName {
		t.Fatalf("expected [%s], got %v", skill.RunSkillScriptName, got)
	}
	// No declared skills → nothing extra.
	if len(skillToolNames(reg, nil)) != 0 {
		t.Fatalf("no skills should map to no tools")
	}
	// Parent run has no script runner (exec disabled / no scripts) → nothing.
	empty := toolruntime.NewRegistry()
	if len(skillToolNames(empty, []string{"lint"})) != 0 {
		t.Fatalf("missing runner should map to no tools")
	}
}

func TestSpawnCancellation(t *testing.T) {
	tool := NewSpawnTool(agentdef.NewStore(), toolruntime.NewRegistry(),
		func(context.Context, agentdef.AgentDef, int) *agent.Loop {
			return agent.New(blockingProvider{}, toolruntime.NewRegistry(), childCfg())
		}, 3)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	res, _ := tool.Call(ctx, map[string]any{"prompt": "x"})
	if !res.IsError {
		t.Fatalf("expected error result on cancellation, got %+v", res)
	}
}

// TestSpawnGatedChildReturnsError pins that a child ending on a permission
// gate (approval/ask_user) does NOT look completed: subagents cannot suspend
// for human input, so the spawn returns an explicit error result naming the
// gated tool, and the sink sees "interrupted" rather than a bare "done".
func TestSpawnGatedChildReturnsError(t *testing.T) {
	store := agentdef.NewStore()
	reg := toolruntime.NewRegistry()
	reg.Register(funcTool{name: "run_command"})

	denyAll := func(context.Context, toolruntime.Tool) (bool, string) {
		return true, agent.ApprovalReasonPrefix + "ask"
	}
	factory := func(_ context.Context, _ agentdef.AgentDef, _ int) *agent.Loop {
		childProv := &scriptProvider{script: [][]provider.Event{
			toolUseEvents("t1", "run_command", `{}`),
		}}
		return agent.New(childProv, toolruntime.NewRegistry(), childCfg()).
			Use(&agent.PermissionMW{Check: denyAll})
	}
	tool := NewSpawnTool(store, reg, factory, 3)
	reg.Register(tool)

	var mu sync.Mutex
	var got []Activity
	ctx := WithSink(context.Background(), func(a Activity) {
		mu.Lock()
		got = append(got, a)
		mu.Unlock()
	})

	res, err := tool.Call(ctx, map[string]any{"prompt": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("gated child must surface an error result, got %+v", res)
	}
	if !strings.Contains(res.Content, "run_command") || !strings.Contains(res.Content, "cannot be delivered inside a subagent") {
		t.Fatalf("error result must name the gated tool and the reason, got %q", res.Content)
	}

	mu.Lock()
	defer mu.Unlock()
	var sawInterrupted, sawDone bool
	for _, a := range got {
		if a.Phase == "interrupted" {
			sawInterrupted = true
			if a.Tool != "run_command" {
				t.Fatalf("interrupted activity must name the gated tool: %+v", a)
			}
		}
		if a.Phase == "done" {
			sawDone = true
		}
	}
	if !sawInterrupted {
		t.Fatalf("expected an interrupted activity, got %+v", got)
	}
	if sawDone {
		t.Fatalf("the trailing done after an interrupt must be suppressed, got %+v", got)
	}
}

// TestSpawnFoldsChildUsageIntoScope pins that a child run's token usage lands
// in the run tree's usage scope, so the root run's terminal usage report (and
// quota accounting) covers subagent model calls.
func TestSpawnFoldsChildUsageIntoScope(t *testing.T) {
	scope := &agent.UsageScope{}
	ctx := agent.WithUsageScope(context.Background(), scope)

	factory := func(context.Context, agentdef.AgentDef, int) *agent.Loop {
		return agent.New(echoProvider{"ok"}, toolruntime.NewRegistry(), childCfg())
	}
	tool := NewSpawnTool(agentdef.NewStore(), toolruntime.NewRegistry(), factory, 3)

	res, err := tool.Call(ctx, map[string]any{"prompt": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	// echoProvider reports 1 input + 1 output token per call.
	if got := scope.Total(); got != (provider.Usage{InputTokens: 1, OutputTokens: 1}) {
		t.Fatalf("scope total = %+v, want the child's usage folded in", got)
	}
}

// TestUsageCaptureForwardsAndFolds covers the capture wrapper directly: every
// event reaches the wrapped emitter, and KindUsage is added to the scope.
func TestUsageCaptureForwardsAndFolds(t *testing.T) {
	scope := &agent.UsageScope{}
	var kinds []agent.EventKind
	cap := usageCapture{
		next:  kindRecorder{fn: func(k agent.EventKind) { kinds = append(kinds, k) }},
		scope: scope,
	}
	_ = cap.Emit(context.Background(), agent.KindText, "hi")
	_ = cap.Emit(context.Background(), agent.KindUsage, provider.Usage{InputTokens: 5, OutputTokens: 2})
	if len(kinds) != 2 {
		t.Fatalf("wrapped emitter must see every event, got %v", kinds)
	}
	if got := scope.Total(); got != (provider.Usage{InputTokens: 5, OutputTokens: 2}) {
		t.Fatalf("scope total = %+v", got)
	}
}

// TestSpawnToolDescriptionListsAgentTypes pins that the tool description
// enumerates resolvable agent types with their when-to-use, so the model can
// pick a subagent_type instead of guessing.
func TestSpawnToolDescriptionListsAgentTypes(t *testing.T) {
	store := agentdef.NewStore()
	store.Put(agentdef.AgentDef{Name: "narrow", WhenToUse: "use for narrow things", Tools: []string{"read_file"}, Scope: identity.SystemScope()})
	tool := NewSpawnTool(store, toolruntime.NewRegistry(),
		func(context.Context, agentdef.AgentDef, int) *agent.Loop {
			return agent.New(echoProvider{"x"}, toolruntime.NewRegistry(), childCfg())
		}, 3)

	desc := tool.Description()
	if !strings.Contains(desc, "- narrow: use for narrow things") {
		t.Fatalf("description must list authored types with when-to-use, got %q", desc)
	}
	if !strings.Contains(desc, agentdef.GeneralPurpose) {
		t.Fatalf("description must list the built-in general-purpose type, got %q", desc)
	}
}

// kindRecorder is an agent.Emitter that records event kinds.
type kindRecorder struct{ fn func(agent.EventKind) }

func (r kindRecorder) Emit(_ context.Context, kind agent.EventKind, _ any) error {
	r.fn(kind)
	return nil
}

func TestSpawnConcurrent(t *testing.T) {
	reg := toolruntime.NewRegistry()
	factory := func(context.Context, agentdef.AgentDef, int) *agent.Loop {
		return agent.New(echoProvider{"ok"}, toolruntime.NewRegistry(), childCfg())
	}
	tool := NewSpawnTool(agentdef.NewStore(), reg, factory, 3)
	reg.Register(tool)

	calls := []toolruntime.Call{
		{ID: "a", Name: ToolName, Args: map[string]any{"prompt": "one"}},
		{ID: "b", Name: ToolName, Args: map[string]any{"prompt": "two"}},
	}
	results := reg.CallAll(context.Background(), calls)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for i, r := range results {
		if r.IsError || r.Content != "ok" {
			t.Fatalf("result %d: %+v", i, r)
		}
	}
}
