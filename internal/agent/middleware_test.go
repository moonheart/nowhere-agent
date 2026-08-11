package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// recordingMW records the order its hooks fire, tagging each entry with its id.
type recordingMW struct {
	name string
	log  *[]string
}

func (m recordingMW) MiddlewareName() string { return m.name }

func (m recordingMW) BeforeModel(_ context.Context, _ *RunState) error {
	*m.log = append(*m.log, "before:"+m.name)
	return nil
}

func (m recordingMW) AfterModel(_ context.Context, _ *RunState) error {
	*m.log = append(*m.log, "after:"+m.name)
	return nil
}

func (m recordingMW) WrapModelCall(ctx context.Context, c *ModelCall, next ModelHandler) (ModelResult, error) {
	*m.log = append(*m.log, "wrap-in:"+m.name)
	res, err := next(ctx, c)
	*m.log = append(*m.log, "wrap-out:"+m.name)
	return res, err
}

// TestChainModelNestsFirstOutermost verifies wrap middleware composes so the
// first-registered middleware is the outermost layer.
func TestChainModelNestsFirstOutermost(t *testing.T) {
	var log []string
	mw := []ModelCallMiddleware{
		recordingMW{name: "m1", log: &log},
		recordingMW{name: "m2", log: &log},
		recordingMW{name: "m3", log: &log},
	}
	inner := func(_ context.Context, _ *ModelCall) (ModelResult, error) {
		log = append(log, "real")
		return ModelResult{Assistant: provider.TextMessage(provider.RoleAssistant, "hi")}, nil
	}
	h := chainModel(mw, inner)
	if _, err := h(context.Background(), &ModelCall{}); err != nil {
		t.Fatalf("chain call: %v", err)
	}
	want := []string{
		"wrap-in:m1", "wrap-in:m2", "wrap-in:m3", "real",
		"wrap-out:m3", "wrap-out:m2", "wrap-out:m1",
	}
	assertOrder(t, want, log)
}

// TestWrapShortCircuit verifies a wrap middleware may decline to call next,
// short-circuiting the inner call entirely.
func TestWrapShortCircuit(t *testing.T) {
	called := false
	inner := func(_ context.Context, _ *ModelCall) (ModelResult, error) {
		called = true
		return ModelResult{}, nil
	}
	short := ModelCallMiddleware(wrapFunc(func(_ context.Context, _ *ModelCall, _ ModelHandler) (ModelResult, error) {
		return ModelResult{Assistant: provider.TextMessage(provider.RoleAssistant, "cached")}, nil
	}))
	h := chainModel([]ModelCallMiddleware{short}, inner)
	res, err := h(context.Background(), &ModelCall{})
	if err != nil {
		t.Fatalf("chain call: %v", err)
	}
	if called {
		t.Fatal("inner handler must not run when middleware short-circuits")
	}
	if res.Assistant.Content[0].Text != "cached" {
		t.Fatalf("short-circuit result = %q, want %q", res.Assistant.Content[0].Text, "cached")
	}
}

// TestWrapRetry verifies a wrap middleware may call next multiple times (the
// retry/fallback power the overflow middleware relies on).
func TestWrapRetry(t *testing.T) {
	calls := 0
	inner := func(_ context.Context, _ *ModelCall) (ModelResult, error) {
		calls++
		if calls < 3 {
			return ModelResult{}, errors.New("overflow")
		}
		return ModelResult{Assistant: provider.TextMessage(provider.RoleAssistant, "ok")}, nil
	}
	retry := ModelCallMiddleware(wrapFunc(func(ctx context.Context, c *ModelCall, next ModelHandler) (ModelResult, error) {
		var res ModelResult
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if res, err = next(ctx, c); err == nil {
				return res, nil
			}
		}
		return res, err
	}))
	h := chainModel([]ModelCallMiddleware{retry}, inner)
	res, err := h(context.Background(), &ModelCall{})
	if err != nil {
		t.Fatalf("retry chain: %v", err)
	}
	if calls != 3 {
		t.Fatalf("inner called %d times, want 3", calls)
	}
	if res.Assistant.Content[0].Text != "ok" {
		t.Fatalf("result = %q, want ok", res.Assistant.Content[0].Text)
	}
}

