# dreaming Specification

## Purpose
TBD - created by archiving change init-nowhere-agent. Update Purpose after archive.
## Requirements
### Requirement: Scheduled offline worker
The system SHALL run an offline dreaming worker on a configurable schedule. Dreaming SHALL be the only writer to long-term memory.

#### Scenario: Configurable cadence
- **WHEN** an operator sets a dreaming frequency
- **THEN** the worker runs at that cadence without code changes

### Requirement: Extraction
The worker SHALL extract durable facts and preferences from recent conversation episodes into semantic long-term memory.

#### Scenario: Episode to facts
- **WHEN** new episodes exist since the last run
- **THEN** the worker derives facts/preferences and stores them via the memory write side

### Requirement: Compression
The worker SHALL compress old episodes into summaries and discard raw episode content per retention policy.

#### Scenario: Summarize old episodes
- **WHEN** episodes age beyond a threshold
- **THEN** they are replaced by summaries and the raw content is discarded

### Requirement: Reorganization
The worker SHALL detect conflicts among memories and update or deprecate stale ones.

#### Scenario: Conflict resolution
- **WHEN** a new fact contradicts an existing memory
- **THEN** the worker updates or deprecates the stale memory rather than keeping both

### Requirement: Reflection
The worker SHALL identify cross-session patterns and generate higher-level insights.

#### Scenario: Cross-session insight
- **WHEN** multiple episodes reveal a recurring pattern
- **THEN** the worker stores an insight capturing that pattern

### Requirement: Budget control
Dreaming's LLM usage SHALL be bounded by a configurable budget.

#### Scenario: Budget cap
- **WHEN** a run would exceed the configured LLM budget
- **THEN** the worker defers or truncates work to stay within budget

### Requirement: Episodes sourced from the message store
The episodes the dreaming worker consumes SHALL be read from the session's persisted messages (the full-block conversation record), not from the run event log. The run event log records lifecycle only and SHALL NOT be the episode source.

#### Scenario: Episodes read as full-block messages
- **WHEN** the dreaming worker processes an ended session
- **THEN** it reads the session's persisted messages in sequence order (text, thinking, tool_use, tool_result) as the episode content

#### Scenario: Run event log not required for episodes
- **WHEN** the run event log holds only lifecycle events for a session
- **THEN** the dreaming worker still obtains complete episode content from the message store

