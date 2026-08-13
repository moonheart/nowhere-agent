package toolruntime

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"nowhere-agent/internal/provider"
)

const defaultTimeout = 30 * time.Second

// defaultMaxConcurrent bounds in-flight tool executions per registry when no
// explicit cap is set (HTTP_TOOL_MAX_CONCURRENT). A model batch of 30 calls
// would otherwise spawn one goroutine per call with no upper bound.
const defaultMaxConcurrent = 8

// Registry holds the tools available to a run and dispatches calls to them.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	// sem, when non-nil, caps concurrent tool executions: Call acquires a
	// slot before dispatch and releases it when the call returns. A nil sem
	// (SetMaxConcurrent(0)) means unlimited. The cap is per registry — each
	// run owns one — so a large batch executes at most maxConcurrent tools at
	// once instead of a goroutine per call.
	sem chan struct{}
}

// NewRegistry creates an empty Registry with the default concurrency cap.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}, sem: make(chan struct{}, defaultMaxConcurrent)}
}

// SetMaxConcurrent caps the number of tool executions this registry runs in
// parallel (0 or negative = unlimited). Call it before dispatching; a change
// applies to subsequent calls.
func (r *Registry) SetMaxConcurrent(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 {
		r.sem = nil
		return
	}
	r.sem = make(chan struct{}, n)
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

// All returns all registered tools, sorted by name. A stable order keeps the
// serialized tool definitions byte-identical across requests, so the LLM's
// prompt-prefix cache (which requires a byte-identical prefix and reads the
// tools array ahead of the messages) can actually hit.
func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// semaphore returns the concurrency-cap channel, or nil when unlimited.
func (r *Registry) semaphore() chan struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sem
}

// Call dispatches one tool call by name, applying the tool's timeout.
// Unknown tools, errors, and panics are returned as error Results so the model
// can self-correct rather than crashing the loop (or, since CallAll dispatches
// each call on its own goroutine, the whole process).
func (r *Registry) Call(ctx context.Context, name string, args map[string]any) (res Result) {
	t, ok := r.Get(name)
	if !ok {
		return Result{Content: fmt.Sprintf("unknown tool: %s (available tools: %s)", name, strings.Join(r.Names(), ", ")), IsError: true}
	}

	// Concurrency cap: acquire a slot before executing. A cancelled ctx while
	// queued is reported as a failed call rather than starting the tool.
	if sem := r.semaphore(); sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return Result{Content: fmt.Sprintf("tool %s failed: %v", name, ctx.Err()), IsError: true}
		}
	}

	timeout := t.Timeout()
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// A panicking tool must not crash the process. Convert the panic into an
	// error result (the named return) the model sees, mirroring how a returned
	// error is handled; log with a stack for the operator.
	defer func() {
		if p := recover(); p != nil {
			slog.Error("tool panicked", "tool", name, "panic", p, "stack", string(debug.Stack()))
			res = Result{Content: fmt.Sprintf("tool %s panicked: %v", name, p), IsError: true}
		}
	}()

	result, err := t.Call(ctx, args)
	if err != nil {
		return Result{Content: fmt.Sprintf("tool %s failed: %v", name, err), IsError: true}
	}
	return result
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
	// ArgsError, when non-empty, means the model's tool-call arguments could not
	// be parsed (malformed JSON). The loop turns such a call into an is_error
	// tool_result without dispatching it, so the model can retry with valid args.
	ArgsError string
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

// generativeUIKey is the ctx key carrying a pusher for live agent-driven UI
// updates (the loop's emitter, threaded into the tool call).
type generativeUIKey struct{}

// GenerativeUIPusher pushes one generative-UI spec to the client DURING a tool
// call. Pushes are live-only (broker frames); the durable spec still rides the
// tool's final Result.GenerativeUI, so a reload re-renders the settled state.
type GenerativeUIPusher func(spec *provider.GenerativeUISpec)

// ContextWithGenerativeUI returns ctx carrying a pusher the tool can call to
// stream live agent-driven UI updates (e.g. a progress card) while it runs.
func ContextWithGenerativeUI(ctx context.Context, push GenerativeUIPusher) context.Context {
	return context.WithValue(ctx, generativeUIKey{}, push)
}

// GenerativeUIFrom returns the pusher placed by ContextWithGenerativeUI, or
// nil when the caller did not dispatch through the loop (e.g. a direct
// registry call in a test).
func GenerativeUIFrom(ctx context.Context) GenerativeUIPusher {
	p, _ := ctx.Value(generativeUIKey{}).(GenerativeUIPusher)
	return p
}
