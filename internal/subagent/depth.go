// Package subagent implements the subagent capability: a built-in spawn_agent
// tool whose Call runs a child agent.Loop over an isolated, prompt-only context
// and returns the child's final assistant text as the tool result. A subagent
// is just another toolruntime.Tool — the parent loop dispatches it like any
// other, so concurrency, cancellation, and timeouts come from the existing
// runtime for free.
package subagent

import "context"

// depthKey carries the current subagent nesting depth through the run context.
// The top-level run is depth 0; each spawn increments it.
type depthKey struct{}

// withDepth returns ctx carrying the given subagent depth.
func withDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, depthKey{}, depth)
}

// depthFrom reads the subagent depth from ctx, defaulting to 0.
func depthFrom(ctx context.Context) int {
	d, _ := ctx.Value(depthKey{}).(int)
	return d
}
