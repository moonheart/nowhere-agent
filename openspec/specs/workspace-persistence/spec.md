# workspace-persistence Specification

## Purpose
TBD - created by archiving change init-nowhere-agent. Update Purpose after archive.
## Requirements
### Requirement: Disposable compute, durable data
Sandbox containers SHALL be disposable; the workspace SHALL live on an external persistent volume independent of container lifetime.

#### Scenario: Container destroyed, data survives
- **WHEN** a session's container is destroyed
- **THEN** the session's workspace files remain intact in the persistent store

### Requirement: Materialize on start, solidify on end
The system SHALL materialize the workspace into the sandbox at session start and solidify it back to the persistent store on session end or idle.

#### Scenario: Restore on start
- **WHEN** a session starts
- **THEN** its persisted workspace is pulled into the sandbox before the agent operates

#### Scenario: Persist on end
- **WHEN** a session ends or goes idle
- **THEN** the current workspace is pushed back to the persistent store

### Requirement: Sync-model abstraction
Workspace movement SHALL follow a sync (push/pull) model rather than a bind-mount, so the persistent backend can be object storage.

#### Scenario: Backend independence
- **WHEN** the persistent backend is swapped
- **THEN** the materialize/solidify contract is unchanged for consumers

### Requirement: Pluggable storage backend
The built-in backend SHALL use a local directory; an S3-compatible backend (e.g., MinIO) SHALL be addable behind the same abstraction.

#### Scenario: Local-directory default
- **WHEN** running with default configuration
- **THEN** workspaces persist to a configured local directory

#### Scenario: S3-compatible backend
- **WHEN** an S3-compatible backend is configured
- **THEN** workspaces persist to object storage without interface changes

### Requirement: Reactivation after long idle
The system SHALL restore a session's workspace even after a long idle gap.

#### Scenario: Long-gap reactivation
- **WHEN** a session is resumed long after its sandbox was destroyed
- **THEN** its workspace is restored to the last solidified state

### Requirement: Atomic solidify
Solidify SHALL be atomic: the new snapshot is staged and validated before the durable pointer is switched, so an interrupted solidify never leaves a partial workspace.

#### Scenario: Interrupted solidify
- **WHEN** the container is killed partway through solidify
- **THEN** the durable workspace still points at the last complete version, not a half-written one

### Requirement: Image payloads stored as workspace files
Image payloads referenced by conversation messages SHALL be stored as files in the owning session's workspace; the message store SHALL hold only a workspace-relative path pointer. Images SHALL be normalized to WebP on ingest to bound payload size.

#### Scenario: Image stored as file, pointer in message
- **WHEN** an image enters a conversation (tool output or upload)
- **THEN** the image bytes are written to the session workspace and the message block records the media type and workspace-relative path

#### Scenario: WebP normalization
- **WHEN** an image is ingested into the workspace
- **THEN** it is re-encoded to WebP (`image/webp`) to reduce payload size

### Requirement: Path confinement
Workspace-relative paths referenced by messages SHALL be resolved and confined to the owning session's workspace root at materialization time. Path traversal outside the session workspace SHALL be rejected.

#### Scenario: Traversal rejected
- **WHEN** a stored image path contains `..`, is absolute, or escapes the session workspace (e.g. via symlink)
- **THEN** materialization rejects it rather than reading outside the session boundary

### Requirement: Authenticated image retrieval for clients
Images referenced in conversation messages SHALL be retrievable by the session owner through a dedicated authenticated endpoint, so image payloads are not inlined into the event stream.

#### Scenario: Owner retrieves image
- **WHEN** the session owner requests an image by its workspace path via the file endpoint
- **THEN** the image bytes are served

#### Scenario: Non-owner denied
- **WHEN** a principal that does not own the session requests the image
- **THEN** the request is denied

