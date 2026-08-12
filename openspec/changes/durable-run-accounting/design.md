## Context

Runs currently have three memory-only accounting/recovery properties that this change makes durable:

1. **Usage attaches to messages** (`migrations/000013_usage.up.sql`): `messages.usage_*` is set only when the assistant message row is written (via `registryEmitter` `KindMessage` → `persistMessage`, `internal/session/registry.go:766-773, 883-914`); `runs.usage_*` is aggregated once at run end by `UsageMW.AfterRun` (`internal/agent/middleware.go:531-559`). Failed retry attempts, discarded overflow responses, and crash windows lose spend with no trace.
2. **Retries are process-local**: `provider.DoWithRetry` (`internal/provider/retry.go:69-94`, 3 attempts) and `OverflowMW` (`internal/agent/middleware.go:384-440`, 3 drops) keep their counters in memory. `RecoverStrandedRuns` (`internal/session/runtime.go:419-426` → `pgstore.go:416-425`) marks every non-terminal run failed at startup, so a crash-restart loop resets retry budgets indefinitely.
3. **`length` stops are not classified**: a stop below the intended output cap is persisted and reported as success. The session already persists lifecycle events (`run_events`, `000002_session_runtime.up.sql:36-47`) and assembled messages (`messages`, `000006_messages.up.sql`); the intended outcome of this change is that per-request cost and per-step recovery facts get the same durability.

The design is a Go-shaped adaptation of the intent-record discipline validated in the pi `harness-v2` design (step attempt records with pre-provisioned result ids; usage records written at settle before classification; recoverable-length classification against the intended cap with a once-per-input guard). The full lane/operation machinery of that design is out of scope: now here runs are single-active per session and recovery is mark-failed, not resume.

## Goals / Non-Goals

**Goals:**
- Every provider request's spend is durable in a ledger row at settle time, before any classification/discard/retry decision, including failed and discarded requests.
- Retry counts (assistant step, overflow recovery) survive process restarts; a crash-restart loop cannot exceed the configured bounds.
- `length` stops below the intended cap are discarded, compacted, and retried exactly once per conversational input, with the guard persisted.
- Recovery inspects step intents before settling non-terminal runs: interrupted steps are distinguishable from completed steps.

**Non-Goals:**
- Full run resume/continuation after crash (runs still end failed at startup; we only record the *why* with better fidelity and honor durable retry counts).
- Provider stream resumption (partial streams stay ephemeral, as today).
- Multi-instance or multi-writer runs (unchanged single-writer assumption).
- Replacing `messages.usage_*` or `runs.usage_*` columns — they remain as display snapshot and recomputed aggregate respectively.

## Decisions

### D1. Two new tables in one migration
`000041_run_accounting.up.sql` adds:

```sql
CREATE TABLE IF NOT EXISTS run_steps (
    id                BIGSERIAL PRIMARY KEY,
    run_id            UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq               INT NOT NULL,           -- per-run monotonic
    step_kind         TEXT NOT NULL,          -- assistant | tool
    attempt           INT NOT NULL,           -- durable count, 1-based within step kind
    result_message_id BIGINT,                 -- pre-provisioned messages.id
    tool_call_id      TEXT,                   -- step_kind=tool: invocation identity
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_run_steps_run ON run_steps(run_id, seq);

CREATE TABLE IF NOT EXISTS usage_records (
    id                BIGSERIAL PRIMARY KEY,
    run_id            UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    cause             TEXT NOT NULL,          -- assistant | tool | adjustment
    result_message_id BIGINT,                 -- bound id; message may not exist
    attempt           INT,
    input             INT NOT NULL DEFAULT 0,
    output            INT NOT NULL DEFAULT 0,
    cache_read        INT NOT NULL DEFAULT 0,
    cache_write       INT NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_usage_records_run ON usage_records(run_id);
```

Rationale: mirrors the pi `StepAttemptRecord`/`UsageRecord` split (`packages/agent/src/harness/session/types.ts` in the pi repo). `result_message_id` is provisioned *before* the effect (insert a `messages` row only at persist time; the binding is a plain bigint, not a FK, because the message may legitimately never exist). Alternatives considered: reusing `run_events` rows with a new kind — rejected because `run_events` is lifecycle-only and `kind` semantics differ (intents are pre-effect, not post-fact); storing counts on `runs` columns — rejected because per-step granularity (which step, which attempt) is what recovery and the overflow guard need.

### D2. Usage write point: settle, in the emitter
`registryEmitter` gains a ledger write on the `KindMessage` path, but the record is written *before* `persistMessage`: the loop's `MessageWithUsage` already carries the response's usage (`internal/agent/loop.go:433-444`). Concretely: in `registry.go`, the `KindMessage` handler first appends a `usage_records` row (cause `assistant`, bound to the message id, `attempt` from the step intent), then persists the message. The run-end aggregation in `UsageMW.AfterRun`/`persistRunUsage` (`registry.go:859-875`) is replaced by `SetRunUsage(SUM(usage_records WHERE run_id=$1))`.

Rationale: the write must happen after the response settles but before any classification/discard — the emitter is the single choke point every assistant response already crosses. For discarded responses (decision D4) there is no message; the ledger write happens inline in the discard path instead.

### D3. Step intents: two write points
- **Assistant**: in `runWorker.execute` (`registry.go:218-289`), before each `work.Loop.Run` iteration's provider call — the loop exposes per-iteration request state via middleware; the cleanest hook is a small `BeforeModel` middleware installed by the registry that inserts the intent row (`step_kind=assistant`, `attempt` = previous count + 1, fresh provisioned message id carried on the run state) and records the id for the emitter's `KindMessage` to reuse.
- **Tool**: before tool dispatch, in `recordToolResults`' sibling (the dispatch path in `loop.go`, around tool invocation): insert `step_kind=tool` with `tool_call_id` and a provisioned result message id.

