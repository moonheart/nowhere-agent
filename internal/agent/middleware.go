package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"

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
	// compressCache carries the run's compression summary across iterations
	// (see CompressMW), so a summary is reused or incrementally extended
	// instead of re-summarized from scratch every iteration.
	compressCache *contextmgmt.CompressionCache
	// viewDropped counts leading history messages the overflow fallback dropped
	// with no compression cache to carry the drop (compression disabled). The
	// loop's view rebuild skips them, or every iteration would re-overflow on
	// the same prefix and burn the retry budget again.
	viewDropped int
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

// ---- gate hook (tool authorization) ------------------------------------------
// A GateFunc authorizes one tool: (deny, reason). deny=true blocks the call.
// It receives the run's context so a policy may resolve request/run-scoped
// inputs (e.g. the owning session's permission-mode setting) at call time rather
// than at middleware-registration time. The loop consults the policy at two
// points with different semantics: the interaction gate (a deny whose reason
// carries the ApprovalReasonPrefix marker ends the run for human input — a
// general interrupt) and the execution gate (any other deny blocks dispatch,
// feeding the reason back to the model). A middleware supplies the policy by
// exposing GateFuncProvider.
type GateFunc func(ctx context.Context, tool toolruntime.Tool) (bool, string)

// GateFuncProvider supplies the tool-authorization policy to the loop. The loop
// uses the ONE returned func at both gate points (interaction and execution) —
// a single policy governs both, so there is no separate per-gate registration.
type GateFuncProvider interface {
	GateCheck() GateFunc
}

// ---- wrap-style hooks (control) ---------------------------------------------

// ModelCall is one provider invocation handed through the wrap chain.
type ModelCall struct {
	// Request is the provider request about to be sent. Middleware may rewrite
	// it (e.g. image materialization fills in base64 blocks).
	Request provider.Request
	// View is the transient working view backing Request.Messages. Middleware
	// that rewrites it (compression, memory injection) must keep
	// Request.Messages consistent. View is a per-attempt copy (down to block
	// granularity, so in-place block mutation is safe; nested reference values
	// inside a block are shared and read-only): NEVER persisted.
	View []provider.Message
	// State is the run's bookkeeping, exposed so wrap middleware can carry
	// run-scoped state across iterations (CompressMW's summary cache). Nil
	// when a ModelCall is constructed outside a Run (tests).
	State *RunState
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

// CircuitBreaker carries the compressor failure count. Production rebuilds the
// loop — and CompressMW — per run, so a count held on the middleware instance
// resets every run and never trips; one breaker shared across instances (wired
// at the server) keeps a persistently failing summarizer tripped.
type CircuitBreaker struct {
	mu       sync.Mutex
	failures int
}

// tripped reports whether consecutive failures reached max.
func (b *CircuitBreaker) tripped(max int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures >= max
}

// record folds one compression outcome into the count and returns the new
// consecutive-failure total (for logging).
func (b *CircuitBreaker) record(failed bool) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if failed {
		b.failures++
	} else {
		b.failures = 0
	}
	return b.failures
}

