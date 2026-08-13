# Tasks for view-image-vision-tool

## 1. Vision config block

- [x] 1.1 Add `Vision` struct to `internal/config/config.go` with `VISION_PROVIDER`, `VISION_MODEL`, `VISION_API_KEY`, `VISION_BASE_URL` envconfig fields (empty defaults)
- [x] 1.2 Parameterize `buildProviderWithKey` (`cmd/server/main.go:1063`) or add a sibling `buildVisionProvider` that builds the vision adapter from the `Vision` config, returning nil when provider/model unset
- [x] 1.3 Construct the vision adapter in `main()` and thread it into the chat handler wiring (nil-safe)

## 2. view_image tool

- [x] 2.1 Add `internal/toolruntime/builtin/view_image.go` implementing `toolruntime.Tool`: `Name()=="view_image"`, `Risk()==RiskReadOnly`, a generous `Timeout()`, schema `{path: string, question?: string}`
- [x] 2.2 Implement `Call`: resolve image bytes via the injected `provider.ImageResolver`, build a `provider.Request` (compact system prompt + user message with image block + question), stream the vision adapter, assemble the text response
- [x] 2.3 Degrade to an error result when the path does not resolve, and surface vision-model/stream errors as error results so the main model can self-correct
- [x] 2.4 Add unit tests for the tool (`view_image_test.go`): valid path returns description, missing path returns error result, question is forwarded

## 3. Vision gate middleware

- [x] 3.1 Add a vision-gate middleware (extend `agent.ImageMW` or add `VisionGateMW` in `internal/agent/middleware.go`) that, per send, consults `LookupProfile(mainProvider, model).ImageInput`
- [x] 3.2 When `ImageInput` is false, rewrite each `BlockImage` in the transient outgoing view to a text hint naming the `view_image` tool; when true (or unknown-profile and vision configured) leave blocks for native serialization
- [x] 3.3 Wire the gate into `bindSessionMiddleware` (`internal/chatapi/memoryinject.go:105`) alongside `ImageMW`
- [x] 3.4 Add unit tests covering: vision model leaves blocks, non-vision model rewrites to hint, unknown model falls back conservatively, durable record untouched

## 4. Upload endpoint

- [x] 4.1 Add `POST /api/chat/sessions/{id}/images` route in `cmd/server/main.go` beside the `/files/` GET, calling `Handler.serveImageUpload`
- [x] 4.2 Implement `serveImageUpload` in `internal/chatapi`: authorize via `authorizeSession`, enforce a size cap, call `ImageStore.Save`, return JSON `{"path":"<rel>.webp"}`
- [x] 4.3 Handle decode/save errors and non-owner/non-existent-session with proper status codes; add tests in `internal/chatapi/files_test.go`

## 5. Chat wire-format image parts

- [x] 5.1 Extend `incomingPart` (`internal/chatapi/request.go:55`) with `{type:"image", mediaType, path}` and make `extractText` skip non-text parts
- [x] 5.2 Extend `toHistory` to emit `provider.Block{Type: BlockImage, MediaType, ImagePath}` for image parts, appended to the user message (full-block user turn at `handler.go:340-344`)
- [x] 5.3 Add unit tests in `internal/chatapi/request_test.go` for mixed text+image messages and image-only messages

## 6. OpenAI adapter native image serialization

- [x] 6.1 Change `apiMessage.Content` in `internal/provider/openai/request.go` to a flexible form (string or parts array) without breaking existing serialization
- [x] 6.2 In `convertMessage`, when the model profile reports `ImageInput` and a `BlockImage` has materialized `ImageData`, emit `image_url` content parts (`data:image/webp;base64,…`)
- [x] 6.3 Keep the `[image: path]` text degradation for non-vision profiles and unmaterialized/dangling images; gate on `LookupProfile`
- [x] 6.4 Add adapter tests: vision model emits image_url parts, non-vision model still degrades, mixed text+image message flattens correctly

## 7. Frontend attachment UI

- [x] 7.1 Add a multipart upload helper to `web/src/lib/api.ts` (upload to `/api/chat/sessions/{id}/images`, returns `{path}`)
- [x] 7.2 Wire a file picker + attachment chips into the composer (`web/src/components/thread.tsx`) using the shadcn `Attachment`/`AttachmentMedia` components; include `images:[{path, mediaType}]` in the chat POST (`web/src/App.tsx`)
- [x] 7.3 Extend `web/src/lib/history.ts` to map `image` parts to `<img>` via the `/files/` endpoint so images render and survive resume
- [x] 7.4 Verify with `pnpm lint` and `npx tsc --noEmit -p tsconfig.app.json` (both clean; `pnpm build` also passes)

## 8. Verification

- [x] 8.1 `go build ./...` and `go vet ./...` clean
- [x] 8.2 `go test ./...` green (including new chatapi, provider/openai, agent, toolruntime/builtin tests)
- [ ] 8.3 Manual smoke: attach image → non-vision model calls `view_image` and reasons about the description; vision-capable model receives native image; `VISION_*` unset preserves old behavior
