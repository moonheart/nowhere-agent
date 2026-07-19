# Spec: session-runtime (delta for persist-raw-messages)

## ADDED Requirements

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
