# Spec: session-runtime

## ADDED Requirements

### Requirement: Run lifecycle state machine
Each run SHALL have an explicit lifecycle: queued, running, waiting_approval, done, failed, cancelled. Run state SHALL be decoupled from any transport connection.

#### Scenario: Run independent of connection
- **WHEN** a client disconnects mid-run
- **THEN** the run continues and its state is preserved

#### Scenario: Waiting for approval
- **WHEN** an action requires approval
- **THEN** the run enters waiting_approval until the user responds

### Requirement: Durable run event log
Run events SHALL be appended to a durable, ordered event log that serves as the source of truth.

#### Scenario: Events persisted
- **WHEN** the loop emits an event
- **THEN** it is appended to the run's durable log before delivery to clients

### Requirement: Reconnect and replay
A client SHALL be able to reconnect and replay events from its last received offset.

#### Scenario: Resume after disconnect
- **WHEN** a client reconnects with a last-event offset
- **THEN** all events after that offset are replayed in order

### Requirement: Stop and cancel
A running run SHALL be cancellable, with cancellation propagating to in-flight tool calls and sandbox execution.

#### Scenario: Cancel propagates
- **WHEN** a user cancels a run
- **THEN** in-flight tools and sandbox exec are cancelled and the run transitions to cancelled

### Requirement: Multi-client attach
Multiple clients SHALL be able to attach to the same session and receive the same event stream.

#### Scenario: Two tabs, one session
- **WHEN** a second client attaches to an active session
- **THEN** it receives the session's event stream (replaying history as needed)

### Requirement: Single active run and multi-writer prevention
A session SHALL run at most one run at a time. State changes SHALL be synchronized to all attached clients, and clients SHALL be blocked from starting a new run while one is active.

#### Scenario: State synced across clients
- **WHEN** one client starts a run
- **THEN** all other attached clients are synced to the running state

#### Scenario: Blocked concurrent submission
- **WHEN** a run is active and another client submits a new run
- **THEN** the new submission is rejected or queued until the active run completes

### Requirement: Persisted runs as episodes
Each run iteration SHALL be flushed to durable storage as it happens. A session SHALL have one or more runs, and these persisted run records SHALL serve as the episodes consumed by the dreaming worker.

#### Scenario: Iteration persisted
- **WHEN** a run iteration completes
- **THEN** it is written to durable storage before the next iteration proceeds

#### Scenario: Episodes available to dreaming
- **WHEN** the dreaming worker runs
- **THEN** it reads the persisted run records (episodes) for ended sessions, with no separate episode store required

### Requirement: Session lifecycle
A session SHALL be considered ended after a configurable period (N minutes) with no active run and no attached client. Session-end SHALL be the single signal triggering sandbox deferred-stop, workspace solidify, and episode eligibility for dreaming.

#### Scenario: Idle session ends
- **WHEN** no run is active and no client is attached for N minutes
- **THEN** the session transitions to ended and triggers downstream cleanup

#### Scenario: Active client keeps session alive
- **WHEN** a client is attached or a run is active
- **THEN** the session is not considered ended regardless of elapsed time