Provisioning a `messages.id` before the row exists requires inserting the `messages` row with a known id — currently `messages.id` is `BIGSERIAL` (`000006_messages.up.sql:9`). The pre-provisioned id is generated by `nextval` in the intent insert (same transaction is not required; a `BIGINT` column holds it), and the persist path inserts the row with that explicit id.

Rationale: intent-before-effect is the pi durability rule ("before an effect: write an intent record that names what will happen and the ids it will produce"). The `BeforeModel` middleware approach avoids touching `loop.go` control flow for the common path.

### D4. Overflow recovery: length classification + persisted guard
- **Classification**: a new function in `internal/agent` (next to `provider.IsContextOverflow` usage in `middleware.go`): `IsRecoverableLength(res, desiredMaxOutput) bool` — true iff `res.StopReason == "length"` and (`desiredMaxOutput <= 0` or `res.Usage.Output < desiredMaxOutput`). `desiredMaxOutput` is the caller-supplied `maxTokens` if set, else the model's `maxTokens`, taken before clamping (the loop's `Config` already carries model/token limits, `agent-loop` spec "Config carries true configuration").
- **Path**: recoverable → write the usage ledger row (no message), append nothing to the view, compact via existing `contextmgmt`, retry once. This lives as a new middleware segment (`LengthRecoveryMW`) running after `OverflowMW`, sharing its drop/compact machinery (`contextmgmt.DropOldestRoundPreservingSummary`, `middleware.go:402-405`).
- **Guard**: `run_steps` rows with `step_kind='overflow_compact'` record each recovery; the guard "one recovery per conversational input" is "no `overflow_compact` row newer than the newest consumed user/steer/follow-up message". This mirrors pi's `overflowRecoveryUsed` derived from the reduction rather than a live flag, so it survives restarts for free.

Rationale: classifying `length` by output-vs-intended-cap is precise and needs no context-percentage heuristics (pi §6). A persisted guard from intent rows is free once D3 exists.

### D5. Recovery: inspect before failing
`RecoverStrandedRuns` (`runtime.go:419-426`) changes from a single `UPDATE runs SET status='failed' WHERE status IN (...)` to a per-run inspection: for each non-terminal run, read newest `run_steps` row(s); if the newest intent's `result_message_id` has no `messages` row → log `run_interrupted` with `attempt`; if it has one → log `run_step_completed`; if no intents → current behavior. The run still ends failed (resume is a non-goal); only the fidelity of the record and the durability of the retry count change.

Rationale: minimal behavioral change (still mark-failed) with maximum information gain; keeps the single-writer/startup semantics intact.

### D6. Retry-count continuity
`provider.DoWithRetry` and `OverflowMW` keep their in-memory loops, but their *budget* input comes from durable state: `runWorker.execute` reads the persisted attempt count for the step from `run_steps` at step start (D3's BeforeModel middleware), and the transport middleware's `MaxAttempts` is passed as `max(configured, persisted+1)`-aware: the persisted count caps total attempts across restarts. This is a deliberate, small deviation from pi (which counts every attempt durably); the transport layer keeps its fast in-process loop, and the durable count is what bounds it.

## Risks / Trade-offs

- [Per-request ledger write adds one INSERT per provider call] → Acceptable: one indexed insert per settled request, same order as the existing message insert; can batch with the intent row in a single statement where both exist.
- [`result_message_id` binding without FK lets drift go unnoticed] → Recovery inspection (D5) reads intents vs messages explicitly; tests assert the binding invariant (every message row that has a matching intent carries the provisioned id).
- [Changing `UsageMW.AfterRun` aggregation could double-count during a transitional deploy] → Migration adds tables only; aggregation switch is a code change in the same release; `runs.usage_*` is recomputed at run end from the ledger, and startup recovery recomputes it for stranded runs as a fallback.
- [Length-classification could mis-fire on providers with imprecise usage] → Classification requires output < intended cap; a provider reporting 0 output on a genuine cap stop is rare, and the once-per-input guard bounds the damage to one wasted request. The error path (give-up entry) is explicit and user-facing wording stays neutral.
- [Intent insert on the hot path adds latency before each provider call] → Single indexed insert, async-safe, same transaction budget as the existing pre-loop writes; if it ever matters, the intent row can be batched with the previous iteration's message write.

## Migration Plan

1. Land `000041_run_accounting.up.sql` (tables only, additive).
2. Land ledger write + aggregation switch (D2) — behavior-neutral for queries; `runs.usage_*` now recomputed.
3. Land step intents (D3) + recovery inspection (D5) + retry-count continuity (D6).
4. Land length classification + guard (D4).
5. Rollback: code revert per step; tables are additive and harmless to leave; no data migration required (old rows have no intents/records, which D5 treats as today's behavior).

## Open Questions

- Whether `attempt` for the assistant step should count transport-layer retries (each `DoWithRetry` attempt) or loop-level attempts only — the design counts loop-level steps durably and lets the transport layer burn its own in-process attempts within one step; revisit if a crash-restart loop repeatedly lands on the same step with fresh transport budgets.
- Whether recovery should, as an optional flag, *restart* interrupted runs instead of only recording them — explicitly deferred (resume is a non-goal), but D5's inspection output is the prerequisite for that future flag.
