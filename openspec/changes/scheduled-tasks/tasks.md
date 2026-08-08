# scheduled-tasks — tasks

## 1. Schema (migration 000021)

- [x] 1.1 `000021_scheduled_tasks.up.sql`: create `scheduled_task` per design D7 (all columns, the
  exactly-one-prompt-source `CHECK`, the `(enabled, next_run_at)` index, FKs to `users`/`teams`/
  `agentdef`/`sessions`)
- [x] 1.2 Same migration: `ALTER session ADD task_id uuid NULL REFERENCES scheduled_task`,
  `ADD source text NOT NULL DEFAULT 'human'`, `ADD metadata jsonb NOT NULL DEFAULT '{}'`; index on
  `task_id`
- [x] 1.3 `000021_scheduled_tasks.down.sql`: drop the added `session` columns, then `scheduled_task`
- [x] 1.4 Tests: migration applies and reverts cleanly against the dev Postgres

## 2. schedule: domain + store

- [x] 2.1 `internal/schedule/task.go`: `Task` struct mapping the table; `PromptSource` resolution
  (free-text vs agentdef+kickoff); `MultitaskStrategy` and `OnRunCompleted` enums
- [x] 2.2 `internal/schedule/store.go`: `Store` port — `Create/Get/Update/Delete/List(owner)`/
  `ListDue(now)`/`Claim(id, next, now)`/`ListSessions(taskID)`; PG impl + in-mem impl (project
  symmetry rule)
- [x] 2.3 Cron next-fire helper over `robfig/cron` (or equivalent), timezone-aware; unit tests
  across timezones and a DST boundary
- [x] 2.4 `Claim` implements the atomic `UPDATE … WHERE next_run_at <= now() … RETURNING` (design
  D4); PG test: two concurrent claims of one due task → exactly one row returned
- [x] 2.5 Tests: store CRUD, owner scoping, due-filter predicate (enabled / not-past-end_time /
  next_run_at<=now), `ListSessions` returns only that task's sessions

## 3. schedule: trigger loop

- [x] 3.1 `internal/schedule/trigger.go`: scan loop on a configurable interval, driven from `main.go`
  on the root context; graceful shutdown
- [x] 3.2 Per due task: `Claim` → multitask gate → build environment → submit; a claim returning
  zero rows skips silently
- [x] 3.3 Multitask gate (design): query target session's active run; `reject` skips+logs,
  `interrupt` cancels via `registry.CancelRun`, `enqueue` waits
- [x] 3.4 Catch-up is the normal claim advancing to the next *future* slot (design D6) — no
  separate path; test that a task overdue by several slots fires once
- [x] 3.5 Tests: due task is claimed and submitted; raced claim fires once; expired task skipped;
  busy-session `reject` skips without firing

## 4. schedule: unattended run construction

- [x] 4.1 Resolve prompt source → system prompt + model + opening user turn (mirror `agentdef`
  resolution used by `spawn_agent`)
- [x] 4.2 Resolve owner identity + team credentials via `routing.PGKeyStore`, degrading to the
  platform key on failure (match chat)
- [x] 4.3 Resolve session: NULL target → create row with `task_id`/`source='scheduled'`; set target
  → load and reuse; deleted target → log + skip, don't crash the scan
- [x] 4.4 Build the loop and bind exactly `tool_whitelist` (design D3); reuse the chat context
  builder for memory recall + skill L0/L1
- [x] 4.5 Submit via `registry.Submit(RunWork{Loop, History, UserMessage})`; persist opening user
  turn as the run's first message
- [x] 4.6 On terminal: apply `on_run_completed` (delete fresh session when set); merge task
  `metadata` into session metadata (design D7)
- [x] 4.7 Tests: claimed task yields a session with correct `task_id`/`source` and merged metadata;
  non-whitelisted tool never callable; empty whitelist binds no tools

## 5. HTTP management routes

- [x] 5.1 `internal/scheduleapi` (or extend an existing console): CRUD + enable/disable +
  list-sessions, all behind `RequireAuth`
- [x] 5.2 Validate cron expression and timezone on create/update; reject invalid with 422-style
  error
- [x] 5.3 Owner/team authorization on every route; cross-owner access denied
- [x] 5.4 Wire routes in `cmd/server/main.go`; start the trigger goroutine alongside the dreaming
  scheduler
- [x] 5.5 Tests: CRUD happy path, validation failures, cross-owner denial, list-sessions scoping

## 6. Config + docs

- [x] 6.1 `internal/config`: `SCHEDULE_ENABLED` (default true when chat is enabled),
  `SCHEDULE_SCAN_INTERVAL` (default `30s`); document in `.env.example`
- [x] 6.2 Tests: config defaults and overrides
- [x] 6.3 Update `AGENTS.md` "Supporting subsystems" with a one-line scheduled-tasks entry

## 7. Frontend (deferred, optional this change)

- [x] 7.1 Typed client in `web/src/lib` for the task API
- [x] 7.2 Task list + create/edit form (cron + timezone + prompt source + whitelist picker)
- [x] 7.3 Session list surfaces `source='scheduled'` and links a task to its sessions
