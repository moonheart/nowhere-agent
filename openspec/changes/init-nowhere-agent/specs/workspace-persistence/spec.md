# Spec: workspace-persistence

## ADDED Requirements

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
