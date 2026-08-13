package toolruntime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeTool struct {
	name    string
	risk    Risk
	timeout time.Duration
	fn      func(ctx context.Context, args map[string]any) (Result, error)
}

func (f fakeTool) Name() string           { return f.name }
func (f fakeTool) Description() string    { return "fake" }
func (f fakeTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (f fakeTool) Risk() Risk             { return f.risk }
func (f fakeTool) Timeout() time.Duration { return f.timeout }
func (f fakeTool) Call(ctx context.Context, args map[string]any) (Result, error) {
	return f.fn(ctx, args)
}

func okTool(name string) fakeTool {
	return fakeTool{name: name, fn: func(context.Context, map[string]any) (Result, error) {
		return Result{Content: "ok"}, nil
	}}
}

func TestRegistryRegisterGet(t *testing.T) {
	r := NewRegistry()
	r.Register(okTool("a"))
	if _, ok := r.Get("a"); !ok {
		t.Error("tool a not found")
	}
	if _, ok := r.Get("missing"); ok {
		t.Error("unexpected hit for missing tool")
	}
	if len(r.All()) != 1 {
		t.Errorf("expected 1 tool, got %d", len(r.All()))
	}
}

func TestRegistryCallSuccess(t *testing.T) {
	r := NewRegistry()
	r.Register(okTool("read"))
	res := r.Call(context.Background(), "read", nil)
	if res.IsError || res.Content != "ok" {
		t.Errorf("unexpected result %+v", res)
	}
}

func TestRegistryCallUnknownTool(t *testing.T) {
	r := NewRegistry()
	res := r.Call(context.Background(), "nope", nil)
	if !res.IsError {
		t.Error("expected error result for unknown tool")
	}
}

func TestRegistryCallToolErrorBecomesErrorResult(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "boom", fn: func(context.Context, map[string]any) (Result, error) {
		return Result{}, errors.New("kaboom")
	}})
	res := r.Call(context.Background(), "boom", nil)
	if !res.IsError {
		t.Error("expected error result")
	}
}

func TestRegistryCallTimeout(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{
		name:    "slow",
		timeout: 20 * time.Millisecond,
		fn: func(ctx context.Context, _ map[string]any) (Result, error) {
			select {
			case <-ctx.Done():
				return Result{}, ctx.Err()
			case <-time.After(2 * time.Second):
				return Result{Content: "done"}, nil
			}
		},
	})
	start := time.Now()
	res := r.Call(context.Background(), "slow", nil)
	if !res.IsError {
		t.Error("expected timeout error result")
	}
	if time.Since(start) > time.Second {
		t.Error("timeout not enforced")
	}
}

func TestRegistryCallAllConcurrent(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "wait", fn: func(ctx context.Context, _ map[string]any) (Result, error) {
		time.Sleep(50 * time.Millisecond)
		return Result{Content: "w"}, nil
	}})
	calls := []Call{{ID: "1", Name: "wait"}, {ID: "2", Name: "wait"}, {ID: "3", Name: "wait"}}
	start := time.Now()
	results := r.CallAll(context.Background(), calls)
	if len(results) != 3 {
		t.Fatalf("got %d results", len(results))
	}
	for _, res := range results {
		if res.Content != "w" {
			t.Errorf("unexpected content %q", res.Content)
		}
	}
	// Concurrent: 3 x 50ms should take ~50ms, not 150ms.
	if time.Since(start) > 120*time.Millisecond {
		t.Errorf("calls did not run concurrently: %v", time.Since(start))
	}
}

// TestRegistryCallRespectsConcurrencyCap pins the per-registry semaphore: with
// a cap of 2, at most two tool executions overlap, and SetMaxConcurrent(0)
// restores unlimited parallelism.
func TestRegistryCallRespectsConcurrencyCap(t *testing.T) {
	var mu sync.Mutex
	var inFlight, peak int
	var blocked bool
	tool := fakeTool{name: "gauge", fn: func(ctx context.Context, _ map[string]any) (Result, error) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return Result{Content: "ok"}, nil
	}}

	capped := NewRegistry()
	capped.Register(tool)
	capped.SetMaxConcurrent(2)
	calls := make([]Call, 8)
	for i := range calls {
		calls[i] = Call{ID: fmt.Sprintf("%d", i), Name: "gauge"}
	}
	capped.CallAll(context.Background(), calls)
	mu.Lock()
	blocked = peak > 2
	mu.Unlock()
	if blocked {
		t.Errorf("peak in-flight = %d, want <= 2 under the cap", peak)
	}

	// Cap removed: 8 concurrent 20ms calls complete in ~20ms, not ~80ms.
	uncapped := NewRegistry()
	uncapped.Register(tool)
	uncapped.SetMaxConcurrent(0)
	start := time.Now()
	uncapped.CallAll(context.Background(), calls)
	if time.Since(start) > 40*time.Millisecond {
		t.Errorf("uncapped calls did not run in parallel: %v", time.Since(start))
	}
}
