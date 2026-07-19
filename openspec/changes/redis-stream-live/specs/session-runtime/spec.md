# Spec: session-runtime (delta for redis-stream-live)

## MODIFIED Requirements

### Requirement: Durable run event log
Run **lifecycle** events SHALL be appended to a durable, ordered event log (`run_events`) that records run state transitions (running, done, error, cancelled). The durable log SHALL NOT be used to store per-token content deltas; conversation content is the province of the message store (persist-raw-messages), and live content delivery is the province of the stream broker.

#### Scenario: Lifecycle events persisted
- **WHEN** the loop emits a lifecycle event (running, done, error, cancelled)
- **THEN** it is appended to the run's durable log

#### Scenario: Content deltas not persisted to run_events
- **WHEN** the loop emits a streaming content delta (text, thinking, tool frame)
- **THEN** it is NOT written to `run_events` (it goes to the live stream broker), so a run produces O(lifecycle) rows, not O(tokens)

### Requirement: Event bus fan-out
Run events SHALL be fanned out to attached clients through a replaceable broker abstraction (in-memory for single instance, Redis-backed for multi-instance) without changing session or transport logic. Live content fan-out SHALL NOT wait on a durable write; the durability boundary for content is the assembled message, not the individual token.

#### Scenario: Live fan-out not gated by persistence
- **WHEN** a run emits a streaming content delta
- **THEN** it is published to attached clients without waiting on a database write

#### Scenario: Lifecycle persisted before settle
- **WHEN** a run reaches a terminal state
- **THEN** the terminal lifecycle event is persisted to the durable log before the run is marked settled, so attached clients observe it

#### Scenario: Broker replaceable across instances
- **WHEN** the deployment changes from single-instance to multi-instance
- **THEN** swapping the in-memory broker for the Redis broker (config only) makes a live run's stream visible to clients connected to any instance, with no change to session or transport logic

### Requirement: Reconnect and replay
A client SHALL be able to reconnect and recover the run's in-flight output. While the run's live stream survives, the client re-reads the stream from its last received offset; once the stream has been cleaned up (run settled), the authoritative content comes from the durable message store.

#### Scenario: Resume from the live stream
- **WHEN** a client reconnects to an active run with a last-received stream offset
- **THEN** all stream frames after that offset are re-delivered in order, then live-followed

#### Scenario: Settled run falls back to the message store
- **WHEN** a client reconnects after the run's stream has been cleaned up
- **THEN** the conversation content is rendered from the persisted messages (final, full blocks)

### Requirement: Persisted runs as episodes
Each run iteration SHALL be flushed to durable storage as it happens. A session SHALL have one or more runs, and the session's persisted **messages** (full-block conversation content) SHALL serve as the episodes consumed by the dreaming worker.

#### Scenario: Iteration persisted
- **WHEN** a run iteration completes
- **THEN** its assembled messages are written to durable storage before the next iteration proceeds

#### Scenario: Episodes available to dreaming
- **WHEN** the dreaming worker runs
- **THEN** it reads the session's persisted messages (full-block, in sequence order) as the episodes for ended sessions, with no separate episode store required

## ADDED Requirements

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
