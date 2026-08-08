# scheduled-tasks — proposal

## Why

nowhere-agent runs an agent only when a user is present to drive it. Every run begins as an HTTP
chat request: `chatapi` resolves the caller's identity, builds a session, binds tools, and hands a
`RunWork` to the `RunRegistry`. There is no way to say "run this agent every morning at 9" — the
platform is entirely pull-driven.

Meanwhile the machinery for unattended runs already exists. The `RunRegistry` is explicitly built
so a run "survives the client that started it disconnecting" (`registry.go:48-52`); its context
derives from `context.Background()`, not from any request. And `internal/scheduler` already ticks
jobs on an interval with catch-up — but its jobs are compiled-in Go structs (`main.go:330`
registers only `dreaming`), so adding one means editing code and redeploying.

What is missing is the **user-facing** half: letting a tenant define a recurring agent run —
schedule, prompt or agent reference, tool scope — and have the platform fire it through the same
execution path a human chat would use, persisting the result as a real, browsable session.

## What Changes

- **A new `scheduled_task` table** (migration `000021`) holds the task definition: cron
  expression + IANA timezone, a prompt source (free-text prompt **or** a reference to an
  `agentdef`), a tool whitelist, an optional fixed target session, lifecycle fields
  (`enabled`, `next_run_at`, `last_run_at`, `end_time`), a `multitask_strategy`, and a `metadata`
  jsonb column for open-ended config. Owner-scoped (`user_id` / `team_id`) like every other
  tenant resource.
- **`session` gains `task_id` (FK, nullable, indexed), `source` (enum), and `metadata` jsonb** so
  scheduled output is identifiable and filterable in the UI without polluting the relational core.
- **A new trigger component** scans for due tasks, claims each with an **atomic
  `UPDATE … WHERE next_run_at <= now()`** (multi-instance safe, no advisory lock), then rebuilds
  the full run environment that `chatapi` normally builds from a request — identity, session, loop,
  whitelisted tools, context injection — and submits a `RunWork` through the existing
  `RunRegistry`. No new execution engine; the scheduler is an *unattended chatapi*.
- **Permission is front-loaded to creation time.** Because no human is present to approve a risky
  tool call at 4 a.m., a task carries an explicit tool whitelist; the run's loop is bound with
  exactly that set and nothing more. Approval-gated tools outside the whitelist are unavailable,
  not queued.
- **Missed runs catch up once.** On restart, a task whose `next_run_at` passed while the server
  was down fires its most-recent missed occurrence — not every occurrence it skipped.
- **HTTP management routes** under the same `RequireAuth` as the other consoles: CRUD for tasks,
  plus list-the-sessions-a-task-produced.

## Non-goals

- **No per-run dynamic prompting.** The prompt (or agent reference) is fixed at creation; a task
  does not take fresh input each fire. That is a workflow/orchestration feature, not this one.
- **No result notification fan-out** (email/webhook on completion). The session record *is* the
  deliverable; notification hooks can later live in `scheduled_task.metadata` without schema change.
- **No second-generation scheduler rework.** `internal/scheduler` keeps driving `dreaming` and
  other ops jobs; the cron trigger is a separate component with different (expression, not
  interval) semantics. Unifying them is out of scope.
- **No sub-minute granularity.** Standard 5-field cron; minimum practical resolution is one minute.
