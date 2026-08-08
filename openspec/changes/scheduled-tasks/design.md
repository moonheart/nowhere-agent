# scheduled-tasks — design

## Overview

A scheduled task is a durable, owner-scoped definition of a recurring agent run. A trigger loop
scans for due tasks, claims each atomically against the database, reconstructs the run environment
that `chatapi` normally assembles from a live HTTP request, and submits the run through the
existing `RunRegistry`. The result persists as a normal session, tagged so the UI can identify it.

The guiding principle: **the scheduler is an unattended `chatapi`, not a new execution engine.**
Everything downstream of "here is a loop and a user message" — streaming, persistence, permission
gating, context compression — is reused unchanged by routing through `registry.Submit`.

## Design decisions

### D1 — Task definition stores a prompt *source*, not a baked loop

A task names its prompt in one of two ways, chosen at creation:

- **free-text `prompt`** — a fixed user-turn string, or
- **`agent_def_id`** — a reference to an `agentdef`, from which the system prompt and model are
  taken (mirroring how `spawn_agent` resolves a child definition).

This is more flexible than langgraph's cron, which only references an assistant. The trigger
resolves the source into a system prompt + model + initial user turn at fire time, so edits to an
`agentdef` take effect on the task's next fire without touching the task.

`prompt` and `agent_def_id` are mutually exclusive (a `CHECK` constraint enforces exactly one is
set). A task always has *some* initial user turn: with `agent_def_id`, a fixed `prompt` supplies
the kickoff message that starts the run.

### D2 — Session targeting is a nullable pointer, not an either/or

`scheduled_task.target_session_id` is nullable:

- **NULL** → each fire creates a fresh session (clean history, no cross-contamination). This is the
  default.