// TestChainToolNestsFirstOutermost verifies tool middleware nests like model
// middleware: first registered is outermost.
func TestChainToolNestsFirstOutermost(t *testing.T) {
	var log []string
	mw := []ToolCallMiddleware{
		toolWrapFunc{name: "m1", log: &log},
		toolWrapFunc{name: "m2", log: &log},
	}
	inner := func(_ context.Context, _ *ToolCall) toolruntime.Result {
		log = append(log, "real")
		return toolruntime.Result{Content: "done"}
	}
	h := chainTool(mw, inner)
	res := h(context.Background(), &ToolCall{})
	if res.Content != "done" {
		t.Fatalf("result = %q, want done", res.Content)
	}
	want := []string{"in:m1", "in:m2", "real", "out:m2", "out:m1"}
	assertOrder(t, want, log)
}

// wrapFunc adapts a function to ModelCallMiddleware (test helper).
type wrapFunc func(ctx context.Context, c *ModelCall, next ModelHandler) (ModelResult, error)

func (f wrapFunc) MiddlewareName() string { return "wrapFunc" }
func (f wrapFunc) WrapModelCall(ctx context.Context, c *ModelCall, next ModelHandler) (ModelResult, error) {
	return f(ctx, c, next)
}

// toolWrapFunc adapts a recording closure to ToolCallMiddleware (test helper).
type toolWrapFunc struct {
	name string
	log  *[]string
}

func (m toolWrapFunc) MiddlewareName() string { return m.name }
func (m toolWrapFunc) WrapToolCall(ctx context.Context, c *ToolCall, next ToolHandler) toolruntime.Result {
	*m.log = append(*m.log, "in:"+m.name)
	res := next(ctx, c)
	*m.log = append(*m.log, "out:"+m.name)
	return res
}

