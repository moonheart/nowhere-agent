# agent-middleware — proposal

## Why

`agent.Loop` already carries six cross-cutting concerns, but they are hand-wired
into the loop body and assembled through a growing `Config` struct plus `With*`
setters:

| Concern | Where it lives today | Injected via |
|---|---|---|
| Memory injection | `attempt()` head (`loop.go:195`) | `WithMemoryInjector` |
| Image materialization | `attempt()` pre-send (`loop.go:212`) | `WithImages` |
| Compression | `Run` main loop (`loop.go:302`) | `Config.Compressor` |
| Overflow retry | `Run` main loop (`loop.go:315`) | `Config.MaxOverflowRetries` |
| Permission gate | `interactionGate` + `dispatch` (`loop.go:602,629`) | `Config.Permission` |
| Usage accounting | `Run` main loop (`loop.go:336`) | hard-coded |

Each new capability (tracing, rate-limiting, guardrails, PII, caching) means
another `Config` field plus another inline edit to `attempt`/`Run`/`dispatch`.
The ordering between concerns (compression before memory injection, permission
consulted twice) is implicit in the code, not stated anywhere. This is exactly
the problem **agent-loop middleware** solves — and it is the shape LangChain's
`langchain` middleware has converged on (node-style hooks for observation,
wrap-style hooks for control).

This is **agent** middleware, not HTTP middleware: it intercepts the
think→tool→think lifecycle (model calls, tool calls), not request/response.

## What changes

- **New capability `agent-middleware`**: a two-kind hook model inside
  `internal/agent`:
  - **Node-style hooks** (`BeforeModel`, `AfterModel`, `AfterRun`) run
    sequentially at fixed points; they observe and may mutate run state.
  - **Wrap-style hooks** (`WrapModelCall`, `WrapToolCall`) receive a `handler`
    callable and control whether it runs zero times (short-circuit), once
    (normal), or many times (retry / fallback).
- **Ordering is the registration order**: `Before*` runs first→last, `Wrap*`
  nests (first registered is outermost), `After*` runs last→first. The implicit
  "compression before memory injection" becomes an explicit slice order.
- **Re-express existing concerns as built-in middleware**: memory injection,
  image materialization, compression, overflow retry, and permission become
  middleware implementing the new interfaces. `Config` shrinks to true config
  (`Model`, `MaxTokens`, `MaxIterations`, …); the `With*` setters for these
  concerns become middleware registration.
- **Transient-view vs durable-record is explicit in the contract**: each hook
  declares whether it rewrites the outgoing working view (droppable, never
  persisted) or observes the durable record (never rewritten).

## Out of scope (this change)

- Migrating the **permission/HITL approval** flow to middleware. Approval is
  run-stateless (the run ends; a later run applies the verdict) and the
  `Permission` callback is consulted twice with different semantics
  (`interactionGate` vs `dispatch`). That needs a dedicated
  `ErrEndRunForApproval` design and is deferred — see `design.md` "Deferred:
  permission as middleware".
- New middleware capabilities (tracing, rate-limit, guardrails). The point of
  the abstraction is that these become *easy*, but none are built here.
- Any change to the SSE/transport layer (`chatapi`) or the session runtime.

## Success criteria

- `agent.Loop` body contains no inline memory/image/compress/overflow logic;
  those live in middleware under `internal/agent/middleware/` (or
  `internal/agentx/`).
- `go test ./...` green, including new middleware unit tests that exercise each
  hook in isolation (no full `Loop` needed).
- A new concern can be added as a middleware without editing `loop.go`.
