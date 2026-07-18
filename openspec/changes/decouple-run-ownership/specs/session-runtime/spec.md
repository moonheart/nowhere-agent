# Spec: session-runtime (delta for decouple-run-ownership)

## MODIFIED Requirements

### Requirement: Run lifecycle state machine
Each run SHALL have an explicit lifecycle: queued, running, waiting_approval, done, failed, cancelled. Run state SHALL be decoupled from any transport connection, and run execution SHALL be owned by a connection-independent worker (registry), not by the submitter's HTTP request.

#### Scenario: Run independent of connection
- **WHEN** the submitting client disconnects mid-run
- **THEN** the run continues and its state is preserved

#### Scenario: Run survives submitter disconnect
- **WHEN** the client that started a run closes its connection
- **THEN** the run keeps executing and other attached clients continue receiving its events

#### Scenario: Waiting for approval
- **WHEN** an action requires approval
- **THEN** the run enters waiting_approval until the user responds

### Requirement: Stop and cancel
A running run SHALL be cancellable by any attached client, with cancellation propagating to in-flight tool calls and sandbox execution, independent of which HTTP connections are open.

#### Scenario: Cancel propagates
- **WHEN** a user cancels a run
- **THEN** in-flight tools and sandbox exec are cancelled and the run transitions to cancelled

#### Scenario: Attached client cancels
- **WHEN** a client that attached to (but did not start) a run requests cancellation
- **THEN** the run is cancelled and all attached clients observe the cancelled terminal state

### Requirement: Multi-client attach
Multiple clients SHALL be able to attach to the same session and receive the same event stream. The submitting client and every reconnecting/attaching client SHALL consume the run through a single, shared attach path (event bus + durable-log replay).

#### Scenario: Two tabs, one session
- **WHEN** a second client attaches to an active session
- **THEN** it receives the session's event stream (replaying history as needed)

#### Scenario: Submitter and attacher are symmetric
- **WHEN** a run is streamed to the submitter and to an attacher
- **THEN** both traverse the same attach path and observe the same terminal state

## ADDED Requirements

### Requirement: Event bus fan-out
Run events SHALL be fanned out to attached clients through an `EventBus` abstraction whose durable source of truth is the run event log. The bus SHALL be replaceable (e.g. in-memory for single instance, Redis-backed for multi-instance) without changing session or transport logic.

#### Scenario: Live fan-out through the bus
- **WHEN** a run emits an event
- **THEN** it is persisted to the durable log and published to the bus for all subscribers

#### Scenario: Terminal event precedes settle
- **WHEN** a run reaches a terminal state
- **THEN** the terminal event is persisted and published before the run is marked settled, so attached clients observe it

#### Scenario: Gap filled from the durable log
- **WHEN** an attached client misses live events (slow consumer)
- **THEN** the missed events are recovered from the durable log on replay/terminal-fill
