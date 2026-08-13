## ADDED Requirements

### Requirement: User-level image upload
The system SHALL provide an authenticated image upload endpoint that does not require a session (`POST /api/chat/uploads`). It SHALL decode the payload (PNG/JPEG/GIF/WebP), reject unsupported or malformed bytes with 415, re-encode to WebP, store it as a blob under the uploading user's scope, and return a `path` of the form `uploads/<id>.webp`. It SHALL reject payloads larger than the configured cap with 413.

#### Scenario: Upload without a session
- **WHEN** an authenticated user uploads a valid image to `POST /api/chat/uploads`
- **THEN** the server stores a WebP blob under the user's scope and responds 200 with `{"path":"uploads/<id>.webp"}`

#### Scenario: Unsupported payload
- **WHEN** a user uploads bytes that are not a decodable image
- **THEN** the server responds 415 and stores nothing

#### Scenario: Oversized payload
- **WHEN** a user uploads a payload larger than the size cap
- **THEN** the server responds 413 and stores nothing

#### Scenario: Unauthenticated upload
- **WHEN** an unauthenticated request hits `POST /api/chat/uploads`
- **THEN** the server rejects it with 401

### Requirement: Upload metadata record
The system SHALL persist a metadata record for every user-level upload — the upload id, the owning user id, the original filename, the byte size, the media type, and the creation time. The blob is referenced by this id; the metadata record is the authoritative index for listing and cleanup.

#### Scenario: Upload is recorded
- **WHEN** a user uploads an image
- **THEN** a row exists in the uploads store carrying the returned id, the user id, filename, size, media type, and created-at timestamp

### Requirement: List own uploads
The system SHALL provide an authenticated endpoint (`GET /api/me/uploads`) that returns the current user's uploads with their id, original filename, size, media type, and creation time, newest first.

#### Scenario: User lists uploads
- **WHEN** a user requests `GET /api/me/uploads`
- **THEN** the server returns only that user's uploads, newest first

#### Scenario: Empty list
- **WHEN** a user with no uploads requests `GET /api/me/uploads`
- **THEN** the server returns an empty list

### Requirement: Delete own upload with reference protection
The system SHALL provide an authenticated endpoint (`DELETE /api/me/uploads/{id}`) that removes an upload's metadata record and blob. Deletion SHALL be rejected with 409 when the upload is still referenced by any stored message (protecting history images); SHALL respond 404 for an id the current user does not own or that does not exist; and SHALL be idempotent-safe (an already-deleted id responds 404).

#### Scenario: Delete unreferenced upload
- **WHEN** a user deletes an upload no message references
- **THEN** the server removes the record and blob and responds 204

#### Scenario: Delete referenced upload
- **WHEN** a user deletes an upload that a stored message references
- **THEN** the server responds 409 with an explanatory message and keeps the record and blob

#### Scenario: Delete someone else's upload
- **WHEN** a user deletes an upload they do not own
- **THEN** the server responds 404

### Requirement: Wire-format uploads path
The chat image part `path` SHALL accept two forms: a session-relative path (existing behavior) and a user-level `uploads/<id>.webp` path. Model materialization SHALL resolve `uploads/…` paths from the run's user scope and session-relative paths from the session scope. The frontend renderer SHALL resolve `uploads/…` paths via the user-level read endpoint and session-relative paths via the session file endpoint.

#### Scenario: First message carries a user-level image
- **WHEN** a chat request for a brand-new session carries an image part with `path: "uploads/<id>.webp"`
- **THEN** the server persists the user-level image in the user turn and, on send, materializes its bytes from the user scope for the model

#### Scenario: History renders a user-level image
- **WHEN** the frontend renders a stored message whose image part has an `uploads/…` path
- **THEN** the image loads from the user-level read endpoint for the session owner

#### Scenario: Existing session images still resolve
- **WHEN** a message references a session-relative image path
- **THEN** the image resolves from the session scope exactly as before

### Requirement: First-message attachment
The composer SHALL allow attaching an image before a session exists: the attach control SHALL be enabled, and the client SHALL upload via the user-level endpoint when there is no session and via the session endpoint when there is one.

#### Scenario: Attach on a new conversation
- **WHEN** a user is in a conversation with no session yet and picks an image
- **THEN** the image uploads via the user-level endpoint and is sent with the first message

#### Scenario: Attach on an existing conversation
- **WHEN** a user is in a conversation that has a session and picks an image
- **THEN** the image uploads via the session endpoint, as today

### Requirement: Blob backend abstraction
Blob I/O for user-level uploads SHALL go through an internal interface so the storage backend can be swapped. This change SHALL ship the local-filesystem implementation only; an S3-compatible backend is explicitly out of scope.

#### Scenario: Local backend serves uploads
- **WHEN** the local blob backend is configured
- **THEN** user-level blobs are stored under the configured root in a user-scoped directory and read back on demand
