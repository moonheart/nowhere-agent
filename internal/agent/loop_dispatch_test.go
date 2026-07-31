package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// scriptCall describes one tool_use block for multiToolUseResponse.
type scriptCall struct{ id, name, args string }

// multiToolUseResponse builds one assistant turn carrying several tool_use
// blocks, ending with the given stop reason — enough to exercise a whole batch
// (ordering, index alignment, concurrency) in a single turn.
func multiToolUseResponse(stop provider.StopReason, calls ...scriptCall) []provider.Event {
	evs := []provider.Event{{Type: provider.EventMessageStart}}
	for i, c := range calls {
		evs = append(evs,
			provider.Event{Type: provider.EventBlockStart, Index: i, Block: &provider.Block{
				Type: provider.BlockToolUse, ToolUseID: c.id, ToolName: c.name, ToolInput: map[string]any{},
			}},
			provider.Event{Type: provider.EventBlockDelta, Index: i, Delta: c.args},
			provider.Event{Type: provider.EventBlockStop, Index: i},
		)
	}
	return append(evs, provider.Event{Type: provider.EventMessageStop, StopReason: stop})
}

// truncatedToolUseResponse builds a turn whose tool call arrives on a message cut
// off at the output-token limit.
func truncatedToolUseResponse(id, name, args string) []provider.Event {
	evs := toolUseResponse(id, name, args)
	evs[len(evs)-1].StopReason = provider.StopMaxTokens
	return evs
}

// toolMWRecorder is a ToolCallMiddleware that records what the loop hands it.
// Dispatch runs the batch concurrently, so it locks.
type toolMWRecorder struct {
	name  string
	mu    *sync.Mutex
	log   *[]string
	seen  *[]*ToolCall
	short toolruntime.Result // when Content is non-empty, return it without calling next
}

func (m toolMWRecorder) MiddlewareName() string { return m.name }

func (m toolMWRecorder) WrapToolCall(ctx context.Context, c *ToolCall, next ToolHandler) toolruntime.Result {
	m.mu.Lock()
	*m.log = append(*m.log, "in:"+m.name)
	if m.seen != nil {
		*m.seen = append(*m.seen, c)
	}
	m.mu.Unlock()
	if m.short.Content != "" {
		return m.short
	}
	res := next(ctx, c)
	m.mu.Lock()
	*m.log = append(*m.log, "out:"+m.name)
	m.mu.Unlock()
	return res
}

// toolResultBlocks returns the tool_result blocks of the first tool-result
// message in a run's produced messages.
func toolResultBlocks(t *testing.T, produced []provider.Message) []provider.Block {
	t.Helper()
	for _, m := range produced {
		if len(m.Content) > 0 && m.Content[0].Type == provider.BlockToolResult {
			return m.Content
		}
	}
	t.Fatalf("no tool-result message in produced: %+v", produced)
	return nil
}

// TestDispatchRoutesThroughToolMiddleware is the regression test for a wiring
// gap: ToolCallMiddleware was collected by Use and composed by chainTool, and
// chainTool was unit-tested in isolation — but dispatch never called it, so
// registering a tool middleware was silently a no-op. This asserts the loop
// really routes each call through the chain, first-registered outermost.
func TestDispatchRoutesThroughToolMiddleware(t *testing.T) {
	var mu sync.Mutex
	var log []string
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "echo", `{}`),
		textResponse("done"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})
	loop.Use(
		toolMWRecorder{name: "m1", mu: &mu, log: &log},
		toolMWRecorder{name: "m2", mu: &mu, log: &log},
	)

	produced, err := loop.Run(context.Background(), nil, &memEmitter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(log) == 0 {
		t.Fatal("tool middleware never ran: dispatch is not routed through chainTool")
	}
	assertOrder(t, []string{"in:m1", "in:m2", "out:m2", "out:m1"}, log)
	// The tool still executed and its result reached the model.
	if got := toolResultBlocks(t, produced)[0].ToolContent; got != "echo-result" {
		t.Errorf("tool result = %q, want echo-result", got)
	}
}

