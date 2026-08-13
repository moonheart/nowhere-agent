# scheduled-tasks Specification

## Purpose
Scheduled tasks let a tenant define a recurring agent run — schedule, prompt or agent reference,
tool scope — and have the platform fire it unattended through the same execution path a human chat
would use, persisting the result as a real, browsable session.

## Requirements
### Requirement: Task definition
The system SHALL let an authenticated user define a scheduled task that fires an agent run on a
recurring schedule. A task SHALL carry: a 5-field cron expression, an IANA timezone (defaulting to
UTC), a prompt source, a tool whitelist, an enabled flag, and owner scope (`user_id`, optional
`team_id`).

The prompt source SHALL be exactly one of: a free-text prompt, or a reference to an agent
definition (`agentdef`) whose system prompt and model are used, together with a fixed kickoff
prompt as the run's opening user turn. The system SHALL reject a task that names neither or both
sources.

The cron expression and timezone SHALL be validated at creation and update; an invalid value SHALL
be rejected rather than stored.

#### Scenario: Create a free-text task
- **WHEN** an authenticated user creates a task with a cron expression, timezone, a free-text
  prompt, and a tool whitelist
- **THEN** the task is persisted with an initial `next_run_at` computed from the cron and timezone

#### Scenario: Create an agent-referencing task
- **WHEN** a user creates a task referencing an existing `agentdef` and supplying a kickoff prompt
- **THEN** the task is persisted and will resolve its system prompt and model from that definition
  at fire time

#### Scenario: Reject an invalid schedule
- **WHEN** a user submits a task whose cron expression or timezone is invalid
- **THEN** the task is rejected with a validation error and nothing is stored

#### Scenario: Reject an ambiguous prompt source
- **WHEN** a task names both an `agentdef` and no kickoff, or neither source
- **THEN** the task is rejected

### Requirement: Due-task scanning and atomic claim
The system SHALL run a trigger loop that finds tasks due to fire: `enabled`, `next_run_at` at or
before the current time, and not past `end_time`. For each due task the trigger SHALL claim it by
advancing `next_run_at` and recording `last_run_at` in a single atomic statement conditioned on
the task still being due, so that under multiple instances exactly one instance claims a given
occurrence.

A claim that affects zero rows (another instance already claimed it) SHALL cause the trigger to
skip that occurrence without error.

#### Scenario: A due task is claimed
- **WHEN** the trigger finds an enabled task whose `next_run_at` has passed and which is not past
  `end_time`
- **THEN** it atomically advances `next_run_at` to the next future occurrence and proceeds to fire

#### Scenario: Two instances race one occurrence
- **WHEN** two instances attempt to claim the same due task occurrence concurrently
- **THEN** exactly one claim succeeds and the other skips without firing

#### Scenario: An expired task does not fire
- **WHEN** a task's `end_time` has passed
- **THEN** the trigger does not fire it

### Requirement: Unattended run construction
For a claimed task, the system SHALL reconstruct the run environment that a chat request would
build, and submit it through the shared run registry so that streaming, persistence, permission,
and compression behave identically to a human-initiated run. Reconstruction SHALL resolve: the
prompt source into a system prompt, model, and opening user turn; the owner's identity and
credentials; the target session; and a tool registry bound to exactly the task's whitelist.

The run's opening user turn SHALL be persisted as the run's first message, as it is for a human
chat.

#### Scenario: A claimed task produces a normal run
- **WHEN** a task is claimed
- **THEN** a run is submitted through the run registry and its messages persist as for a human chat

#### Scenario: Team credentials resolve as in chat
- **WHEN** a team-scoped task fires and its team credentials cannot resolve
- **THEN** the run degrades to the platform key, matching human-chat behaviour

### Requirement: Session targeting
A task SHALL either create a fresh session per fire or append to a fixed session, governed by a
nullable `target_session_id`: when NULL, each fire creates a new session; when set, each fire
appends to that session.

Every session a task produces SHALL record `task_id` and `source = 'scheduled'` so scheduled
output is identifiable and filterable. A task SHALL also carry `on_run_completed` (`keep` or
`delete`); for a freshly created session, `delete` SHALL remove the session after the run reaches a
terminal state, while `keep` (the default) retains it.

#### Scenario: Fresh session per fire
- **WHEN** a task with NULL `target_session_id` fires
- **THEN** a new session is created carrying that task's `task_id` and `source = 'scheduled'`

#### Scenario: Continuity on a fixed session
- **WHEN** a task with a set `target_session_id` fires repeatedly
- **THEN** each run appends to that same session

#### Scenario: Fire-and-forget cleanup
- **WHEN** a task with `on_run_completed = 'delete'` finishes a run on a freshly created session
- **THEN** that session is removed after the run terminates

### Requirement: Unattended permission via whitelist
Because no human is present to approve a risk-gated tool call during an unattended run, the run
SHALL be bound with exactly the task's tool whitelist and no others. A tool not on the whitelist
SHALL NOT be registered into the run's loop, so it cannot be called. An empty whitelist SHALL yield
a run with no tools.

#### Scenario: Non-whitelisted tool is unavailable
- **WHEN** a task's whitelist omits a tool the model attempts to call
- **THEN** that tool is not present in the run's registry and cannot be invoked

#### Scenario: Empty whitelist runs tool-free
- **WHEN** a task has an empty whitelist
- **THEN** its runs execute with no tools bound

### Requirement: Concurrency strategy
A task SHALL carry a `multitask_strategy` (`reject`, `interrupt`, or `enqueue`, default `reject`)
governing what happens when a fire is due but the target session already has an active run.
`reject` SHALL skip the fire; `interrupt` SHALL cancel the active run and start the new one;
`enqueue` SHALL skip the fire and let the next scheduled occurrence start once the active run
has drained (the single-active-run registry enforces the ordering; the fire is not queued).

#### Scenario: Reject skips a busy session
- **WHEN** a fire is due and its target session has an active run under `reject`
- **THEN** the fire is skipped and recorded, and the next occurrence proceeds normally

### Requirement: Missed-occurrence catch-up
If the server is down through a task's scheduled occurrence, on recovery the trigger SHALL fire
the most-recent missed occurrence once and advance `next_run_at` to the next future slot. It SHALL
NOT fire once per occurrence skipped during the downtime.

#### Scenario: One catch-up after downtime
- **WHEN** a daily task's server was down for several days and restarts
- **THEN** the task fires a single most-recent occurrence, not one per missed day

### Requirement: Task management API
The system SHALL expose authenticated HTTP routes to create, read, update, delete, enable/disable,
and list tasks, scoped to the caller (and to a team when the task is team-scoped). It SHALL also
expose a route listing the sessions a task has produced. All routes SHALL sit behind the same
authentication as the other management consoles; a caller SHALL NOT see or modify another owner's
tasks.

#### Scenario: Owner manages own tasks
- **WHEN** an authenticated user lists, updates, or deletes their own tasks
- **THEN** the operations succeed and reflect only their scope

#### Scenario: Cross-owner access is denied
- **WHEN** a user attempts to read or modify a task owned by another user or a team they are not in
- **THEN** the request is denied

#### Scenario: List a task's sessions
- **WHEN** an owner requests the sessions a task produced
- **THEN** the sessions recorded with that `task_id` are returned
