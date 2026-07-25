package toolruntime

import (
	"context"
	"strings"
	"testing"
)

// TestRegistryCallRecoversFromPanic verifies a panicking tool becomes an error
// result rather than crashing the process.
func TestRegistryCallRecoversFromPanic(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "boom", fn: func(context.Context, map[string]any) (Result, error) {
		panic("kaboom")
	}})
	res := r.Call(context.Background(), "boom", nil)
	if !res.IsError {
		t.Fatal("expected error result from panicking tool")
	}
	if !strings.Contains(res.Content, "panicked") {
		t.Errorf("content = %q, want it to mention the panic", res.Content)
	}
}

// TestCallAllIsolatesPanickingTool verifies one panicking tool in a concurrent
// batch does not take down its siblings (each runs on its own goroutine).
func TestCallAllIsolatesPanickingTool(t *testing.T) {
	r := NewRegistry()
	r.Register(okTool("ok"))
	r.Register(fakeTool{name: "boom", fn: func(context.Context, map[string]any) (Result, error) {
		panic("kaboom")
	}})
	results := r.CallAll(context.Background(), []Call{{ID: "1", Name: "ok"}, {ID: "2", Name: "boom"}})
	if len(results) != 2 {
		t.Fatalf("got %d results", len(results))
	}
	if results[0].IsError || results[0].Content != "ok" {
		t.Errorf("healthy tool result affected by sibling panic: %+v", results[0])
	}
	if !results[1].IsError {
		t.Error("panicking tool did not yield an error result")
	}
}
