## Why

A suspended tool batch's identity is not recorded at suspend time. On resume, `FoldBatch` re-derives the batch by scanning session history for "the last assistant message with tool_use" (`suspendedToolUses`). If the user sends a new message while an approval/ask_user card is pending — which nothing prevents today — a subsequent run appends its own tool_use-bearing assistant messages, the scan finds the WRONG batch, and the fold mis-executes: the new run's already-executed (or still-pending) tool calls get dispatched again, while the original gated call's verdict is silently dropped, leaving its tool_use permanently dangling in the durable record. This is a real, reachable race with duplicate side effects and a permission-gate bypass variant.

## What Changes

- **Capture the suspended batch at suspend time**: when the interaction gate parks a batch, persist a batch snapshot — `(run_id, message_seq, tool_call_ids[])` — in the same transaction as the interaction rows.
- **Fold by snapshot, not by history heuristic**: `FoldBatch` loads the snapshot for the folding run, locates the suspended assistant message by its recorded seq, strictly validates that the message's tool_use IDs equal the snapshot's ID set, and folds exactly those calls. Any mismatch fails loudly. The `suspendedToolUses` history scan is deleted.
- **Atomic batch fold**: recording the final decision of a batch and persisting the folded tool_result message happen in one transaction, eliminating the "decided but never folded" intermediate state.
- **Submission gate while interactions are pending**: submitting a new chat message to a session with pending interactions is rejected with 409 (durable, PG-backed check — correct across instances), so a suspended batch can never be buried under newer turns.

## Capabilities

### New Capabilities

- `suspend-batch-snapshot`: Durable capture of a suspended tool batch's identity at gate time, snapshot-driven batch fold with strict validation, atomic decision+fold persistence, and the pending-interaction submission gate.

### Modified Capabilities

- `agent-loop`: The "General interrupt primitive" requirement gains the obligation to durably record the suspended batch identity (message seq + tool_call ID set) when the run suspends.
- `session-runtime`: The "Single active run and multi-writer prevention" requirement extends to rejecting new submissions while the session has pending interactions.

## Impact

- **DB**: new migration — a `suspended_batches` table (or columns on `approvals`); FK to runs; written in the same transaction as interaction creation.
- **Backend**: `internal/session` (registry: gate persistence, FoldBatch rewrite, atomic fold; runtime/Submit: pending-interaction check), `internal/chatapi` (409 mapping in the submit path), `internal/agent` (loop surfaces batch identity to the emitter).
- **API**: `POST /api/chat` returns 409 with a pending-interaction error when the session has undecided interactions.
- **Frontend**: `web` should surface the 409 and guide the user to resolve the pending card (history already echoes pending interactions on reload).
- **Reference**: design borrows LangGraph's interrupt semantics (interrupt bound into state, structural resume, atomic super-step writes) without adopting a checkpoint system — the messages table remains the single source of truth.
