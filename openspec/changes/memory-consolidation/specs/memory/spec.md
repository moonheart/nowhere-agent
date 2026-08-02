# memory — delta for memory-consolidation

## MODIFIED Requirements

### Requirement: MemoryPort interface
The system SHALL define a MemoryPort for long-term memory with a read side (Recall) and a write
side (Store, Update, Deprecate, Forget, ListByScope, GetByID).

The write side SHALL support revising an existing memory's content in place, preserving its
identity and creation time and recording that it changed. Revision SHALL invalidate any stored
embedding for that memory, because an embedding describes the text it was derived from and would
otherwise rank the memory by content it no longer holds.

#### Scenario: Backend-agnostic access
- **WHEN** memory is read or written
- **THEN** it goes through MemoryPort regardless of the backing store

#### Scenario: In-place revision keeps identity
- **WHEN** a memory's content is updated
- **THEN** its identifier and creation time are unchanged and its update time reflects the revision

#### Scenario: Revision invalidates the embedding
- **WHEN** a memory's content is updated
- **THEN** any embedding stored for it is cleared, so semantic recall cannot rank it by its previous content

#### Scenario: Updating an absent memory
- **WHEN** an update names a memory that does not exist
- **THEN** the caller is told it was not found and no memory is modified
