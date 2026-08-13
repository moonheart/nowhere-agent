# usage-ledger Specification

## Purpose
A durable per-request usage ledger decoupled from message rows: every provider request settles into a `usage_records` row regardless of outcome, run aggregates are recomputed from the ledger, and message-level usage columns remain immutable display snapshots.

## ADDED Requirements
### Requirement: Usage recorded at settle, before classification
Every provider request SHALL write a `usage_records` row when the request settles, before any classification, retry decision, or discard. A row SHALL be written for succeeded requests, failed attempts, and discarded responses alike; only a request that reports no usage (a pending deferred fetch) may skip the write.

#### Scenario: Successful request settles
- **WHEN** a provider request returns a response with usage
- **THEN** a `usage_records` row is written with the input/output/cache-read/cache-write counts before the response is classified

#### Scenario: Failed attempt billed
- **WHEN** a provider request fails after spending tokens (retryable or terminal)
- **THEN** its usage is still recorded, so spend does not vanish with the failed response

#### Scenario: Discarded overflow response billed
- **WHEN** a recoverable overflow response is discarded without becoming a message
- **THEN** its usage is still recorded

### Requirement: Ledger bound to pre-provisioned message ids
A harness-written `usage_records` row SHALL bind `result_message_id` to the pre-provisioned id of the message the request was expected to produce. The bound message may never exist (failed attempt, discarded response); the binding still holds. Adjustments and tool-reported usage SHALL be recorded with `cause` distinguishing `assistant` / `tool` / `adjustment`.

#### Scenario: Bound id without message
- **WHEN** a failed attempt writes a usage record bound to a message id
- **THEN** the id need not exist in `messages`; the record is still valid and summable

#### Scenario: Tool-reported usage
- **WHEN** a tool result reports nested LLM work
- **THEN** a `tool`-cause usage record is written beside the tool result

### Requirement: Run aggregates recomputed from the ledger
`runs.usage_*` SHALL be the sum of the run's `usage_records` rows, recomputed at run end rather than accumulated incrementally. `messages.usage_*` SHALL remain as written and never be altered by ledger operations; it is an immutable display snapshot, and effective cost is the ledger sum.

#### Scenario: Run total equals ledger sum
- **WHEN** a run ends and its aggregate is computed
- **THEN** each `runs.usage_*` column equals `SUM` of the matching `usage_records` columns for that run

#### Scenario: Message snapshot unchanged by adjustments
- **WHEN** an adjustment usage record is added for a run
- **THEN** existing `messages.usage_*` values are unchanged and the effective cost includes the adjustment

#### Scenario: Crash before run-end aggregation
- **WHEN** a process crashes before a run's aggregate is recomputed
- **THEN** the ledger rows are intact and the aggregate can be recomputed on recovery