// TestToolMiddlewareSeesResolvedCall pins the chain's input contract: middleware
// receives the originating call AND a non-nil, already-resolved Tool, so a
// middleware may inspect the tool (e.g. its Risk) without a nil check.
func TestToolMiddlewareSeesResolvedCall(t *testing.T) {
	var mu sync.Mutex
	var log []string
	var seen []*ToolCall
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "echo", `{"a":1}`),
		textResponse("done"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100}).
		Use(toolMWRecorder{name: "m1", mu: &mu, log: &log, seen: &seen})

	if _, err := loop.Run(context.Background(), nil, &memEmitter{}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("middleware saw %d calls, want 1", len(seen))
	}
	tc := seen[0]
	if tc.Tool == nil {
		t.Fatal("ToolCall.Tool is nil; middleware must get a resolved tool")
	}
	if tc.Tool.Name() != "echo" {
		t.Errorf("ToolCall.Tool.Name() = %q, want echo", tc.Tool.Name())
	}
	if tc.Call.ID != "tu1" || tc.Call.Name != "echo" {
		t.Errorf("ToolCall.Call = %+v, want id tu1 name echo", tc.Call)
	}
	if tc.Call.Args["a"] != float64(1) {
		t.Errorf("ToolCall.Call.Args = %v, want the decoded arguments", tc.Call.Args)
	}
}

// TestToolMiddlewareCanShortCircuitDispatch verifies a tool middleware may
// decline to call next — the tool does not execute and the middleware's result
// is what the model sees. This is the control power that makes the wrap hook
// worth having (caching, mocking, policy substitution).
func TestToolMiddlewareCanShortCircuitDispatch(t *testing.T) {
	var mu sync.Mutex
	var log []string
	called := false
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "danger", `{}`),
		textResponse("done"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(riskTool{name: "danger", risk: toolruntime.RiskReadOnly, called: &called})
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100}).
		Use(toolMWRecorder{name: "block", mu: &mu, log: &log,
			short: toolruntime.Result{Content: "intercepted", IsError: true}})

	produced, err := loop.Run(context.Background(), nil, &memEmitter{})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("tool executed despite the middleware short-circuiting")
	}
	blk := toolResultBlocks(t, produced)[0]
	if blk.ToolContent != "intercepted" || !blk.IsError {
		t.Errorf("tool result = %q (isError=%v), want the middleware's result", blk.ToolContent, blk.IsError)
	}
}

