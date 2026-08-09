## ADDED Requirements

### Requirement: Durable subagent run records
Every `spawn_agent` execution SHALL write a `subagent_runs` row at start and
update it through the lifecycle: status (`running` → `completed` | `failed` |
`cancelled` | `gated`), outcome code, collapsed result, accumulated usage, and
start/finish timestamps. The record SHALL carry the session id, the parent run
id, the spawn tool-call id, the agent type, and the depth, so nested trees are
reconstructable. Recording SHALL be best-effort: a recorder write failure SHALL
NOT fail the spawn itself. When no recorder is wired (tests/dev), spawns SHALL
behave exactly as before.

#### Scenario: Lifecycle recorded
- **WHEN** a subagent run starts, finishes, fails, or is gated
- **THEN** its row reflects each transition with timestamps and, at the end, the outcome code and usage

#### Scenario: Nested tree attributable
- **WHEN** a child itself spawns a grandchild
- **THEN** the grandchild's record names the child's tool-call id chain and depth, so the whole tree under a session is queryable

#### Scenario: Recorder outage does not fail spawns
- **WHEN** the record write fails while the child loop itself succeeds
- **THEN** the spawn still returns its result to the parent and the loss is logged

### Requirement: Subagent run inspection API
The platform SHALL expose authenticated, session-scoped read endpoints:
`GET /api/sessions/{sessionID}/subagent-runs` (list, newest first) and
`GET /api/sessions/{sessionID}/subagent-runs/{id}` (detail incl. collapsed
result and usage). Access SHALL follow session ownership: an account reads only
its own sessions' records; platform administrators read any. Each record SHALL
expose status, outcome code, agent type, depth, parent linkage, usage, and
timestamps.

#### Scenario: Owner lists records
- **WHEN** an account lists subagent runs for its own session
- **THEN** all records for that session are returned with status, outcome, type, depth, usage, and timing

#### Scenario: Non-owner rejected
- **WHEN** an account requests records for another account's session
- **THEN** the response does not distinguish "session does not exist" from "not yours"

#### Scenario: Platform admin reads any session
- **WHEN** a platform administrator requests records for any session
- **THEN** the request is authorized
