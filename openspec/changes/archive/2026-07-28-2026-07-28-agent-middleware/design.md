# agent-middleware — design

## Model: two kinds of hooks

Adapted from LangChain's middleware (node-style for observation, wrap-style for
control), Go-ified and fitted to the run-stateless loop.

```go
package agent

// ---- shared state -----------------------------------------------------------

// RunState is the mutable per-run state node-style hooks observe and update.
// It is NOT the durable conversation record; it is loop bookkeeping for one Run.
type RunState struct {
    View      []provider.Message // outgoing working view (transient, droppable)
    Produced  []provider.Message // assembled messages this run (durable-bound)
    Usage     provider.Usage     // accumulated across turns
    Iteration int
}

// ---- node-style hooks (observation) ----------------------------------------

// NodeHook runs sequentially at a fixed point. It observes and may mutate
// RunState; it never controls whether the wrapped call executes.
type NodeHooks interface {
    // BeforeModel runs before each provider call, after the view is assembled.
    BeforeModel(ctx context.Context, s *RunState) error
    // AfterModel runs after each provider call returns (before tool dispatch).
    AfterModel(ctx context.Context, s *RunState) error
    // AfterRun runs once, at natural termination or terminal error.
    AfterRun(ctx context.Context, s *RunState) error
}

// ---- wrap-style hooks (control) ---------------------------------------------

// ModelCall is one provider invocation. A WrapModelCall middleware receives it
// plus the next handler; it may transform the call, short-circuit (not call
// next), or call next multiple times (retry/fallback).
type ModelCall struct {
    Request provider.Request
    // View is the transient working view backing Request.Messages. Middleware
    // that rewrites it (compression, memory injection, image materialization)
    // must replace Request.Messages consistently. View is NEVER persisted.
    View []provider.Message
}

type ModelHandler func(ctx context.Context, c *ModelCall) (ModelResult, error)

type ModelResult struct {
    Assistant provider.Message
    Calls     []toolruntime.Call
    Stop      provider.StopReason
    Usage     *provider.Usage
}

type ModelCallMiddleware interface {
    WrapModelCall(ctx context.Context, c *ModelCall, next ModelHandler) (ModelResult, error)
}

// ToolCall is one tool invocation. A WrapToolCall middleware may authorize,
// transform, or short-circuit it.
type ToolCall struct {
    Call toolruntime.Call
    Tool toolruntime.Tool
}

type ToolHandler func(ctx context.Context, c *ToolCall) toolruntime.Result

type ToolCallMiddleware interface {
    WrapToolCall(ctx context.Context, c *ToolCall, next ToolHandler) toolruntime.Result
}
```

A single middleware type implements whichever of these it cares about; the loop
type-asserts at registration. This mirrors LangChain: one middleware can
contribute both a node hook and a wrap hook.

## Ordering

`Loop` holds `middleware []Middleware` in registration order. Per turn:

```
BeforeModel        : m1 → m2 → m3            (registration order)
WrapModelCall      : m1( m2( m3( realCall ))) (first registered = outermost)
AfterModel         : m3 → m2 → m1            (reverse)
WrapToolCall       : m1( m2( m3( realDispatch )))
AfterRun (once)    : m3 → m2 → m1            (reverse)
```

This makes today's implicit order explicit. Example: compression must run before
memory injection so the injector sees the compressed view, and image
materialization runs last (innermost, just before the real call) so it
materializes whatever the outer middleware produced:

```go
loop.Use(compressMW)   // outermost: shrinks the view first
loop.Use(memoryMW)     // appends recalled memories to the (compressed) view
loop.Use(imageMW)      // innermost: materializes image paths → base64
```

## Mapping today's inline logic to middleware

| Today (inline) | Becomes | Hook kind |
|---|---|---|
| Memory injection (`attempt` head) | `memoryMW` | `WrapModelCall` (rewrites `View`) |
| Image materialization (pre-send) | `imageMW` | `WrapModelCall` (rewrites `Request`) |
| Compression (`Run:302`) | `compressMW` | `WrapModelCall` (rewrites `View`) |
| Overflow retry (`Run:315`) | `overflowMW` | `WrapModelCall` (calls `next` up to N times) |
| Usage accounting (`Run:336`) | `usageMW` | `AfterModel` + `AfterRun` (node) |
| `KindMessage` persistence signal | `persistMW` | `AfterModel` (node) |

### Overflow retry as `WrapModelCall` (the canonical case)

```go
func (m overflowMW) WrapModelCall(ctx context.Context, c *ModelCall, next ModelHandler) (ModelResult, error) {
    res, err := next(ctx, c)
    for attempts := 0; err != nil && provider.IsContextOverflow(err) && attempts < m.max; attempts++ {
        shrunk, ok := contextmgmt.DropOldestRound(c.View)
        if !ok { break }
        c.View = shrunk
        c.Request.Messages = shrunk
        res, err = next(ctx, c)
    }
    return res, err
}
```

This is exactly LangChain's "call `handler` zero / one / many times" power, and
it removes the retry loop from `Run` entirely. The compression circuit breaker
(per-run failure count) lives in `compressMW`'s own per-run state — see "State
ownership" below.

## The contract that LangChain doesn't have: transient view vs durable record

Your loop has an invariant LangChain lacks: some middleware rewrites the
**outgoing working view** (memory injection, compression, image materialization)
which must **never** enter the durable conversation record, while others observe
the **durable-bound** produced messages (usage, `KindMessage` persistence).

The contract is enforced by *what each hook is allowed to touch*:

- `WrapModelCall` receives `*ModelCall` whose `View` is a **per-attempt copy**.
  Mutating it is always safe — it never reaches `Produced`. (Today `loop.go:199`
  already copies before appending injected memories; the middleware model keeps
  that copy-on-write.)
- `AfterModel`/`AfterRun` receive `*RunState` and may read `Produced` but must
  not rewrite already-assembled durable messages; they only append bookkeeping.

This is documented on the types (see `View` comment above) so a middleware
author cannot accidentally poison prompt-cache prefixes or the durable history.

## State ownership

Middleware instances are created **per `Loop`**, and a `Loop` runs one `Run` at
a time in your model (the registry enforces single-active-run). Per-run mutable
state (compression circuit-breaker count, usage accumulator) therefore lives on
the middleware value and is reset by an optional `Reset()` at `Run` start, OR is
kept in `RunState` and passed to node hooks. Recommendation: keep accumulators in
`RunState` (single source of truth, reset trivially), keep only *policy* (max
retries, thresholds) on the middleware value. This sidesteps the "one middleware
instance per run?" question entirely.

## Deferred: permission as middleware

The `Permission` callback is consulted **twice with different semantics**:
`interactionGate` (should this call end the run and ask a human?) and `dispatch`
(should this call execute?). A naive `WrapToolCall` only covers the second. The
first — ending the run for human input — is a run-stateless interrupt, not a
tool short-circuit. Doing it as middleware needs a sentinel
(`ErrEndRunForApproval`) that unwinds `WrapToolCall` → turn → `Run` and sets
`Loop.PendingApproval`. That is a real design decision with migration risk, so
it is **out of scope for this change**. `Config.Permission` and the two inline
call sites stay as-is for now; the middleware interfaces are designed so a later
`permissionMW` can slot in without changing them.

## Where the code lives

New subpackage `internal/agent/middleware` (the built-ins) + hook types in
`internal/agent` (so `Loop` can reference them without an import cycle). The
`builtin → agent` edge already exists (`plan.go`), so built-in middleware
importing `agent` for the hook types is consistent with the current graph.
