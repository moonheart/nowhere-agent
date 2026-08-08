# Design: Suspended Batch Snapshot

## Context

The agent loop's unified interaction gate (internal/agent/loop.go:391) suspends the whole tool batch the moment any call needs the client. The run ends statelessly; resume is a fresh run whose history is rebuilt from the `messages` table. The fold step (`RunRegistry.FoldBatch`, internal/session/registry.go:317) must reconstruct exactly which calls formed the suspended batch — but today it re-derives them via `suspendedToolUses` (registry.go:423), a heuristic scan for "the last assistant message with tool_use" across the WHOLE session history.

Meanwhile `StartRun` only blocks on an active run, not on pending interactions, so a user can submit a new message while approval cards hang. The new run appends its own tool_use-bearing assistant messages, poisoning the heuristic. On the later resume, FoldBatch matches the folding run's interactions against the WRONG assistant message: unmatched IDs fall into the "no interaction → dispatch now" branch, so the new run's calls execute (again, or without their own verdict), and the original gated call's verdict is dropped — its tool_use dangles forever (EnsurePairing only patches the transient view, loop.go:290).

Reference: LangGraph's HITL middleware never re-derives the batch. `interrupt()` persists the pending task INSIDE the checkpoint (channel writes committed atomically per super-step), interrupt IDs are structural (`xxh3(checkpoint_ns)`), and resume replays against the same state. We deliberately borrow these semantics without adopting a checkpoint system: our `messages` log (per-session monotonic seq, full blocks, runID) already is the versioned state; what's missing is (a) binding the suspension into that state and (b) atomic fold writes.

## Goals / Non-Goals

Goals:
- Fold can never target a batch other than the one the folding run suspended.
- No silent fallbacks in the fold path: every mismatch is a loud error.
- The "decided but never folded" window is recoverable, not fatal.
- A suspended batch can never be buried under newer turns.
- Legacy in-flight interactions (pending at deploy time) keep working.

Non-Goals:
- A general checkpoint/snapshot tree (version chains, fork, time-travel).
- Mid-loop crash recovery of loop-internal state (compression counters etc.).
- Exactly-once tool execution (at-least-once, same as LangGraph; tools own idempotency).
- Queueing new submissions behind pending interactions (enqueue strategy) — reject only.

## Decisions

### D1: Batch snapshot table, written with the interaction rows

New table `suspended_batches`:

