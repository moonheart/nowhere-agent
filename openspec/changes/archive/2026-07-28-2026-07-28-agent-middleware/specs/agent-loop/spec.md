# agent-loop — spec delta (agent-middleware)

## ADDED Requirements

### Requirement: Agent-loop middleware hooks
The loop SHALL expose a middleware mechanism that intercepts the think→tool→think
lifecycle, with two kinds of hooks. Node-style hooks (`BeforeModel`, `AfterModel`,
`AfterRun`) SHALL run sequentially at fixed points to observe and update per-run
state. Wrap-style hooks (`WrapModelCall`, `WrapToolCall`) SHALL receive a handler
callable and control whether the wrapped call runs zero times (short-circuit),
once (normal), or multiple times (retry/fallback).

#### Scenario: Wrap hook controls execution
- **WHEN** a `WrapModelCall` middleware declines to call its handler
- **THEN** the provider call is short-circuited and the middleware's own result is used

#### Scenario: Wrap hook retries
- **WHEN** a `WrapModelCall` middleware calls its handler more than once
- **THEN** the provider call is re-executed, enabling retry/fallback behaviour

### Requirement: Middleware ordering is registration order
The loop SHALL run middleware deterministically by registration order: `BeforeModel`
first→last, `WrapModelCall`/`WrapToolCall` nested with the first registered outermost,
and `AfterModel`/`AfterRun` last→first.

#### Scenario: Nested wrap order
- **WHEN** three model-call middleware are registered in order m1, m2, m3
- **THEN** the call composes as m1(m2(m3(realCall))) so m1 is outermost

#### Scenario: Reverse after order
- **WHEN** three after-model middleware are registered in order m1, m2, m3
- **THEN** they run m3, m2, m1

### Requirement: Cross-cutting concerns are middleware
Compression, memory injection, image materialization, and the context-overflow
fallback SHALL be implemented as middleware rather than inline loop logic. The loop's
`Config` SHALL carry only true configuration (model, token limits, iteration guard,
permission); adding a cross-cutting capability SHALL NOT require editing the loop body.

#### Scenario: Compression as wrap middleware
- **WHEN** the working view crosses the context budget
- **THEN** a `WrapModelCall` compression middleware rewrites the outgoing view before the provider call

#### Scenario: Overflow fallback as wrap middleware
- **WHEN** the provider rejects a request as too large
- **THEN** a `WrapModelCall` overflow middleware drops the oldest round and calls its handler again, up to a bounded retry count

### Requirement: Transient view vs durable record
Middleware that rewrites the working view (compression, memory injection, image
materialization) SHALL operate on a per-attempt copy that is never persisted. The
durable conversation record SHALL NOT be altered by any middleware.

#### Scenario: Injected memory never persisted
- **WHEN** a memory-injection middleware appends recalled memories to the outgoing view
- **THEN** those messages reach the provider request but do not enter the produced
  messages, so the durable history stays append-only and the prompt-caching prefix stays
  byte-stable
