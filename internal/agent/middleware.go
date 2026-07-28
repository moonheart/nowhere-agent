package agent

import (
	"context"
	"log/slog"

	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// This file defines the agent-middleware capability (design: two kinds of
// hooks, adapted from LangChain's middleware model — node-style for
// observation, wrap-style for control). Middleware intercepts the
// think→tool→think lifecycle: model calls and tool calls. It is NOT HTTP
// middleware; nothing here touches the transport.
//
// THE contract that distinguishes transient view from durable record:
//
//   - WrapModelCall receives a *ModelCall whose View (and Request.Messages) is
//     a PER-ATTEMPT COPY. Mutating it is always safe — it never reaches the
//     durable conversation record. Compression, memory injection, and image
//     materialization rewrite the view this way without polluting history or
//     breaking the byte-stable prompt-caching prefix.
//   - Node hooks (AfterModel/AfterRun) receive a *RunState and may read
//     Produced, but must NOT rewrite already-assembled durable messages; they
//     only accumulate bookkeeping (usage, persistence signals).

// RunState is the mutable per-run state node-style hooks observe and update.
// It is loop bookkeeping for one Run, NOT the durable conversation record.
type RunState struct {
	// View is the outgoing working view for the current turn (transient,
	// droppable). Compression/injection rewrite this; it is never persisted.
	View []provider.Message
	// Produced accumulates the assembled messages this run (durable-bound):
	// each assistant message and each tool-result message, in order.
	Produced []provider.Message
	// Usage accumulates token usage across the run's provider calls.
	Usage provider.Usage
	// Iteration is the current loop iteration (0-based).
	Iteration int
	// Emit is the run's event emitter, exposed so AfterRun middleware can report
	// terminal signals (e.g. UsageMW emitting KindUsage).
	Emit Emitter
}

// Middleware is the marker every middleware satisfies. A concrete middleware
// implements one or more of the hook interfaces below; Loop type-asserts at
// registration and invokes whichever hooks are present.
type Middleware interface {
	// MiddlewareName identifies the middleware for logging/debugging.
	MiddlewareName() string
}

// ---- node-style hooks (observation) -----------------------------------------
// Each is a separate interface so a middleware implements only what it needs.

// BeforeModelHook runs before each provider call, after the view is assembled.
// Registration order: first → last.
type BeforeModelHook interface {
	BeforeModel(ctx context.Context, s *RunState) error
}

// AfterModelHook runs after each provider call returns, before tool dispatch.
// Registration order: last → first (reverse).
type AfterModelHook interface {
	AfterModel(ctx context.Context, s *RunState) error
}

// AfterRunHook runs once at natural termination or terminal error.
// Registration order: last → first (reverse).
type AfterRunHook interface {
	AfterRun(ctx context.Context, s *RunState) error
}

// ---- wrap-style hooks (control) ---------------------------------------------

// ModelCall is one provider invocation handed through the wrap chain.
type ModelCall struct {
	// Request is the provider request about to be sent. Middleware may rewrite
	// it (e.g. image materialization fills in base64 blocks).
	Request provider.Request
	// View is the transient working view backing Request.Messages. Middleware
	// that rewrites it (compression, memory injection) must keep
	// Request.Messages consistent. View is a per-attempt copy: NEVER persisted.
	View []provider.Message
}

// ModelResult is the assembled outcome of one provider call.
type ModelResult struct {
	Assistant provider.Message
	Calls     []toolruntime.Call
	Stop      provider.StopReason
	Usage     *provider.Usage
}

// ModelHandler invokes the next stage of the model-call chain. The innermost
// handler performs the real provider stream.
type ModelHandler func(ctx context.Context, c *ModelCall) (ModelResult, error)

// ModelCallMiddleware wraps a model call. It may transform the call, short-
// circuit (return without calling next), or call next multiple times (retry /
// fallback — e.g. the context-overflow fallback drops the oldest round and
// calls next again). Registration order nests: first registered is OUTERMOST.
type ModelCallMiddleware interface {
	WrapModelCall(ctx context.Context, c *ModelCall, next ModelHandler) (ModelResult, error)
}

// ToolCall is one tool invocation handed through the wrap chain.
type ToolCall struct {
	Call toolruntime.Call
	Tool toolruntime.Tool
}

// ToolHandler invokes the next stage of the tool-call chain; the innermost
// handler executes the tool.
type ToolHandler func(ctx context.Context, c *ToolCall) toolruntime.Result

// ToolCallMiddleware wraps a single tool call. It may authorize, transform, or
// short-circuit the call. Registration order nests: first registered is
// OUTERMOST.
type ToolCallMiddleware interface {
	WrapToolCall(ctx context.Context, c *ToolCall, next ToolHandler) toolruntime.Result
}

// ---- chain assembly ----------------------------------------------------------

// chainModel composes model-call middleware around the innermost real call.
// middleware[0] becomes the outermost layer.
func chainModel(mw []ModelCallMiddleware, inner ModelHandler) ModelHandler {
	h := inner
	for i := len(mw) - 1; i >= 0; i-- {
		m := mw[i]
		next := h
		h = func(ctx context.Context, c *ModelCall) (ModelResult, error) {
			return m.WrapModelCall(ctx, c, next)
		}
	}
	return h
}

// chainTool composes tool-call middleware around the innermost real dispatch.
// middleware[0] becomes the outermost layer.
func chainTool(mw []ToolCallMiddleware, inner ToolHandler) ToolHandler {
	h := inner
	for i := len(mw) - 1; i >= 0; i-- {
		m := mw[i]
		next := h
		h = func(ctx context.Context, c *ToolCall) toolruntime.Result {
			return m.WrapToolCall(ctx, c, next)
		}
	}
	return h
}

// ---- built-in middleware -----------------------------------------------------
// These re-express the loop's former inline cross-cutting concerns as
// middleware. They live in the agent package so Loop can reference them without
// an import cycle; optional/third-party middleware can live elsewhere.

// CompressMW compresses the working view when it crosses the context budget
// (WrapModelCall). Only the transient view is rewritten — the durable record is
// never touched (D1). A circuit breaker stops calling a failing summarizer
// after MaxFailures consecutive failures.
type CompressMW struct {
	Compressor  contextmgmt.Compressor
	Window      int     // model context window in tokens; <=0 disables
	MaxTokens   int     // reserved reply space
	Threshold   float64 // fraction of usable window that triggers compression
	KeepRecent  int     // recent rounds kept verbatim
	MaxFailures int     // circuit breaker; consecutive failures before tripping

	failures int // per-run consecutive failure count
}

func (m *CompressMW) MiddlewareName() string { return "compress" }

func (m *CompressMW) WrapModelCall(ctx context.Context, c *ModelCall, next ModelHandler) (ModelResult, error) {
	if m.Compressor == nil || m.Window <= 0 {
		return next(ctx, c)
	}
	if m.MaxFailures <= 0 {
		m.MaxFailures = 3
	}
	if m.Threshold <= 0 {
		m.Threshold = 0.8
	}
	if m.KeepRecent <= 0 {
		m.KeepRecent = 2 // recent rounds stay verbatim; older rounds are summarized
	}
	if m.failures >= m.MaxFailures {
		// Breaker tripped: don't even call the failing summarizer.
		return next(ctx, c)
	}
	// Usable window reserves room for the model's reply (design D5).
	budget := m.Window - m.MaxTokens
	if budget <= 0 {
		budget = m.Window
	}
	policy := contextmgmt.Policy{MaxTokens: budget, Threshold: m.Threshold, KeepRecent: m.KeepRecent}
	compressed, err := contextmgmt.Compress(ctx, c.View, policy, m.Compressor)
	if err != nil {
		m.failures++
		slog.Warn("agent: compression failed; using uncompressed view", "err", err, "failures", m.failures)
		return next(ctx, c)
	}
	m.failures = 0
	c.View = compressed
	c.Request.Messages = compressed
	return next(ctx, c)
}

// OverflowMW is the reactive context-overflow fallback (design D7): when the
// threshold trigger mis-estimated and the provider rejects the request as too
// large, drop the oldest round and call next again, up to MaxRetries.
type OverflowMW struct {
	MaxRetries int
}

func (m *OverflowMW) MiddlewareName() string { return "overflow" }

func (m *OverflowMW) WrapModelCall(ctx context.Context, c *ModelCall, next ModelHandler) (ModelResult, error) {
	maxRetries := m.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	res, err := next(ctx, c)
	for attempts := 0; err != nil && provider.IsContextOverflow(err) && attempts < maxRetries; attempts++ {
		shrunk, ok := contextmgmt.DropOldestRound(c.View)
		if !ok {
			break // nothing safe left to drop
		}
		c.View = shrunk
		c.Request.Messages = shrunk
		res, err = next(ctx, c)
	}
	return res, err
}

// MemoryInjectMW surfaces newly-created long-term memories into the outgoing
// view (WrapModelCall). The injected messages append to the transient view
// copy only — never to the durable record — keeping the durable history
// append-only and the prompt-caching prefix byte-stable.
type MemoryInjectMW struct {
	Injector  MemoryInjector
	SessionID string
}

func (m *MemoryInjectMW) MiddlewareName() string { return "memory-inject" }

func (m *MemoryInjectMW) WrapModelCall(ctx context.Context, c *ModelCall, next ModelHandler) (ModelResult, error) {
	if m.Injector == nil {
		return next(ctx, c)
	}
	extra, err := m.Injector.Inject(ctx, m.SessionID, c.View)
	if err != nil {
		slog.Warn("agent: memory injection failed; continuing without it", "err", err)
		return next(ctx, c)
	}
	if len(extra) > 0 {
		// c.View is already a per-attempt copy, so appending here never touches
		// the durable record.
		c.View = append(c.View, extra...)
		c.Request.Messages = c.View
	}
	return next(ctx, c)
}

// ImageMW materializes image blocks (path → base64) before the send
// (WrapModelCall, innermost). Byte-stable across turns for prompt caching.
type ImageMW struct {
	Resolver provider.ImageResolver
}

func (m *ImageMW) MiddlewareName() string { return "image" }

func (m *ImageMW) WrapModelCall(ctx context.Context, c *ModelCall, next ModelHandler) (ModelResult, error) {
	if m.Resolver == nil {
		return next(ctx, c)
	}
	c.Request = provider.MaterializeImages(ctx, c.Request, m.Resolver)
	c.View = c.Request.Messages
	return next(ctx, c)
}

// UsageMW reports the run's accumulated token usage once at run end via
// KindUsage (AfterRun). The loop folds each call's usage into RunState.Usage;
// this middleware only emits the total. It is registered automatically by Run
// (pointed at the run's emitter), so callers never wire it by hand. A zero
// total is not emitted.
type UsageMW struct{}

func (m *UsageMW) MiddlewareName() string { return "usage" }

func (m *UsageMW) AfterRun(ctx context.Context, s *RunState) error {
	if s.Emit == nil || s.Usage == (provider.Usage{}) {
		return nil
	}
	_ = s.Emit.Emit(ctx, KindUsage, s.Usage)
	return nil
}

