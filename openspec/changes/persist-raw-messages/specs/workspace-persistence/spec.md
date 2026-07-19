# Spec: workspace-persistence (delta for persist-raw-messages)

## ADDED Requirements

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
