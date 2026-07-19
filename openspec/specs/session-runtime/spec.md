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
Run **lifecycle** events SHALL be appended to a durable, ordered event log (`run_events`) that records run state transitions (running, done, error, cancelled) and the user message. The durable log SHALL NOT store per-token content deltas; conversation content is the province of the message store (persist-raw-messages), and live content delivery is the province of the stream broker.

#### Scenario: Events persisted
- **WHEN** the loop emits a lifecycle event (running, done, error, cancelled) or a user message
- **THEN** it is appended to the run's durable log

#### Scenario: Content deltas not persisted to run_events
- **WHEN** the loop emits a streaming content delta (text, thinking, tool frame)
- **THEN** it is NOT written to `run_events` (it goes to the live stream broker), so a run produces O(lifecycle) rows, not O(tokens)

### Requirement: Event bus fan-out
Run events SHALL be fanned out to attached clients through a replaceable broker abstraction (in-memory for single instance, Redis-backed for multi-instance) without changing session or transport logic. Live **content** fan-out SHALL NOT wait on a durable write; the durability boundary for content is the assembled message, not the individual token. Lifecycle events are persisted to the run event log before fan-out.

#### Scenario: Live fan-out through the bus
- **WHEN** a run emits a streaming content delta
- **THEN** it is published to attached clients via the live broker without waiting on a database write

#### Scenario: Terminal event precedes settle
- **WHEN** a run reaches a terminal state
- **THEN** the terminal lifecycle event is persisted to the durable log and published before the run is marked settled, so attached clients observe it

#### Scenario: Gap filled from the durable log
- **WHEN** an attached client misses live frames (slow consumer or reconnect)
- **THEN** the missed content is recovered from the broker's retained stream (Read after the client's last offset); lifecycle events remain recoverable from the durable run event log

### Requirement: Reconnect and replay
A client SHALL be able to reconnect and recover the run's in-flight output. While the run's live stream survives, the client re-reads the stream from its last received offset; once the stream has been cleaned up (run settled), the authoritative content comes from the durable message store.

#### Scenario: Resume after disconnect
- **WHEN** a client reconnects to an active run with a last-received stream offset
- **THEN** all stream frames after that offset are re-delivered in order, then live-followed

#### Scenario: Settled run falls back to the message store
- **WHEN** a client reconnects after the run's stream has been cleaned up
- **THEN** the conversation content is rendered from the persisted messages (final, full blocks)

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
Each run iteration SHALL be flushed to durable storage as it happens. A session SHALL have one or more runs, and the session's persisted **messages** (full-block conversation content) SHALL serve as the episodes consumed by the dreaming worker.

#### Scenario: Iteration persisted
- **WHEN** a run iteration completes
- **THEN** its assembled messages are written to durable storage before the next iteration proceeds

#### Scenario: Episodes available to dreaming
- **WHEN** the dreaming worker runs
- **THEN** it reads the session's persisted messages (full-block, in sequence order) as the episodes for ended sessions, with no separate episode store required

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

### Requirement: Live stream broker port
Live run output SHALL be delivered through a `StreamBroker` port with offset-based publish/read and run-end cleanup. The port SHALL have an in-memory implementation (single instance) and a Redis Streams implementation (multi-instance) with identical consumer-visible semantics.

#### Scenario: Publish assigns offsets
- **WHEN** a content frame is published to the broker for a session
- **THEN** it is assigned a monotonic offset within that session's live stream

#### Scenario: Read after offset
- **WHEN** a consumer reads the stream from a given offset
- **THEN** it receives every frame published after that offset, in order

#### Scenario: Run-end cleanup
- **WHEN** a run reaches a terminal state
- **THEN** the broker schedules the session's live stream for cleanup (TTL on Redis, buffer reset in memory), so the transient stream is discarded once its live purpose ends

### Requirement: Redis-backed live stream
When configured for multi-instance, the live stream SHALL be backed by Redis Streams: publish appends via `XADD`, consumers read via `XREAD`, and a short TTL is applied when the run settles so the stream self-destructs. The Redis stream SHALL be the only live channel; durable content remains in Postgres.

#### Scenario: Cross-instance visibility
- **WHEN** a worker on one instance publishes a content frame
- **THEN** a client subscribed on another instance receives it via the shared Redis stream

#### Scenario: Stream expires after settle
- **WHEN** a run settles and its TTL elapses
- **THEN** the session's Redis stream is removed, and reconnecting clients fall back to the message store for content

