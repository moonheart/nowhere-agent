# agent-middleware — tasks

Standing constraints: write `*_test.go` for all Go code and run `go test ./...`
green before commit. No `Co-Authored-By` trailer.

## 1. Hook types + Loop plumbing (internal/agent)

- [x] 1.1 Define `RunState`, `NodeHooks`, `ModelCall`/`ModelHandler`/`ModelResult`/`ModelCallMiddleware`, `ToolCall`/`ToolHandler`/`ToolCallMiddleware`, and the `Middleware` marker. Document transient-view vs durable-record contract on the types. — `internal/agent/middleware.go`
- [x] 1.2 `Loop.Use(...Middleware)` registration preserving order; `Loop` stores the slice. — `internal/agent/loop.go`
- [x] 1.3 Build the wrap chain: `chainModel([]ModelCallMiddleware, realCall)` nesting first-registered outermost; `chainTool` likewise. — `internal/agent/middleware.go`
- [x] 1.4 Unit-test ordering: before m1→m2→m3, wrap nested m1 outermost, after m3→m2→m1, using recording middleware. — `internal/agent/middleware_test.go`

## 2. Migrate model-call concerns to middleware (internal/agent/middleware)

- [x] 2.1 `compressMW` (`WrapModelCall`): move `maybeCompress` + circuit-breaker; state in `RunState`, policy (threshold/window/max-failures) on the value. — `internal/agent/middleware/compress.go`
- [x] 2.2 `overflowMW` (`WrapModelCall`): move the overflow retry loop out of `Run`. — `internal/agent/middleware/overflow.go`
- [x] 2.3 `memoryMW` (`WrapModelCall`): move `memInjector` invocation; keeps copy-on-write so injected messages never reach `Produced`. — `internal/agent/middleware/memory.go`
- [x] 2.4 `imageMW` (`WrapModelCall`): move `MaterializeImages` pre-send. — `internal/agent/middleware/image.go`
- [x] 2.5 Unit tests for each: compression shrinks view over budget + breaker trips; overflow retries then gives up; memory appends only to the copy; image materializes paths. — `internal/agent/middleware/*_test.go`

## 3. Migrate observation concerns to node hooks

- [x] 3.1 `usageMW` (`AfterModel` accumulates into `RunState.Usage`; `AfterRun` emits `KindUsage`). — `internal/agent/middleware/usage.go`
- [x] 3.2 `persistMW` (`AfterModel` emits `KindMessage` for the assembled assistant + tool-result messages). — `internal/agent/middleware/persist.go`
- [x] 3.3 Unit tests: usage totals across turns; `KindMessage` emitted per assembled message with usage pairing. — `internal/agent/middleware/*_test.go`

## 4. Rewire Loop + Config slim-down

- [x] 4.1 `Run`/`attempt`/`consume` delegate to the hook chain; delete inline memory/image/compress/overflow/usage code. `Run` keeps orchestration + tool batching only. — `internal/agent/loop.go`
- [x] 4.2 `Config` keeps true config (`Model`, `System`, `MaxTokens`, `MaxIterations`, `CacheablePrefix`); remove `Compressor`, `Images`, `MemoryInjector`, overflow/compress knobs (now middleware). Keep `Permission` (deferred). — `internal/agent/loop.go`
- [x] 4.3 Replace `WithImages`/`WithMemoryInjector` with `Use(imageMW)/Use(memoryMW)`; keep `WithTools`. — `internal/agent/loop.go`

## 5. Wire the server to the new assembly order

- [x] 5.1 `cmd/server/main.go`: register middleware in order `compress → memory → image` for both `newChatLoop` and `subFactory`. — `cmd/server/main.go`
- [x] 5.2 `cmd/e2e/main.go`: same minimal wiring. — `cmd/e2e/main.go`
- [x] 5.3 Update `chatapi` call sites that used `WithImages`/`WithMemoryInjector` to pass middleware via the loop factory instead. — `internal/chatapi/handler.go`

## 6. Verify + spec

- [x] 6.1 `go test ./...` green; existing loop tests (compress/overflow/memoryinject/gate) still pass against the middleware-backed loop.
- [x] 6.2 Add `openspec/changes/2026-07-28-agent-middleware/specs/agent-loop/spec.md` delta for the middleware requirement.
- [x] 6.3 Note in `design.md` remains accurate: permission stays non-middleware this change.

## Deferred (not this change)

- Permission/HITL approval as a `WrapToolCall` middleware with `ErrEndRunForApproval`.
- Tracing / rate-limit / guardrails middleware (enabled by, but not built in, this change).