func assertOrder(t *testing.T, want, got []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order length = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestChainModelPanicIsolation pins that a panicking model-call middleware is
// converted to an error at its own layer instead of crashing the run.
func TestChainModelPanicIsolation(t *testing.T) {
	boom := wrapFunc(func(context.Context, *ModelCall, ModelHandler) (ModelResult, error) {
		panic("kaboom")
	})
	innerCalled := false
	inner := func(context.Context, *ModelCall) (ModelResult, error) {
		innerCalled = true
		return ModelResult{}, nil
	}
	h := chainModel([]ModelCallMiddleware{boom}, inner)
	if _, err := h(context.Background(), &ModelCall{}); err == nil ||
		!strings.Contains(err.Error(), "kaboom") || !strings.Contains(err.Error(), "wrapFunc") {
		t.Fatalf("panic should surface as a named layer error, got %v", err)
	}
	if innerCalled {
		t.Fatal("inner handler must not run when the outer layer panics before next")
	}
}

// TestChainToolPanicIsolation pins that a panicking tool-call middleware
// becomes an error tool-result for that call (the dispatch fan-out runs one
// goroutine per call — an unrecovered panic would kill the process).
func TestChainToolPanicIsolation(t *testing.T) {
	boom := panicToolWrap{}
	inner := func(context.Context, *ToolCall) toolruntime.Result {
		return toolruntime.Result{Content: "done"}
	}
	h := chainTool([]ToolCallMiddleware{boom}, inner)
	res := h(context.Background(), &ToolCall{})
	if !res.IsError || !strings.Contains(res.Content, "kaboom") || !strings.Contains(res.Content, "panicTool") {
		t.Fatalf("panic should become a named error result, got %+v", res)
	}
}

// panicToolWrap is a ToolCallMiddleware that always panics (test helper).
type panicToolWrap struct{}

func (panicToolWrap) MiddlewareName() string { return "panicTool" }
func (panicToolWrap) WrapToolCall(context.Context, *ToolCall, ToolHandler) toolruntime.Result {
	panic("kaboom")
}

// TestUsePartitionStability pins the Use partition contract: a middleware lands
// in exactly the chains whose hook interfaces it implements, once per chain.
func TestUsePartitionStability(t *testing.T) {
	loop := New(&recordingProvider{reply: "x"}, toolruntime.NewRegistry(), Config{Model: "m"})
	var log []string
	loop.Use(recordingMW{name: "multi", log: &log})
	if len(loop.before) != 1 {
		t.Errorf("before chain = %d, want 1", len(loop.before))
	}
	if len(loop.afterModel) != 1 {
		t.Errorf("afterModel chain = %d, want 1", len(loop.afterModel))
	}
	if len(loop.modelWrap) != 1 {
		t.Errorf("modelWrap chain = %d, want 1", len(loop.modelWrap))
	}
	if len(loop.toolWrap) != 0 {
		t.Errorf("toolWrap chain = %d, want 0 (recordingMW has no tool hook)", len(loop.toolWrap))
	}
	// New() registers UsageMW, an AfterRun hook.
	if len(loop.afterRun) != 1 {
		t.Errorf("afterRun chain = %d, want 1 (UsageMW only)", len(loop.afterRun))
	}
	if loop.gateInteraction != nil {
		t.Error("gate should be nil when no GateFuncProvider is registered")
	}
}

// TestUseGateFirstRegisteredWins pins that the first GateFuncProvider supplies
// the policy and a later one is ignored (with a warning, not silent reuse).
func TestUseGateFirstRegisteredWins(t *testing.T) {
	loop := New(&recordingProvider{reply: "x"}, toolruntime.NewRegistry(), Config{Model: "m"})
	loop.Use(
		&PermissionMW{Check: func(context.Context, toolruntime.Tool) (bool, string) { return true, "first" }},
		&PermissionMW{Check: func(context.Context, toolruntime.Tool) (bool, string) { return true, "second" }},
	)
	if loop.Gate() == nil {
		t.Fatal("gate should be registered")
	}
	_, reason := loop.Gate()(context.Background(), echoTool{})
	if reason != "first" {
		t.Errorf("gate reason = %q, want %q (first registered wins)", reason, "first")
	}
}

// beforeHookFunc/afterModelHookFunc/afterRunHookFunc adapt closures to the
// node-hook interfaces (test helpers).
type beforeHookFunc func(context.Context, *RunState) error

func (f beforeHookFunc) MiddlewareName() string { return "beforeHook" }
func (f beforeHookFunc) BeforeModel(ctx context.Context, s *RunState) error {
	return f(ctx, s)
}

type afterModelHookFunc func(context.Context, *RunState) error

func (f afterModelHookFunc) MiddlewareName() string { return "afterModelHook" }
func (f afterModelHookFunc) AfterModel(ctx context.Context, s *RunState) error {
	return f(ctx, s)
}

type afterRunHookFunc func(context.Context, *RunState) error

func (f afterRunHookFunc) MiddlewareName() string { return "afterRunHook" }
func (f afterRunHookFunc) AfterRun(ctx context.Context, s *RunState) error {
	return f(ctx, s)
}

// TestUsageHookFuncAdaptsClosure pins the func-to-middleware adapter: a
// one-shot observer (metrics reporting) registers through Use and fires once
// with the run's final state at termination.
func TestUsageHookFuncAdaptsClosure(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{usageTextResponse("hi", 10, 3)}}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	var fired int
	var gotUsage provider.Usage
	loop.Use(UsageHookFunc(func(_ context.Context, s *RunState) error {
		fired++
		gotUsage = s.Usage
		return nil
	}))

	emit := &memEmitter{}
	if _, err := loop.Run(context.Background(), nil, emit); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fired != 1 {
		t.Fatalf("hook fired %d times, want exactly once", fired)
	}
	if gotUsage.InputTokens != 10 || gotUsage.OutputTokens != 3 {
		t.Fatalf("hook saw usage %+v, want the run's aggregate", gotUsage)
	}
	if emit.count(KindDone) != 1 {
		t.Fatalf("KindDone frames = %d, want 1", emit.count(KindDone))
	}
}