- **set** → each fire appends to that one session, giving continuity ("how does today compare to
  yesterday") at the cost of a growing context that the existing compressor must manage.

This mirrors langgraph's `thread_id` (nullable) rather than forcing a single session model. Paired
with it, `on_run_completed ∈ {keep, delete}` controls whether a freshly created session survives
after the run (default `keep`; `delete` suits fire-and-forget maintenance runs whose output is only
read via the task's session list).

### D3 — Permission is front-loaded to creation via a tool whitelist

The hard constraint of unattended execution: a risk-gated tool that would normally `Ask` and suspend
for approval has no one to approve it. Queuing it for a human "tomorrow" defeats the schedule;
auto-approving is unsafe.

So the task carries an explicit **`tool_whitelist`** (a text array of tool names). At fire time the
loop's registry is bound with exactly that set. Tools outside it are not registered at all — the
model cannot call what it cannot see. This turns runtime approval into creation-time curation: the
owner decides, once, what the unattended agent may touch. An empty whitelist means a read-only /
reasoning-only run (no tools bound).

This composes with, not replaces, the existing risk gate: whitelisted tools that are *also* risky
still pass through `permission`, but since the whitelist is the owner's explicit grant, the task's
sandbox/permission context is constructed to treat whitelisted entries as pre-authorized within the
task's scope.

### D4 — Multi-instance safety via atomic claim, not an advisory lock

The platform is single-Postgres with optional multi-instance fan-out. A naive "scan then fire" on
every instance would run each due task N times. Two options were weighed:

- **`pg_try_advisory_lock`** — a global or per-task lock held across the fire. Works, but serializes
  unrelated tasks behind one lock and holds a connection for the run's duration.
- **Atomic `UPDATE … RETURNING`** *(chosen)* — claim and advance `next_run_at` in one statement:

```sql
UPDATE scheduled_task
SET    next_run_at = <next fire per cron+timezone>,
       last_run_at = now()
WHERE  id            = $1
  AND  enabled
  AND  (end_time IS NULL OR end_time > now())
  AND  next_run_at  <= now()      -- still due: another instance may have claimed it already
RETURNING *;
```

The first instance to commit advances `next_run_at` into the future; a concurrent instance's
`WHERE next_run_at <= now()` then matches zero rows, and it skips. Per-task granularity, no lock
held during execution, no extra infrastructure. This is the same mechanism langgraph's Postgres
backend uses (its Go `Crons.Next()` advances `next_run_date` atomically), as opposed to its in-memory
backend which can afford a racy separate update because it is single-process by definition.

### D5 — A dedicated trigger component, separate from `internal/scheduler`

`internal/scheduler` is interval-based and process-local (its `lastRun` map resets every boot; the
dreaming job tolerates this only because idempotency rests on the `dreamed_seq` watermark). Cron
semantics are different: expression parsing, per-task timezone, durable `next_run_at`, single-fire
claiming. Rather than stretch the interval scheduler, the trigger is its own component
(`internal/schedule` or similar) owning the scan-claim-fire loop, so each keeps a coherent model.
It still runs as a goroutine from `main.go` and respects the root context for shutdown.

### D6 — Catch-up fires the most-recent missed occurrence only

If the server is down past a task's `next_run_at`, on restart the due-scan naturally finds it
overdue. Firing it once and advancing `next_run_at` to the next *future* slot means a daily 9 a.m.
task down until 10 fires once (the 9 a.m. run, slightly late) — not once per missed day if down for
a week. The claim statement in D4 already produces exactly this: it advances to the next future
occurrence regardless of how many were skipped. No separate catch-up path is needed; the normal
claim *is* the catch-up, bounded to one fire.

### D7 — `metadata` jsonb on both new and touched tables, relational columns only for relations and hot filters

Following the project rule "SQL-aware (JOIN / constraint / hot WHERE) → dedicated column; carried
for display/aggregation → jsonb":

- `scheduled_task.metadata` jsonb — open-ended task config (future notification webhook, retry
  policy, concurrency notes) that is not yet fixed.
- `session.metadata` jsonb — open-ended per-run annotations (trigger detail, cost, tags, external
  ids), which the platform is already accumulating demand for (cf. the `runs.team_id`/`runs.model`
  exact-usage gap).
- but `session.task_id` and `session.source` stay **dedicated columns**: `task_id` is a foreign key
  (JOIN + cascade) and `source` is a high-frequency list filter. Neither belongs in jsonb.

At fire time the task's `metadata` is merged into the run/session metadata (task-level keys
overridden by run-specific ones), so provenance travels with the output.

## Data model (migration `000021`)

```
scheduled_task
  id                 uuid PK
  user_id            uuid  NOT NULL  FK -> users        (owner)
  team_id            uuid  NULL      FK -> teams        (optional team scope)
  agent_def_id       text  NULL      FK -> agentdef     (D1: reference source)
  prompt             text  NULL                          (D1: free-text source / kickoff)
  tool_whitelist     text[] NOT NULL DEFAULT '{}'        (D3)
  cron               text  NOT NULL                      (5-field)
  timezone           text  NOT NULL DEFAULT 'UTC'        (IANA)
  target_session_id  uuid  NULL      FK -> sessions      (D2)
  on_run_completed   text  NOT NULL DEFAULT 'keep'       (keep|delete)
  multitask_strategy text  NOT NULL DEFAULT 'reject'     (reject|interrupt|enqueue)
  end_time           timestamptz NULL                    (D6: auto-expire)
  enabled            bool  NOT NULL DEFAULT true
  next_run_at        timestamptz NOT NULL
  last_run_at        timestamptz NULL
  metadata           jsonb NOT NULL DEFAULT '{}'         (D7)
  created_at         timestamptz NOT NULL DEFAULT now()
  updated_at         timestamptz NOT NULL DEFAULT now()
  CHECK (exactly one of agent_def_id / prompt-for-agent or standalone prompt is set)
  INDEX (enabled, next_run_at)   -- the due-scan hot path

session  (ALTER)
  + task_id   uuid NULL FK -> scheduled_task   INDEX     (D7: relation)
  + source    text NOT NULL DEFAULT 'human'              (human|scheduled|subagent)
  + metadata  jsonb NOT NULL DEFAULT '{}'                (D7)
```

## Execution flow

```
 trigger loop (per scan interval)
   │  SELECT id FROM scheduled_task
   │  WHERE enabled AND next_run_at <= now()
   │    AND (end_time IS NULL OR end_time > now())
   ▼  for each candidate
 atomic claim (D4 UPDATE … RETURNING; 0 rows = another instance took it → skip)
   │
   ▼  claimed
 multitask gate (D-from-langgraph): if target_session has an active run
   │   reject → skip this fire · interrupt → cancel active · enqueue → wait
   ▼
 rebuild run environment (the "unattended chatapi"):
   resolve prompt source (D1) → system prompt + model + kickoff user turn
   resolve owner identity (task.user_id, team credentials via routing.PGKeyStore)
   resolve session (D2): new row w/ task_id+source='scheduled' OR reuse target
   build loop, bind exactly tool_whitelist (D3)
   inject memory recall + skill L0/L1 (same context builder as chat)
   ▼
 registry.Submit(RunWork{Loop, History, UserMessage})   ← identical to a human chat
   ▼
 on terminal: apply on_run_completed; merge metadata into session
```

## Failure & edge handling

- **Claim succeeds but the fire panics/errors before submit** — `next_run_at` is already advanced,
  so the task is not retried until its next slot. The miss is recorded in `last_run_at` + a slog
  error; acceptable because scheduled runs are best-effort, not transactional exactly-once.
- **`multitask_strategy = reject` with a busy target session** — the fire is skipped (logged), the
  next slot proceeds normally. Prevents unattended pile-up of duplicate runs.
- **Deleted `agent_def` or `target_session`** — the FK `ON DELETE` behaviour surfaces at fire time;
  the trigger logs and skips (does not crash the scan loop) and the task remains for the owner to fix.
- **Team credential resolution failure** — degrades to the platform key, exactly as a human chat
  does (`routing.PGKeyStore` semantics preserved).

## Testing strategy

- **Unit** (`internal/schedule`): cron next-fire across timezones and DST; due-filter predicate;
  the atomic claim returning zero rows when already advanced; catch-up firing once after downtime;
  multitask `reject` skipping a busy session.
- **PG tests** against the real dev Postgres, unique random names, scoped cleanup only (project
  rule): claim under two simulated instances firing the same due task — exactly one wins.
- **Integration**: a task with a stub agent fires through `registry.Submit` and produces a session
  row with `task_id`/`source='scheduled'` and the merged metadata; whitelist exclusion means a
  non-whitelisted tool is never callable in that run.
- **HTTP**: CRUD authz (owner-only, team scope), validation of cron expression and timezone,
  list-sessions-for-task.
