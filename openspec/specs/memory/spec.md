# memory Specification

## Purpose
TBD - created by archiving change init-nowhere-agent. Update Purpose after archive.
## Requirements
### Requirement: MemoryPort interface
The system SHALL define a MemoryPort for long-term memory with a read side (Recall) and a write side (Store, Forget, ListByScope).

#### Scenario: Backend-agnostic access
- **WHEN** memory is read or written
- **THEN** it goes through MemoryPort regardless of the backing store

### Requirement: Read/write split
Long-term memory reads SHALL be performed online by the agent loop; writes SHALL be performed exclusively by the dreaming worker. The loop SHALL NOT write long-term memory directly.

#### Scenario: Online read
- **WHEN** the loop recalls memories
- **THEN** the read path is low-latency and cacheable, and does not block on writes

#### Scenario: Single writer
- **WHEN** any long-term memory is created, updated, or deprecated
- **THEN** it is performed by the dreaming worker, never the loop

### Requirement: Scoped isolation
Memories SHALL carry a scope (user/team/system) and SHALL only be recalled within authorized scopes. One user's private memories SHALL never be recalled for another user.

#### Scenario: Cross-user isolation
- **WHEN** user A and user B both have private memories
- **THEN** recalling for user A never returns user B's private memories

#### Scenario: Team-scope recall
- **WHEN** a memory is team-scoped
- **THEN** it is recallable only by members of that team

### Requirement: Forgetting
The system SHALL support permanent deletion of memories (e.g., for GDPR erasure).

#### Scenario: Delete a memory
- **WHEN** Forget is called for a memory
- **THEN** it is removed and no longer recallable by any scope

### Requirement: Pluggable backend
The built-in implementation SHALL use Postgres plus a vector index; alternative memory frameworks (e.g., Mem0/Zep) SHALL be addable behind MemoryPort without changing consumers.

#### Scenario: Swap backend
- **WHEN** a different memory framework is configured
- **THEN** read-retrieval and write-extraction can be replaced independently without changing the loop or dreaming callers

