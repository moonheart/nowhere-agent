# session-runtime Specification

## Purpose
Owns the session/run lifecycle for the agent platform: the run state machine, the
durable run event log (source of truth and episodes for dreaming), reconnect/replay,
multi-client attach, and run stop/cancel. Runs execute on connection-independent
workers behind an EventBus port, so a run's lifecycle is decoupled from any HTTP
connection and every client (submitter or attacher) is a symmetric consumer of the
same event stream.
## Requirements
### Requirement: Run lifecycle state machine
Each run SHALL have an explicit lifecycle: queued, running, waiting_approval, done, failed, cancelled. Run state SHALL be decoupled from any transport connection, and run execution SHALL be owned by a connection-independent worker (registry), not by the submitter's HTTP request.

#### Scenario: Run independent of connection
- **WHEN** a client disconnects mid-run
- **THEN** the run continues and its state is preserved

#### Scenario: Run survives submitter disconnect
- **WHEN** the client that started a run closes its connection
- **THEN** the run keeps executing and other attached clients continue receiving its events

#### Scenario: Waiting for approval
- **WHEN** an action requires approval
- **THEN** the run enters waiting_approval until the user responds

### Requirement: Durable run event log
Run events SHALL be appended to a durable, ordered event log that serves as the source of truth.

#### Scenario: Events persisted
- **WHEN** the loop emits an event
- **THEN** it is appended to the run's durable log before delivery to clients

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

### Requirement: Reconnect and replay
A client SHALL be able to reconnect and replay events from its last received offset.

#### Scenario: Resume after disconnect
- **WHEN** a client reconnects with a last-event offset
- **THEN** all events after that offset are replayed in order

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

### Requirement: Faithful message persistence
The system SHALL persist every conversation message in its original, full-block form (text, thinking including signature, tool_use, tool_result) to a durable message store, so that the conversation can be rebuilt without information loss. The message store SHALL be the authoritative record of conversation content, distinct from the run event log (which records lifecycle and streaming frames).

#### Scenario: Assistant message with tool call persisted
- **WHEN** a run produces an assistant message containing text, thinking (with signature), and a tool_use block
- **THEN** that message is persisted as one row with all blocks intact

#### Scenario: Tool result persisted
- **WHEN** a run dispatches tool calls and receives results
- **THEN** the tool results are persisted as a user-role message containing tool_result blocks

#### Scenario: Full fidelity round-trip
- **WHEN** a stored message is read back
- **THEN** its blocks (text, thinking incl. signature, tool_use input, tool_result content and error flag) match what was produced

#### Scenario: Image block persisted as pointer
- **WHEN** a message contains an image block
- **THEN** it is persisted with the media type and a workspace-relative path pointer, not the base64 payload, so the stored row stays small

#### Scenario: Oversized tool result truncated
- **WHEN** a tool_result block exceeds the size cap
- **THEN** it is persisted truncated with a truncation marker appended

### Requirement: Per-session message ordering
Messages SHALL be stored with a monotonic per-session sequence so conversation order is stable across runs and survives runs that settle mid-stream.

#### Scenario: Ordering across runs
- **WHEN** multiple runs append messages to the same session
- **THEN** reading the session's messages returns them in the order they were produced across all runs

#### Scenario: Sequence continues after a settled run
- **WHEN** a run is cancelled or completes and a later run appends to the same session
- **THEN** the new messages continue the session sequence without collision or reset

### Requirement: Authoritative cross-run history
Cross-run conversation history SHALL be rebuilt from the persisted message store rather than from client-supplied message history. For a session the caller owns, the server record is authoritative and client-sent history SHALL NOT modify it.

#### Scenario: History rebuilt from the store
- **WHEN** a client sends a new message on an existing session
- **THEN** the loop receives history rebuilt from the persisted messages, with full blocks

#### Scenario: Client history cannot rewrite the past
- **WHEN** a client sends a message history that differs from the persisted record for its session
- **THEN** the persisted record is used and the client-sent history is ignored

