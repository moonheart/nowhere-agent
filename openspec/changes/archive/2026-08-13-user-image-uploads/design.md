## Context

Image support (`image-input`, view-image-vision-tool change) uploads into a **session** workspace: `POST /api/chat/sessions/{id}/images` stores the WebP blob under `<root>/<sessionID>/` and returns a session-relative path that the chat message references as an image part. Materialization resolves the path via `workspace.ImageStore.ResolverFor(sessionID)`; the frontend renders it via `GET /api/chat/sessions/{id}/files/{path}`.

Because a new conversation has no session id until its first message, the composer disables the attach button up front. Tencent Yuanbao decouples upload from conversation (a user-level resource + a URL reference in the message). This change brings that model to nowhere-agent while keeping the existing session-scoped path fully intact.

The workspace package already establishes the "pluggable backend" convention: `workspace.Store` documents that an S3-compatible backend implements the same contract, with a local implementation shipped first. This change follows the same pattern for image blobs.

## Goals / Non-Goals

**Goals:**
- A session-independent, user-scoped image upload path so the first message of a chat can carry an image.
- Users can list and delete their own uploads; deletion protects referenced images.
- A `uploads/<id>.webp` path form in the chat wire format, resolved consistently by model materialization and the frontend renderer.
- A `BlobStore` seam so a local backend ships now and S3 can be added later without touching upper layers.

**Non-Goals:**
- S3 (or any remote object store) backend implementation — the seam is the deliverable, not the second implementation.
- Automatic GC / quota for uploads — listing + delete is the management surface; quota and TTL are future work.
- Migrating the existing session-scoped image storage to the `BlobStore` interface — session images keep their current path to avoid regression.
- Multiple-image-per-message batching changes; the existing wire format (top-level `images` array) is reused.

## Decisions

### 1. Blob + metadata table, not bare files
User-level uploads are two layers: a **blob** (the WebP bytes on disk) and a **metadata row** in a new `uploads` table (id, user_id, filename, size, media_type, created_at). The message references only `uploads/<id>.webp`.

- *Why the table?* Listing ("what did I upload?"), cleanup, and reference protection all need queries and metadata (filename/size/time) that a bare directory scan cannot answer reliably.
- *Alternatives considered:* bare directory + scan — rejected (no metadata, no index, weak enumeration). Key-value store — unnecessary, Postgres is already the system of record.

Layout: `<root>/__uploads__/<userID>/<id>.webp`. The reserved directory name `__uploads__` cannot collide with session ids (UUIDs), so session-scoped reads are unaffected.

### 2. `uploads/…` path prefix in the wire format
The image part keeps its single `path` field; a path beginning with `uploads/` denotes a user-level upload, anything else stays a session-relative path.

- *Why a prefix and not a new field or new part type?* It is additive: existing messages, `requestImages`, and the frontend body assembly need no schema change. The prefix is unambiguous because uploads return `uploads/<uuid>.webp` (uuid is the `uploads` row id) and session paths are bare filenames.
- *Alternatives considered:* a `scope` field on the part — more schema surface, and a stale scope field would be easy to get wrong; rejected as unnecessary.

### 3. One resolver that knows both scopes
`ImageStore.ResolverFor(sessionID)` becomes `ResolverFor(sessionID, userID)`. Inside, `ResolveImage(path)` dispatches on the prefix: `uploads/…` reads from the user scope, otherwise from the session scope. `agent.ImageMW` and `provider.MaterializeImages` are unchanged — they only see an `ImageResolver`.

- *Why userID on the resolver?* A run's image parts can mix both forms; the resolver must resolve each from the right scope. The run's session owner is the uploader (same authenticated user), so `userID` is the session's owner id, taken from the run context where the middleware is bound.
- *Why not materialize at ingest time (store bytes on the message)?* Image bytes are referenced by path and materialized per send for prompt-cache determinism; inlining base64 at ingest would change the durable record and cache stability.

### 4. Ownership = the authenticated user, enforced by directory isolation
`GET /api/chat/uploads/{id}` reads `<root>/__uploads__/<currentUserID>/<id>.webp` — the path never carries a user id, so one user cannot address another's upload even with a guessed uuid. `GET /api/me/uploads` and `DELETE /api/me/uploads/{id}` are likewise scoped to the caller.

- *Alternative considered:* embedding the user id in the path (`uploads/<userID>/<id>.webp`). Rejected: it leaks account ids into durable message content and complicates cross-user sharing later.

### 5. Reference protection via message-content lookup
Deletion checks whether any stored message references the upload id (`messages.content::text LIKE '%uploads/<id>%'`) before removing the row + blob; a hit returns 409.

- *Why not a foreign key?* Message content is a JSON document, not a normalized relation, so referential integrity cannot be enforced by the schema. The LIKE scan is a management-path cost (deletion is rare), not a hot path.
- *Risk:* unbounded LIKE over a large `messages` table. *Mitigation:* acceptable at this scale; if it becomes a problem, a trigger-maintained reference table or an indexed text-extract column is a follow-up.

### 6. Blob backend abstraction (local only)
`ImageStore` exposes the user-level blob read/write through a small internal `blobStore` interface; the shipped implementation is the local filesystem. Session-scoped images keep their existing direct-file code path.

- *Why not route session images through it too?* The session path is tested and stable; touching it widens the change's blast radius for no user-visible gain. The seam targets the new user-level surface.
- *Why this shape?* It mirrors `workspace.Store`'s documented "S3-compatible backend implements the same contract" convention, so the future S3 work is additive.

### 7. Wiring / injection
`cmd/server/main.go` builds:
- `uploadStore := upload.NewStore(pool)` (records + orchestration),
- `imageStore := workspace.NewImageStore(cfg.Workspace.Dir)` (blobs),
- passes both to `chatapi` (`WithImageStore` already exists; new `WithUploads`) and to the console handler for the `GET/DELETE /api/me/uploads` routes.

This mirrors how `messageStore`/`skillStore` are injected today — one assembly point, no global state.

## Risks / Trade-offs

- [LIKE reference scan on large message tables] → Mitigation: deletion is rare and administrative; a normalized reference index is a documented follow-up if it bites.
- [Orphan blobs (user attaches, then switches conversation before sending)] → Mitigation: acceptable — the `uploads` listing makes them visible and the delete endpoint removes them; automatic TTL/GC is a non-goal.
- [Prefix ambiguity if a future session path ever begins with `uploads/`] → Mitigation: session paths are bare `<name>.webp` from the store and the reserved dir name is distinct from UUIDs; the convention is documented in the spec.
- [BlobStore only covers user-level blobs] → Trade-off: S3 migration later must also port session images; that is a deliberate scope cut, and the interface lets it happen incrementally.

## Migration Plan

- Add the `uploads` table via a new migration (`000029_uploads.up.sql` / `.down.sql`). It is additive — no existing rows or columns change.
- Ship the new routes and the composer change together; the frontend falls back to the session upload path whenever a session exists, so behavior is identical on existing conversations.
- Rollback: revert the migration and the new routes; existing session images and history are untouched either way.

## Open Questions

- None blocking implementation. (Quota, TTL/GC, and S3 are recorded as non-goals rather than open questions.)
