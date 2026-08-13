## 1. Migration and store methods

- [x] 1.1 Create `migrations/000041_run_accounting.up.sql` with `run_steps` and `usage_records` tables plus indexes (design D1)
- [x] 1.2 Add `PGStore.AppendUsageRecord` and `PGStore.AppendRunStep` methods (`internal/session/pgstore.go`), including `nextval`-based pre-provisioned `messages.id` acquisition for intent rows
- [x] 1.3 Add `PGStore.SumUsage(runID)` and `PGStore.NewestRunSteps(runID, limit)` queries; add run-step insertion helpers on `PGMessageStore` for explicit-id message inserts

## 2. Usage ledger (problem 2 — settle-time writes)

- [x] 2.1 In `registryEmitter` (`internal/session/registry.go`), write a `usage_records` row (cause `assistant`, bound to the message id) before `persistMessage` on the `KindMessage` path
- [x] 2.2 Replace `persistRunUsage`/`SetRunUsage` run-end accumulation with `runs.usage_* = SumUsage(runID)` recomputation; keep `messages.usage_*` writes unchanged
- [x] 2.3 Add `cause=tool` usage record beside tool-result message persistence (tool-reported usage path)
- [x] 2.4 Tests: `internal/session` unit tests asserting (a) ledger row written before message row, (b) run aggregate equals ledger SUM, (c) adjustments never mutate `messages.usage_*`

## 3. Step intents (problem 1 — intent records and durable counts)

- [x] 3.1 Add a `BeforeModel` middleware installed by `RunRegistry` that inserts the assistant step intent (`run_steps`, kind `assistant`, attempt = persisted count + 1, provisioned message id) and carries the id on run state
- [x] 3.2 Ensure the `KindMessage` persist path inserts the `messages` row with the pre-provisioned id (explicit-id insert)
- [x] 3.3 Insert `run_steps` rows (kind `tool`, with `tool_call_id`) before tool dispatch in the loop; persist tool-result messages with the provisioned id
- [x] 3.4 Retry-count continuity: step start reads persisted attempt count; `OverflowMW`/transport retry budgets account for it across restarts
- [x] 3.5 Tests: intent-before-effect ordering (no effect starts before its row), attempt counts survive restart, provisioned-id binding invariant

## 4. Recovery inspection (problem 1 — decidable crash sites)

- [x] 4.1 Change `RecoverStrandedRuns` (`internal/session/runtime.go`) to per-run inspection: newest intent without result → log `run_interrupted` with attempt; with result → `run_step_completed`; no intents → current behavior
- [x] 4.2 Keep mark-failed semantics; add slog records for interrupted vs completed cases
- [x] 4.3 Tests: crashed-before-intent, intent-without-result, result-present, and no-intent runs each settle as specified

## 5. Overflow recovery (problem 3 — length classification and guard)

- [x] 5.1 Add `IsRecoverableLength(res, desiredMaxOutput)` next to the overflow classification in `internal/agent`; wire `desiredMaxOutput` from caller `maxTokens` else model `maxTokens`, pre-clamp
- [x] 5.2 Add `LengthRecoveryMW` (runs after `OverflowMW`): recoverable → ledger write (no message), discard response, compact via `contextmgmt`, retry once
- [x] 5.3 Persist the guard: `run_steps` kind `overflow_compact` rows; enforce once-per-conversational-input (no newer `overflow_compact` row than newest consumed user/steer/follow-up message); second recovery → give-up error entry + fail run
- [x] 5.4 Keep genuine cap-reached `length` stops completing normally; keep `OverflowMW` error path unchanged except shared guard
- [x] 5.5 User-facing truncation wording neutral ("truncated before completion"); tests for classification, guard persistence across restart, and second-recovery failure

## 6. Verification

- [x] 6.1 `go build ./...`, `go vet ./...`, `go test ./...` green
- [x] 6.2 Verify no chatapi/SSE framing, provider adapter, or web frontend changes are needed
