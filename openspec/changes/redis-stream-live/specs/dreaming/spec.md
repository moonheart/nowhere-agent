# Spec: dreaming (delta for redis-stream-live)

## ADDED Requirements

### Requirement: Episodes sourced from the message store
The episodes the dreaming worker consumes SHALL be read from the session's persisted messages (the full-block conversation record), not from the run event log. The run event log records lifecycle only and SHALL NOT be the episode source.

#### Scenario: Episodes read as full-block messages
- **WHEN** the dreaming worker processes an ended session
- **THEN** it reads the session's persisted messages in sequence order (text, thinking, tool_use, tool_result) as the episode content

#### Scenario: Run event log not required for episodes
- **WHEN** the run event log holds only lifecycle events for a session
- **THEN** the dreaming worker still obtains complete episode content from the message store