// TestDispatchScreensBeforeMiddleware pins the screening order: a call with
// malformed arguments, an unregistered name, or a permission deny is answered
// inline and never enters the chain. That is what lets the chain guarantee a
// non-nil Tool, and it keeps results index-aligned with calls so tool_use and
// tool_result stay paired.
func TestDispatchScreensBeforeMiddleware(t *testing.T) {
	var mu sync.Mutex
	var log []string
	var seen []*ToolCall
	p := &scriptProvider{script: [][]provider.Event{
		multiToolUseResponse(provider.StopEndTurn,
			scriptCall{"tu1", "echo", `{}`},
			scriptCall{"tu2", "echo", `{not json`},
			scriptCall{"tu3", "nope", `{}`},
		),
		textResponse("done"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100}).
		Use(toolMWRecorder{name: "m1", mu: &mu, log: &log, seen: &seen})

	produced, err := loop.Run(context.Background(), nil, &memEmitter{})
	if err != nil {
		t.Fatal(err)
	}
	// Only the one dispatchable call reached the chain.
	if len(seen) != 1 || seen[0].Call.ID != "tu1" {
		t.Fatalf("chain saw %d calls (want only tu1): %+v", len(seen), seen)
	}
	blocks := toolResultBlocks(t, produced)
	if len(blocks) != 3 {
		t.Fatalf("got %d tool_result blocks, want 3 (one per tool_use)", len(blocks))
	}
	for i, want := range []struct {
		id, contains string
		isErr        bool
	}{
		{"tu1", "echo-result", false},
		{"tu2", "invalid tool arguments", true},
		{"tu3", "unknown tool: nope", true},
	} {
		if blocks[i].ToolResultID != want.id {
			t.Errorf("block[%d] id = %q, want %q — results must stay index-aligned with calls", i, blocks[i].ToolResultID, want.id)
		}
		if !strings.Contains(blocks[i].ToolContent, want.contains) {
			t.Errorf("block[%d] content = %q, want it to contain %q", i, blocks[i].ToolContent, want.contains)
		}
		if blocks[i].IsError != want.isErr {
			t.Errorf("block[%d] isError = %v, want %v", i, blocks[i].IsError, want.isErr)
		}
	}
}

// barrierTool blocks until every concurrent invocation has arrived, so a serial
// dispatch cannot complete the batch. Its timeout bounds that failure instead of
// hanging the test.
type barrierTool struct {
	arrived chan struct{}
	release chan struct{}
}

func (barrierTool) Name() string           { return "barrier" }
func (barrierTool) Description() string    { return "blocks until its peers arrive" }
func (barrierTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (barrierTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (barrierTool) Timeout() time.Duration { return time.Second }
func (b barrierTool) Call(ctx context.Context, _ map[string]any) (toolruntime.Result, error) {
	select {
	case b.arrived <- struct{}{}:
	case <-ctx.Done():
		return toolruntime.Result{}, ctx.Err()
	}
	select {
	case <-b.release:
		return toolruntime.Result{Content: "released"}, nil
	case <-ctx.Done():
		return toolruntime.Result{}, ctx.Err()
	}
}

// TestDispatchStaysConcurrent guards the property that moving the fan-out out of
// Registry.CallAll (so each call could be wrapped by middleware) preserved:
// a batch still runs concurrently. Both calls must be in flight at once, which a
// serial dispatch can never satisfy.
func TestDispatchStaysConcurrent(t *testing.T) {
	tool := barrierTool{arrived: make(chan struct{}, 2), release: make(chan struct{})}
	go func() {
		for i := 0; i < 2; i++ {
			select {
			case <-tool.arrived:
			case <-time.After(3 * time.Second):
				return // leave release shut: the calls time out and the test reports it
			}
		}
		close(tool.release)
	}()

	p := &scriptProvider{script: [][]provider.Event{
		multiToolUseResponse(provider.StopEndTurn,
			scriptCall{"tu1", "barrier", `{}`},
			scriptCall{"tu2", "barrier", `{}`},
		),
		textResponse("done"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(tool)
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(context.Background(), nil, &memEmitter{})
	if err != nil {
		t.Fatal(err)
	}
	for i, blk := range toolResultBlocks(t, produced) {
		if blk.ToolContent != "released" {
			t.Fatalf("call %d = %q (isError=%v); both calls must be in flight at once — dispatch is serial",
				i, blk.ToolContent, blk.IsError)
		}
	}
}

// TestLoopTruncatedToolBatchNotDispatched is the tool-call half of the L1
// truncation guard. A message cut off at max_tokens WHILE emitting tool calls
// used to fall straight through to dispatch: the loop only checked the stop
// reason when there were no calls. The batch is untrustworthy — the cut-off
// call's arguments are incomplete and the plan is half-written — so every call
// fails with a message naming the real cause, and the model gets to re-issue.
func TestLoopTruncatedToolBatchNotDispatched(t *testing.T) {
	called := false
	p := &scriptProvider{script: [][]provider.Event{
		truncatedToolUseResponse("tu1", "danger", `{"path":"/etc"}`),
		textResponse("re-issued and finished"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(riskTool{name: "danger", risk: toolruntime.RiskReadOnly, called: &called})
	emit := &memEmitter{}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 4096})

	produced, err := loop.Run(context.Background(), nil, emit)
	if err != nil {
		t.Fatalf("a truncated batch is recoverable, not fatal: %v", err)
	}
	if called {
		t.Error("a tool call from a truncated message must not execute")
	}
	// The model was told what actually happened — not that its JSON was malformed.
	blocks := toolResultBlocks(t, produced)
	if len(blocks) != 1 {
		t.Fatalf("got %d tool_result blocks, want 1 — every tool_use needs a paired result", len(blocks))
	}
	if !blocks[0].IsError {
		t.Error("a truncated call's result must be an error")
	}
	if !strings.Contains(blocks[0].ToolContent, "max_tokens") {
		t.Errorf("result = %q, want it to name the max_tokens limit as the cause", blocks[0].ToolContent)
	}
	if blocks[0].ToolResultID != "tu1" {
		t.Errorf("result id = %q, want tu1", blocks[0].ToolResultID)
	}
	// The loop continued rather than failing the run: the second turn ran and
	// finished cleanly.
	if p.calls != 2 {
		t.Errorf("provider calls = %d, want 2 — the model must get a chance to re-issue", p.calls)
	}
	if emit.count(KindDone) != 1 {
		t.Errorf("KindDone = %d, want 1", emit.count(KindDone))
	}
	if emit.count(KindError) != 0 {
		t.Errorf("KindError = %d, want 0 — a re-issuable truncation is not a run failure", emit.count(KindError))
	}
}

// TestLoopTruncatedBatchSkipsInteractionGate pins the ordering that makes the
// guard safe: the truncation check runs BEFORE the interaction gate. Otherwise a
// truncated ask_user or client-side tool call would suspend the run — parking a
// durable Interaction and putting a card in front of the user — for an action
// the model had not finished specifying.
//
// The arguments here are deliberately VALID: a message can be cut off just after
// a complete tool_use block, so the call parses cleanly and the existing
// malformed-arguments check does not catch it. That is the case only the stop
// reason can detect.
func TestLoopTruncatedBatchSkipsInteractionGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool toolruntime.Tool // nil for ask_user (built-in, needs no registration)
		call string
		args string
	}{
		{name: "ask_user", call: AskUserToolName, args: `{"questions":[{"q":"which one?"}]}`},
		{name: "client_tool", tool: clientSideTool{name: "browser_thing"}, call: "browser_thing", args: `{"seconds":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &scriptProvider{script: [][]provider.Event{
				truncatedToolUseResponse("tu1", tc.call, tc.args),
				textResponse("re-issued and finished"),
			}}
			reg := toolruntime.NewRegistry()
			if tc.tool != nil {
				reg.Register(tc.tool)
			}
			emit := &memEmitter{}
			loop := New(p, reg, Config{Model: "m", MaxTokens: 4096})

			if _, err := loop.Run(context.Background(), nil, emit); err != nil {
				t.Fatal(err)
			}
			if loop.PendingInteraction != nil {
				t.Errorf("run suspended on a call from a truncated message: %+v", loop.PendingInteraction)
			}
			if emit.count(KindInterrupt) != 0 {
				t.Error("a truncated message must not raise an interrupt frame")
			}
			if p.calls != 2 {
				t.Errorf("provider calls = %d, want 2 — the loop must continue so the model can re-issue", p.calls)
			}
		})
	}
}

// TestLoopTruncationWithoutCallsStillFails guards the asymmetry: with no tool
// calls a truncated turn is a cut-off final answer with nothing actionable to
// feed back, so it stays a run failure (the original L1 behaviour). Only a
// truncated tool batch is recoverable.
func TestLoopTruncationWithoutCallsStillFails(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{truncatedResponse("partial")}}
	emit := &memEmitter{}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	if _, err := loop.Run(context.Background(), nil, emit); err == nil {
		t.Fatal("a truncated answer with no tool calls must still fail the run")
	}
	if emit.count(KindError) != 1 {
		t.Errorf("KindError = %d, want 1", emit.count(KindError))
	}
}