// CompressMW compresses the working view when it crosses the context budget
// (WrapModelCall). Only the transient view is rewritten — the durable record is
// never touched (D1). The summary is carried across iterations via the run's
// CompressionCache: reused while the growing tail still fits the budget
// (byte-stable prompt prefix) and extended incrementally when it does not,
// instead of re-summarizing the whole history every iteration. A circuit
// breaker stops calling a failing summarizer after MaxFailures consecutive
// failures.
type CompressMW struct {
	Compressor  contextmgmt.Compressor
	Window      int     // model context window in tokens; <=0 disables
	MaxTokens   int     // reserved reply space
	Threshold   float64 // fraction of usable window that triggers compression
	KeepRecent  int     // recent rounds kept verbatim
	MaxFailures int     // circuit breaker; consecutive failures before tripping
	// Breaker, when non-nil, carries the failure count across runs. Nil uses a
	// lazily-created per-instance breaker (single-loop use, tests).
	Breaker *CircuitBreaker

	breaker *CircuitBreaker // lazy per-instance breaker
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
	br := m.Breaker
	if br == nil {
		if m.breaker == nil {
			m.breaker = &CircuitBreaker{}
		}
		br = m.breaker
	}
	if br.tripped(m.MaxFailures) {
		// Breaker tripped: don't even call the failing summarizer.
		return next(ctx, c)
	}
	// Usable window reserves room for the model's reply (design D5).
	budget := m.Window - m.MaxTokens
	if budget <= 0 {
		budget = m.Window
	}
	// The system prompt and tool schemas ride on every request but live
	// outside the message view the estimator sees; without subtracting them
	// the trigger fires late and the overflow fallback has to rescue the
	// request by dropping rounds. If the envelope alone busts the budget there
	// is nothing compression can do — keep the full budget and let it pass.
	if overhead := contextmgmt.EstimateOverhead(c.Request.System, c.Request.Tools); overhead < budget {
		budget -= overhead
	}
	policy := contextmgmt.Policy{MaxTokens: budget, Threshold: m.Threshold, KeepRecent: m.KeepRecent}
	var cache *contextmgmt.CompressionCache
	if c.State != nil {
		if c.State.compressCache == nil {
			c.State.compressCache = &contextmgmt.CompressionCache{}
		}
		cache = c.State.compressCache
	}
	compressed, err := contextmgmt.CompressWithCache(ctx, c.View, policy, m.Compressor, cache)
	if err != nil {
		n := br.record(true)
		slog.Warn("agent: compression failed; using uncompressed view", "err", err, "failures", n)
		return next(ctx, c)
	}
	br.record(false)
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
		var mse *midStreamError
		if errors.As(err, &mse) {
			// The overflow surfaced mid-stream, after deltas already reached
			// the client: retrying with a shrunk view would re-emit them. Fail
			// the run rather than duplicate output.
			break
		}
		// Preserve a leading compression summary: it is the only record of the
		// already-dropped history, so dropping it first would silently erase
		// the oldest context while keeping verbatim what could be re-dropped.
		// Only when nothing else remains does the plain drop take the summary.
		shrunk, ok := contextmgmt.DropOldestRoundPreservingSummary(c.View)
		if !ok {
			shrunk, ok = contextmgmt.DropOldestRound(c.View)
		}
		if !ok {
			break // nothing safe left to drop
		}
		// Keep the compression cache aligned with what is actually sent:
		// rounds dropped here must not re-enter the view next iteration via
		// the cache's hysteresis rebuild (which re-derives the view from
		// durable history). If the summary itself was dropped, the cache no
		// longer describes the view at all and must be rebuilt from scratch.
		if c.State != nil && c.State.compressCache != nil && c.State.compressCache.Covered > 0 &&
			len(c.View) > 0 && contextmgmt.IsSummary(c.View[0]) {
			cache := c.State.compressCache
			if len(shrunk) > 0 && contextmgmt.IsSummary(shrunk[0]) {
				if n := len(c.View) - len(shrunk); n > 0 {
					cache.Advance(c.View[1 : 1+n])
				}
			} else {
				cache.Invalidate()
			}
		}
		// With no compression cache to carry the drop (compression disabled or
		// absent), record the dropped prefix on the run state: the view is
		// rebuilt from durable history every iteration, so without this the
		// dropped rounds would return next iteration and overflow again. When a
		// cache exists the Advance/Invalidate above already accounts for it —
		// recording here too would double-drop against its history-relative
		// coverage.
		if c.State != nil && c.State.compressCache == nil {
			c.State.viewDropped += len(c.View) - len(shrunk)
		}
		c.View = shrunk
		c.Request.Messages = shrunk
		res, err = next(ctx, c)
	}
	return res, err
}

// MemoryInjectMW surfaces newly-created long-term memories into the outgoing
// view (BeforeModel). It runs BEFORE the wrap chain — i.e. before compression —
// so the injected memories are counted against the context budget instead of
// inflating the request past it, and so they ride inside the compressed view
// rather than vanishing when compression rebuilds it. The injected messages
// append to the per-iteration transient view only — never to the durable
// record — keeping the durable history append-only and the prompt-caching
// prefix byte-stable.
type MemoryInjectMW struct {
	Injector  MemoryInjector
	SessionID string
}

func (m *MemoryInjectMW) MiddlewareName() string { return "memory-inject" }

func (m *MemoryInjectMW) BeforeModel(ctx context.Context, s *RunState) error {
	if m.Injector == nil {
		return nil
	}
	extra, err := m.Injector.Inject(ctx, m.SessionID, s.View)
	if err != nil {
		slog.Warn("agent: memory injection failed; continuing without it", "err", err)
		return nil
	}
	if len(extra) > 0 {
		// s.View is rebuilt from the durable record every iteration, so
		// appending here never touches it.
		s.View = append(s.View, extra...)
	}
	return nil
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

// PermissionMW is the built-in tool-authorization middleware. It carries the
// policy callback and exposes it to the loop via GateFuncProvider: the loop
// applies the ONE policy at both gate points — the interaction gate (a deny
// with the ApprovalReasonPrefix marker ends the run for human input, a general
// interrupt) and the execution gate (any other deny blocks dispatch, feeding the
// reason back to the model as an error tool-result). It does not own the run
// orchestration — the loop keeps the unified suspend point and the run-
// termination semantics; PermissionMW only answers "is this tool allowed, and
// why".
type PermissionMW struct {
	Check GateFunc
}

func (m *PermissionMW) MiddlewareName() string { return "permission" }

// GateCheck supplies the policy to the loop (used at both gate points).
func (m *PermissionMW) GateCheck() GateFunc { return m.Check }
