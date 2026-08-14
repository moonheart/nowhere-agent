package agent

import (
	"context"
	"sync"

	"nowhere-agent/internal/provider"
)

// UsageScope accumulates token usage from descendant agent loops (subagents)
// so the root run's terminal KindUsage report — and with it the persisted
// runs-row usage and any quota accounting — covers the whole run tree, not
// just the top-level loop's own model calls.
//
// The scope rides in the run context: Loop.Run installs one when the incoming
// context carries none (marking that run the root), and every nested child
// loop inherits the same instance. A subagent's spawn tool folds each child
// loop's terminal KindUsage into the scope; UsageMW adds the accumulated
// descendant total only at the root, so a child's own emission stays its own
// (no double counting). Safe for concurrent use: parallel subagents Add from
// their own goroutines.
type UsageScope struct {
	mu    sync.Mutex
	child provider.Usage
	root  bool
}

type usageScopeKey struct{}

// WithUsageScope returns ctx carrying the given scope.
func WithUsageScope(ctx context.Context, s *UsageScope) context.Context {
	return context.WithValue(ctx, usageScopeKey{}, s)
}

// UsageScopeFrom returns the scope on ctx, or nil when none is installed.
func UsageScopeFrom(ctx context.Context) *UsageScope {
	s, _ := ctx.Value(usageScopeKey{}).(*UsageScope)
	return s
}

// Add folds one descendant run's total usage into the scope.
func (s *UsageScope) Add(u provider.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.child.InputTokens += u.InputTokens
	s.child.OutputTokens += u.OutputTokens
	s.child.ReasoningTokens += u.ReasoningTokens
	s.child.CacheReadTokens += u.CacheReadTokens
	s.child.CacheWriteTokens += u.CacheWriteTokens
}

// Total returns the accumulated descendant usage.
func (s *UsageScope) Total() provider.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.child
}

// IsRoot reports whether this scope marks its run as the root of the run
// tree — the only level whose terminal usage report folds descendants in.
func (s *UsageScope) IsRoot() bool { return s.root }
