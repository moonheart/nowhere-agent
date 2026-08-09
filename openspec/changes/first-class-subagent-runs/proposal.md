# first-class-subagent-runs

## Why

Subagent runs are fire-and-forget: a spawn produces no durable record of its own (no id, status, timing, usage, or error outside the parent's collapsed tool result), so nothing can be audited, retried, attached to, or inspected per child run. A parent run that resumes after interruption also re-executes `spawn_agent` from scratch — child work is neither checkpointed nor idempotent, so completed subagent work (and its cost) is silently duplicated. Separately, two robustness gaps remain: every child text/thinking delta is one broker frame (flood amplification on chatty children), and fan-out budgets are tree-global with no per-agent-type limits.

## What Changes

- Durable `subagent_runs` records: every spawn writes a row (id, session id, parent run id, spawn tool-call id, agent type, depth, status, outcome code, usage, timestamps) updated through the child lifecycle.
- Structured outcome codes on spawn results (`completed`, `depth_exceeded`, `budget_exhausted`, `timeout`, `cancelled`, `gated`, `child_error`) so the parent model — and clients — can distinguish retryable from terminal failures instead of parsing prose.
- Idempotent spawn replay: a re-issued spawn carrying the same tool-call id within a resumed parent run returns the recorded outcome of the already-completed child instead of re-executing it; a child recorded as interrupted/failed is re-run deliberately by a new spawn call.
- Observability API: authenticated read endpoints to list/get a session's subagent runs (per-run drill-down: status, outcome, usage, timing); the console/run panel reads from it instead of relying solely on live frames.
- Activity frame throttling: streamed child text/thinking deltas are coalesced per child run (time-windowed flush) before hitting the broker, bounding frame rate under chatty fan-out.
- Per-agent-type budgets: in addition to the tree-global max-total/max-concurrent, optional per-type caps (total and concurrent) configured per deployment, enforced at spawn.

## Capabilities

### New Capabilities
- `subagent-run-records`: Durable per-spawn run records (lifecycle, outcome codes, usage) with an authenticated read API for session-scoped inspection.
- `subagent-run-budgets`: Per-agent-type fan-out budgets (total/concurrent) layered on the existing tree-global budget, and coalesced activity streaming.

### Modified Capabilities
- `subagent`: Spawn gains durable lifecycle recording and structured outcome codes; resumed parent runs replay already-completed spawns idempotently instead of re-executing them.

## Impact

- **DB**: new migration (`subagent_runs` table); no changes to existing tables.
- **Backend**: `internal/subagent` (recorder, outcome codes, replay, throttle, per-type budget), `cmd/server/main.go` wiring, read endpoints (session-scoped, behind `RequireAuth`).
- **Frontend**: run panel / subagent card consumes the records API for post-hoc inspection; live `data-subagent` frames unchanged in shape (fewer of them under throttling).
- **No breaking changes**: spawn tool name/schema unchanged; outcome code is additive metadata on the tool result; existing collapse behavior is the fallback when no recorder is wired (tests/dev).
