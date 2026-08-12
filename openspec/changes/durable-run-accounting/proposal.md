## Why

Run accounting and recovery are currently memory-only in three places that cost real money or correctness: (1) token usage is attached to assistant messages, so failed/discarded provider responses and crash windows lose spend silently; (2) retry counts (transport retry, overflow retry) reset on every process restart, so a crash-restart loop can retry the same failing request forever; (3) a `length` stop below the intended output cap is treated as a normal completion, so context-truncated responses pass silently instead of being compacted and retried once. The session already persists lifecycle events and assembled messages; this change extends that durability to the per-request accounting and per-step recovery facts.

## What Changes

- **Usage ledger decoupled from messages** (problem 2). A new `usage_records` table records every provider request at settle time, before any classification, retry, or discard decision — including failed attempts and discarded overflow responses. `runs.usage_*` becomes a recomputed aggregate over the ledger; `messages.usage_*` stays as an immutable display snapshot. No schema is removed; existing queries keep working.
- **Step intent records + durable retry counts** (problem 1). A new `run_steps` table records each assistant step and tool invocation before its effect (pre-provisioned result message id, attempt number). Retry counts survive restarts: a crash-restart loop can no longer reset them. Recovery (`RecoverStrandedRuns`) distinguishes "step completed" from "step interrupted" instead of blindly failing every non-terminal run.
- **Recoverable-length classification and once-per-input guard** (problem 3). A `length` stop whose output is below the intended cap is classified recoverable: the response is discarded (never persisted), context is compacted, and the request is retried once per conversational input. The guard is persisted in `run_steps`, so it survives restarts. Genuine output-limit stops keep today's behavior. The existing error-path overflow middleware remains.
- **BREAKING**: none to public HTTP/SSE APIs. Internal storage and recovery behavior change.

## Capabilities

### New Capabilities
- `usage-ledger`: per-request usage records bound to pre-provisioned message ids, written at settle before classification; run aggregates recomputed from the ledger; entry snapshots immutable.
- `run-step-intents`: durable step intent records (assistant + tool) with pre-provisioned result ids and durable attempt counts; crash-site decidable recovery.
- `overflow-recovery`: recoverable-length classification (output vs intended cap), discard-and-compact-and-retry-once per conversational input, persisted guard.

### Modified Capabilities
- `agent-loop`: the reactive context-overflow fallback requirement gains recoverable-length handling and the once-per-input recovery guard.
- `observability`: per-LLM-call usage recording moves from "recorded on the produced message" to "recorded at settle, regardless of outcome".
- `session-runtime`: startup recovery no longer marks every non-terminal run failed without inspecting step state; retry counts and interrupted steps are observable at recovery time.

## Impact

- **Migrations**: two new tables (`usage_records`, `run_steps`); one new migration file.
- **Session package** (`internal/session`): `registry.go` (step intent writes before loop effects, usage records at settle), `pgstore.go`/`pgmessagestore.go` (new tables' store methods, `RecoverStrandedRuns` semantics), `runtime.go` (recovery behavior).
- **Agent loop** (`internal/agent`): `loop.go` (length classification, discard path, once-per-input guard, intent writes), `middleware.go` (`OverflowMW` shares the persisted guard).
- **Usage middleware** (`internal/agent/middleware.go` `UsageMW`): settle-time ledger write instead of end-of-run aggregation.
- **No changes** to chatapi/SSE framing, provider adapters, or the web frontend.