```sql
CREATE TABLE suspended_batches (
    run_id         TEXT PRIMARY KEY REFERENCES runs(id),
    session_id     TEXT NOT NULL,
    message_seq    BIGINT,              -- backfilled once known; nullable
    tool_call_ids  JSONB NOT NULL,      -- full batch, tool_use order
    folded_seq     BIGINT,              -- set when the fold commits (D3)
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

- `tool_call_ids` is the FULL batch (gated + ungated siblings), in assistant-message block order — the fold's answer key.
- Write path: the loop already emits KindMessage (assistant batch) before the interrupt frames (loop.go:322 → :402). Each `Interaction` payload gains a `Batch []toolruntime.Call` field (the full ordered batch). The store's approval-insert becomes one transaction: `INSERT suspended_batches ... ON CONFLICT (run_id) DO NOTHING` + insert the interaction row. The existing ordering guarantee (row committed before the frame is published, registry.go:592) is preserved per frame; the batch insert is idempotent across the batch's frames.
- `message_seq` may be filled by the emitter when the assistant message persists (persistMessage already gets the seq back from AppendMessage), but the fold does not depend on it (D2 locates by run_id).
- Alternative considered — columns on `approvals`: rejected; the batch is a per-run fact (includes ungated siblings with no approval row), not a per-interaction fact.
- Alternative considered — a dedicated KindSuspend frame: rejected; a new event kind ripples through stream conformance for zero transactional benefit over piggybacking the interrupt payload.

### D2: Fold by snapshot, scoped by run, strictly validated

`FoldBatch` becomes:

1. Load the `suspended_batches` row for `runID`. Missing → error (no heuristic fallback; legacy rows are backfilled at migration, D7).
2. Rebuild history from `messages`; find the LAST tool_use-bearing assistant message **with RunID == runID** (StoredMessage already carries RunID — scoping makes cross-run pollution structurally impossible).
3. Validate: the message's tool_use ID set MUST equal `tool_call_ids`. Mismatch → error, nothing executes, nothing persists.
4. Fold each call: interaction verdicts by ToolCallID; calls without an interaction dispatch via the provided registry (unchanged semantics — but now provably only calls from THIS suspended batch).
5. `suspendedToolUses` is deleted.

Validation turns every residual inconsistency (double-write bugs, manual DB edits) into an error instead of a mis-execution — the LangGraph "decisions count mismatch raises" posture.

### D3: Recoverable, idempotent fold commit

True atomicity of "record final decision + persist tool_result message" is impossible: the fold EXECUTES approved tools (slow, side-effecting) between the two writes, and a DB transaction must not span tool execution. Instead, make the fold a recoverable step:

- After executing/dispatching all calls, commit in ONE transaction: append the tool_result message + set `suspended_batches.folded_seq`.
- `FoldBatch` first checks `folded_seq`: already folded → skip execution entirely, rebuild and return history (idempotent resume retry).
- Crash between final decision and fold commit → the next resume attempt re-folds. Approved tools may re-execute: at-least-once, identical to LangGraph's node-replay semantics; documented for tool authors.
- The resume endpoint must not strand this recovery: `RecordDecision` on an already-decided row returns ErrNoPendingApproval, and a naive 409 there would deadlock (the retry never reaches the idempotent fold). serveChatResume therefore consults `BatchFoldState`: decided-but-not-folded falls through to the fold; decided-AND-folded keeps the 409. Concurrent fold retries converge via a `SELECT ... FOR UPDATE` claim on the batch row — the loser gets ErrBatchAlreadyFolded and rebuilds history (idempotent success).
- The fold is commit-class work, not request-scoped: `FoldBatch` detaches from the caller's cancellation (`context.WithoutCancel`) — a client disconnect after POSTing the final verdict must not abort tool execution halfway, because there is no automatic retry (a decided row renders no pending card). This mirrors the run model, where a run's ctx derives from `context.Background`, not the submitter's connection. Per-tool timeouts still bound each call; ctx values are preserved (serveChatResume stamps the session id so the execution gate resolves the session's permission mode correctly at fold).

### D4: Malformed-args calls never execute — including at fold

A call whose arguments failed to parse is refused by the loop's dispatch screen ("invalid tool arguments") and gets no interaction row. The parse failure is persisted on the tool_use block (`args_error`) — without it, a nil `ToolInput` is ambiguous with a legitimate no-args call. The fold screens such calls exactly like the loop does: it never dispatches them and folds an `is_error: "invalid tool arguments: ..."` tool_result, while gated siblings still execute per their verdicts.

### D5: Fold re-applies the execution gate to un-gated siblings

"Not gated" ≠ "within policy": the interaction gate suspends only on deny-with-approval-marker (plus ask_user / client tools), so a HARD-denied call (env policy `Deny`, no approval marker) is an un-gated sibling. The loop's dispatch screen would have refused it; if the fold executed it via the bare registry, one policy's outcome would depend on whether the batch happened to contain an approval-gated neighbour. LangGraph avoids this structurally: its HITL middleware never executes tools — execution happens in the downstream tools node through the normal chain on resume. Our fold mirrors that by threading the loop's execution gate (`agent.Loop.Gate()` → `session.ToolGate`) into `FoldBatch`: a denied sibling folds an `is_error: "permission denied: ..."` result and never dispatches. Gated calls skip the re-check — the human verdict supersedes the ask-tier, and the env tier is static config unchanged since suspend. The tool-wrap middleware chain is NOT threaded (no `WrapToolCall` implementation exists; `CallAll` replicates the loop's innermost call exactly); if one is ever added it must be routed into the fold.

### D6: Pending-interaction submission gate (durable)

- `serveChat` (chat submit path) checks `PendingInteractionsForSession(sessionID)` against the STORE (PG — an in-memory check would be wrong in multi-instance deployments where instance B doesn't know instance A's pending cards) before `Submit`. Any pending → 409 with a typed error body (`{"error":"pending_interaction"}`), so the frontend can point at the unresolved card instead of showing a generic conflict.
- The schedule trigger's submit path (internal/schedule/trigger.go) applies the same check and skips with a log line — a scheduled run must not bury a human's pending approval either.
- Deliberately reject, not enqueue: while an interaction is pending the model context is incomplete; queueing a user turn would either wait on a human or resume against a stale conversation. Enqueue can be added later as a product decision without changing the gate's mechanics.

### D7: Migration with backfill

1. Create `suspended_batches`.
2. Backfill: for every run with pending interactions, find that run's last tool_use-bearing assistant message and insert `(run_id, session_id, seq, ids)`. Runs where no such message exists are unrecoverable under any semantics — their pending interactions are marked rejected ("superseded by migration") so they can't hang a session's submission gate forever.

## Risks / Trade-offs

- [Snapshot/message double-write inconsistency] → Same-transaction insert (D1) + strict ID-set validation at fold (D2): residual inconsistency surfaces as a loud fold error, never a mis-execution.
- [Re-execution of approved tools on fold retry after a crash] → At-least-once is the domain's accepted semantics (LangGraph identical); the `folded_seq` idempotency check shrinks the window to an actual crash mid-commit.
- [409 on submit surprises users mid-flow] → Frontend already echoes pending interactions on history load (chatapi/history.go); the typed error lets the UI deep-link the pending card. Server 409 is the backstop, not the primary UX.
- [Batch payload repeated on every interrupt frame] → A few hundred bytes per frame; ON CONFLICT DO NOTHING makes repeats no-ops. Cheaper than a new event kind's conformance surface.

## Migration Plan

1. Ship migration (table + backfill) — safe before code: nothing reads the table yet.
2. Ship backend (D1–D6). Old pending interactions work via backfilled snapshots.
3. Ship frontend handling of the typed 409.
4. Rollback: code rollback only needs to restore the legacy `suspendedToolUses` path (kept behind nothing — it's deleted; rollback = revert commit). The table is additive; no down-migration needed for data safety.

## Open Questions

- Should `POST /api/chat` with an explicit "cancel pending and proceed" flag exist (auto-reject all pending interactions, then submit)? LangGraph's `multitask_strategy=rollback` analog. Deferred — the 409 + manual decision flow covers the use case today.
- Does the frontend need a session-level `pendingInteractions` field in the history payload to disable the composer, or is the existing queue echo sufficient?
