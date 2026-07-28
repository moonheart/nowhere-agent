package agent

import (
	"context"
	"errors"
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
