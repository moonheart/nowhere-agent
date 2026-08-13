# Tasks for user-image-uploads

## 1. Schema

- [x] 1.1 Add migration `000029_uploads.up.sql` / `.down.sql`: `uploads` table (`id UUID PK`, `user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE`, `filename TEXT NOT NULL`, `size BIGINT NOT NULL`, `media_type TEXT NOT NULL DEFAULT 'image/webp'`, `created_at TIMESTAMPTZ NOT NULL DEFAULT now()`) with an index on `(user_id, created_at)`
- [x] 1.2 Apply the migration to the dev DB

## 2. Blob seam in workspace.ImageStore

- [x] 2.1 Add an internal `blobStore` interface in `internal/workspace/imagestore.go` (write/read a user-level blob by id) with a local-filesystem implementation under `<root>/__uploads__/<userID>/<id>.webp`; mirror the `workspace.Store` "S3-compatible later" convention in a comment
- [x] 2.2 Add `SaveUserUpload(userID, name string, raw []byte) (string, error)` returning `uploads/<id>.webp` (WebP-normalized via the existing decode/encode path, size-capped by the caller) and `OpenUserUpload(userID, path string) (io.ReadCloser, error)` for `uploads/…` paths confined to the user's dir
- [x] 2.3 Change `ResolverFor(sessionID string)` to `ResolverFor(sessionID, userID string)`; `ResolveImage` dispatches `uploads/…` to the user scope and everything else to the session scope
- [x] 2.4 Add `internal/workspace/imagestore_test.go` cases: user-level save/read round-trip, WebP normalization + unsupported payload, cross-user isolation, `uploads/` prefix dispatch vs session paths, path-escape confinement for user blobs

## 3. upload package (records + orchestration)

- [x] 3.1 Add `internal/upload/store.go`: `Upload` type (ID, UserID, Filename, Size, MediaType, CreatedAt), a `Store` interface, and a PG implementation with `Create`, `ListByUser`, `Get`, `Delete`, and `ReferencedByMessage(ctx, uploadID)` (a `messages.content::text LIKE '%uploads/<id>%'` scan)
- [x] 3.2 Add `internal/upload/service.go`: `Upload(ctx, userID, name, raw)` = decode/validate → blob write → record insert (one id); `Delete(ctx, userID, id)` rejects referenced uploads with an `ErrReferenced` sentinel, deletes record + blob otherwise
- [x] 3.3 Add store/service tests (PG): create/list/delete, ownership scoping, reference detection, referenced-delete rejection, idempotent-ish 404s

## 4. chatapi routes

- [x] 4.1 Add `serveUserImageUpload` (`POST /api/chat/uploads`, authenticated, size cap, returns `{path}`) and `serveUserFile` (`GET /api/chat/uploads/{id}`, owner-only read) in `internal/chatapi/files.go`
- [x] 4.2 Add `WithUploads(s upload.Uploader)` to the chat handler and register both routes in `Register`/`RegisterAuthed`
- [x] 4.3 In `internal/chatapi/memoryinject.go` `bindSessionMiddleware`, pass the run's user id to `ResolverFor(sessionID, userID)` for `ImageMW`
- [x] 4.4 Add handler tests: user-level upload returns an `uploads/…` path, owner-only file read (non-owner 404), and a first-message request whose image part uses an `uploads/…` path

## 5. Console self-service routes

- [x] 5.1 Add `GET /api/me/uploads` (list caller's uploads, newest first) and `DELETE /api/me/uploads/{id}` (204; 404 not-owned/unknown; 409 referenced) to the console handler (`internal/adminapi` self tier), with audit on delete
- [x] 5.2 Add route tests: listing scoped to caller, delete own unreferenced upload, 409 on referenced upload, 404 on another user's upload

## 6. main.go wiring

- [x] 6.1 In `cmd/server/main.go`, build `upload.NewStore(pool)` (+ optional service), keep the workspace `ImageStore`, and inject both into the chat handler (`WithImageStore` + `WithUploads`) and the upload endpoints into the console handler
- [x] 6.2 `go build ./...` clean

## 7. Frontend

- [x] 7.1 Add `uploadUserImage(file)` to `web/src/lib/api.ts` (POST `/api/chat/uploads`, raw body, returns `{path}`)
- [x] 7.2 In `web/src/lib/image-attachment.ts`, make `imageFileUrl(sessionId, path)` return `/api/chat/uploads/{id}` for `uploads/…` paths and the existing session file URL otherwise
- [x] 7.3 In `web/src/components/thread.tsx`, enable the attach button without a session; pick the user-level upload endpoint when `sessionId` is null and the session endpoint otherwise; keep chips rendering via the updated `imageFileUrl`
- [x] 7.4 `pnpm lint` + `npx tsc --noEmit -p tsconfig.app.json` clean

## 8. Verification

- [x] 8.1 `go test ./...` green (including new workspace/upload/chatapi/adminapi tests)
- [x] 8.2 `go vet ./...` clean
- [x] 8.3 `pnpm build` succeeds