// TestBeforeModelAbortRun pins the ErrAbortRun sentinel: a BeforeModel hook
// returning it (wrapped) aborts the run before the provider is called, with a
// terminal KindError frame.
func TestBeforeModelAbortRun(t *testing.T) {
	rp := &recordingProvider{reply: "hi"}
	loop := New(rp, toolruntime.NewRegistry(), Config{Model: "m"})
	loop.Use(beforeHookFunc(func(context.Context, *RunState) error {
		return fmt.Errorf("policy breach: %w", ErrAbortRun)
	}))
	emit := &memEmitter{}
	if _, err := loop.Run(context.Background(), nil, emit); !errors.Is(err, ErrAbortRun) {
		t.Fatalf("run error = %v, want wrapped ErrAbortRun", err)
	}
	if len(rp.requests) != 0 {
		t.Error("provider must not be called when BeforeModel aborts")
	}
	if emit.count(KindError) != 1 {
		t.Errorf("KindError frames = %d, want 1", emit.count(KindError))
	}
	if emit.count(KindDone) != 0 {
		t.Error("an aborted run must not emit KindDone")
	}
}

// TestBeforeModelPlainErrorDoesNotAbort pins that a non-sentinel hook error is
// still logged and skipped — the run completes.
func TestBeforeModelPlainErrorDoesNotAbort(t *testing.T) {
	rp := &recordingProvider{reply: "hi"}
	loop := New(rp, toolruntime.NewRegistry(), Config{Model: "m"})
	loop.Use(beforeHookFunc(func(context.Context, *RunState) error {
		return errors.New("transient observer failure")
	}))
	emit := &memEmitter{}
	if _, err := loop.Run(context.Background(), nil, emit); err != nil {
		t.Fatalf("plain hook error must not abort the run: %v", err)
	}
	if emit.count(KindDone) != 1 {
		t.Errorf("KindDone frames = %d, want 1", emit.count(KindDone))
	}
}

// TestAfterModelAbortRun pins that an AfterModel abort answers the already-
// durable tool batch before failing the run (no unpaired tool_use), and the
// batch is never dispatched.
func TestAfterModelAbortRun(t *testing.T) {
	sp := &scriptProvider{script: [][]provider.Event{toolUseResponse("t1", "echo", "{}")}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	loop := New(sp, reg, Config{Model: "m"})
	loop.Use(afterModelHookFunc(func(context.Context, *RunState) error {
		return fmt.Errorf("halt: %w", ErrAbortRun)
	}))
	emit := &memEmitter{}
	if _, err := loop.Run(context.Background(), nil, emit); !errors.Is(err, ErrAbortRun) {
		t.Fatalf("run error = %v, want wrapped ErrAbortRun", err)
	}
	if sp.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (no iteration after abort)", sp.calls)
	}
	if emit.count(KindToolResult) != 1 {
		t.Errorf("KindToolResult frames = %d, want 1 (batch answered synthetically)", emit.count(KindToolResult))
	}
	if emit.count(KindError) != 1 || emit.count(KindDone) != 0 {
		t.Errorf("terminal frames: error=%d done=%d, want 1/0", emit.count(KindError), emit.count(KindDone))
	}
}

// TestAfterRunAbortSkipsRemaining pins that ErrAbortRun from an AfterRun hook
// stops the remaining AfterRun hooks (the run itself is already ending) but
// does not fail an otherwise successful run.
func TestAfterRunAbortSkipsRemaining(t *testing.T) {
	rp := &recordingProvider{reply: "hi"}
	loop := New(rp, toolruntime.NewRegistry(), Config{Model: "m"})
	var fired []string
	loop.Use(afterRunHookFunc(func(context.Context, *RunState) error {
		fired = append(fired, "early")
		return nil
	}))
	loop.Use(afterRunHookFunc(func(context.Context, *RunState) error {
		fired = append(fired, "late")
		return ErrAbortRun
	}))
	emit := &memEmitter{}
	if _, err := loop.Run(context.Background(), nil, emit); err != nil {
		t.Fatalf("AfterRun abort must not fail a completed run: %v", err)
	}
	// Reverse order: "late" fires first and aborts; "early" is skipped.
	assertOrder(t, []string{"late"}, fired)
	if emit.count(KindDone) != 1 {
		t.Errorf("KindDone frames = %d, want 1", emit.count(KindDone))
	}
}
