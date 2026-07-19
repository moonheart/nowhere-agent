# sandbox — delta for file-tools

## ADDED Requirements

### Requirement: Local filesystem sandbox backend
The system SHALL provide a `Port` backend that isolates a session's files in a host directory, for development and single-tenant self-hosting where a container runtime is unavailable. This backend SHALL confine all file operations to the session workspace.

#### Scenario: Session workspace created
- **WHEN** a local sandbox is created for a session
- **THEN** a per-session workspace directory is created and subsequent `ReadFile`/`WriteFile`/`ListDir` operate within it

#### Scenario: Path escape prevented
- **WHEN** a file operation uses a path with `..`, an absolute path, or a symlink resolving outside the workspace
- **THEN** the backend rejects the operation rather than touching a file outside the workspace

#### Scenario: Backend selected by configuration
- **WHEN** the server is configured with a sandbox backend
- **THEN** it uses the local filesystem backend or the Docker backend interchangeably through the same `Port` interface
