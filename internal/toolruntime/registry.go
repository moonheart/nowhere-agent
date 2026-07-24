package toolruntime

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const defaultTimeout = 30 * time.Second

// Registry holds the tools available to a run and dispatches calls to them.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register adds a tool, replacing any with the same name.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// All returns all registered tools.
func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	return out
}

// Call dispatches one tool call by name, applying the tool's timeout.
// Unknown tools and errors are returned as error Results so the model can
// self-correct rather than crashing the loop.
func (r *Registry) Call(ctx context.Context, name string, args map[string]any) Result {
	t, ok := r.Get(name)
	if !ok {
		return Result{Content: fmt.Sprintf("unknown tool: %s", name), IsError: true}
	}

	timeout := t.Timeout()
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := t.Call(ctx, args)
	if err != nil {
		return Result{Content: fmt.Sprintf("tool %s failed: %v", name, err), IsError: true}
	}
	return res
}

// CallAll executes multiple tool calls concurrently and returns results in the
// same order as the input calls.
func (r *Registry) CallAll(ctx context.Context, calls []Call) []Result {
	results := make([]Result, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func(i int, c Call) {
			defer wg.Done()
			// Expose the call's id on the ctx so a tool that emits progress (the
			// spawn tool) can tag its output frames with the tool-call they belong
			// to, letting the UI nest them under the right call when several run
			// in parallel.
			results[i] = r.Call(ContextWithCallID(ctx, c.ID), c.Name, c.Args)
		}(i, c)
	}
	wg.Wait()
	return results
}

// Call is a single requested tool invocation.
type Call struct {
	ID   string
	Name string
	Args map[string]any
}

// callIDKey is the ctx key carrying the in-flight tool call's id.
type callIDKey struct{}

// ContextWithCallID returns ctx carrying the tool call id being dispatched.
func ContextWithCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callIDKey{}, id)
}

// CallIDFrom returns the tool call id placed by ContextWithCallID, or "".
func CallIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(callIDKey{}).(string)
	return s
}
