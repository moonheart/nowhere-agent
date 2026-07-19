# Spec: agent-loop (delta for redis-stream-live)

## ADDED Requirements

### Requirement: Content deltas emitted to a live channel
The loop's per-token content deltas (text, thinking, tool frames) SHALL be emitted to a live delivery channel rather than to a durable per-token log, so streaming throughput is not gated by a synchronous durable write. Assembled messages SHALL still be exposed for durable persistence (see "Assembled messages available for persistence").

#### Scenario: Delta emitted without durable-write latency
- **WHEN** the loop produces a streaming content delta
- **THEN** it is emitted to the live channel without waiting on a database write

#### Scenario: Assembled message still persisted
- **WHEN** the loop finishes one assistant turn
- **THEN** the assembled message is still exposed for durable persistence even though its individual deltas were not separately persisted
