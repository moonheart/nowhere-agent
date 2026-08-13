## Why

Image upload is bound to a specific session (`POST /api/chat/sessions/{id}/images`), so a brand-new conversation cannot attach an image — the composer disables the button until the first message creates a session. Tencent Yuanbao solves this by treating upload as a *user-level* resource: the client asks the server for an upload grant, stores the file, and the message references the resource URL, with no dependency on a conversation existing yet. This change adopts that model for nowhere-agent: images become user-scoped resources a message can reference, so the first message of a chat can carry an image, and users can view and clean up what they uploaded.

## What Changes

- **User-level image upload** (`POST /api/chat/uploads`): stores the image (WebP-normalized) as a user-scoped blob and records its metadata in a new `uploads` table. No session is required — this is what unlocks first-message attachments.
- **User-level resource management** (`GET /api/me/uploads`, `DELETE /api/me/uploads/{id}`): users can list their uploads (original filename, size, time) and delete unreferenced ones; deletion of an image still referenced by a message is rejected (409) to protect history.
- **Wire-format extension**: the image part `path` gains a `uploads/<id>.webp` prefix form alongside the existing session-relative path. Model materialization (`ImageResolver`) and the frontend renderer both resolve both forms.
- **Composer unlocks first-message images**: the attach button is enabled before a session exists; without a session the client uploads via the user-level endpoint, with one via the session endpoint.
- **`BlobStore` seam for future S3**: `ImageStore` routes blob I/O through a small internal interface (local implementation now), mirroring the existing `workspace.Store` "local first, S3-compatible later" convention. S3 is explicitly out of scope for this change.

## Capabilities

### New Capabilities
- `user-image-uploads`: user-scoped image resources — session-independent upload ingest, metadata tracking, listing and cleanup with reference protection, the `uploads/…` path form in the chat wire format, and its resolution in model materialization and frontend rendering.

### Modified Capabilities
<!-- image-input is not yet a main spec (the view-image-vision-tool change is still open); the incremental behavior — upload no longer requiring a session, and the wire format accepting uploads/ paths — is specified inside the new user-image-uploads capability. -->

## Impact

- **`migrations/`** — new `uploads` table (id, user_id, filename, size, media_type, created_at).
- **`internal/workspace/imagestore.go`** — `BlobStore` seam; user-scoped `Save`/`Open`; `ResolverFor` gains the user id and resolves `uploads/…` paths from the user scope, other paths from the session scope.
- **`internal/upload`** (new package) — DB-backed record store + upload/list/delete orchestration with reference protection.
- **`internal/chatapi/files.go` + `handler.go`** — user-level upload/serve routes; `ResolverFor(sessionID, userID)`.
- **`internal/adminapi`** (or console self-service) — `GET /api/me/uploads`, `DELETE /api/me/uploads/{id}` routes.
- **`cmd/server/main.go`** — wire the user upload store into chatapi and the console.
- **`web/src/lib/api.ts`** — `uploadUserImage`.
- **`web/src/lib/image-attachment.ts`** — `imageFileUrl` handles `uploads/…` paths.
- **`web/src/components/thread.tsx`** — attach button enabled without a session; picks the upload endpoint by session presence.
- Backward compatible: existing session uploads, stored messages, and history rendering are unchanged; the `uploads/…` path is purely additive.
