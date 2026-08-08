# Tasks: Suspended Batch Snapshot

## 1. Migration and store

- [x] 1.1 Create migration `000019_suspended_batches.up.sql` / `.down.sql`: `suspended_batches(run_id PK REFERENCES runs, session_id, message_seq BIGINT NULL, tool_call_ids JSONB NOT NULL, folded_seq BIGINT NULL, created_at)`; backfill one row per run with pending interactions from that run's last tool_use-bearing assistant message; reject pending interactions whose run has no such message
- [x] 1.2 Add store methods: `CreateInteractionBatch(ctx, batch SuspendedBatch, in Interaction)` (batch insert `ON CONFLICT (run_id) DO NOTHING` + interaction insert in one transaction), `SuspendedBatchForRun(ctx, runID)`, `MarkBatchFolded(ctx, runID, foldedSeq)`; PG + in-memory implementations
- [x] 1.3 Store method `PendingInteractionsForSession(ctx, sessionID)` (or reuse the existing pending-queue query used by the history echo) exposed for the submission gate
- [x] 1.4 PG store tests for the new methods (real dev Postgres, unique random names, delete only own rows by ID)

## 2. Suspend path: capture the batch

- [x] 2.1 Add `Batch []toolruntime.Call` to `agent.Interaction`; populate it in the loop's gate path (loop.go:391) with the full ordered batch before emitting interrupt frames
- [x] 2.2 `registryEmitter.persistInteraction`: call `CreateInteractionBatch` so the snapshot row commits atomically with the first interaction row (idempotent across the batch's frames)
- [x] 2.3 Optionally backfill `message_seq` on the snapshot when `persistMessage` persists the run's tool_use-bearing assistant message — RESOLVED: not needed, the fold locates the message by run_id and validates the ID set
- [x] 2.4 Tests: suspending a mixed batch (gated + ungated siblings) persists one snapshot with all IDs in order; multi-frame batches yield exactly one row

## 3. Fold path: snapshot-driven, validated, atomic

- [x] 3.1 Rewrite `FoldBatch`: load snapshot by runID (missing → error); locate last tool_use-bearing assistant message with `RunID == runID`; validate tool_use ID set equals snapshot (mismatch → error, no execution, no persistence)
- [x] 3.2 Delete `suspendedToolUses` and its call sites/tests
- [x] 3.3 Idempotent fold: `folded_seq` set → skip execution, rebuild and return history; otherwise execute/dispatch, then append tool_result message + `MarkBatchFolded` in one transaction
- [x] 3.4 Regression tests for the original race: new run appends tool_use messages while interaction pending → resume folds the OLD batch correctly; new run's own calls are never re-dispatched; mismatched snapshot errors; fold retry after simulated crash does not re-execute

## 4. Submission gate

- [x] 4.1 `chatapi.serveChat`: durable pending-interaction check before `Submit`; 409 `{"error":"pending_interaction"}` when any exist
- [x] 4.2 Schedule trigger submit path: same check, skip with log line
- [x] 4.3 Tests: submit with pending interaction → 409 and no run; after decision+fold → submit succeeds; mem-store and PG-store variants

## 5. Frontend and verification

- [x] 5.1 `web`: handle the typed 409 in the chat submit path — surface the pending card and keep the user's draft
- [x] 5.2 `go build ./... && go vet ./... && go test ./...` green; `cd web && pnpm lint && pnpm build` green
