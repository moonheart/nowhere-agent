# view-image-vision-tool Design

## Context

The image pipeline is fully built and tested but has no production ingress: `BlockImage` (`internal/provider/types.go:23`), WebP-normalizing `ImageStore.Save` (`internal/workspace/imagestore.go:75`), byte-stable `MaterializeImages` (`internal/provider/image.go:29`), Anthropic serialization, the authenticated `/files/` GET, and compression accounting all exist. The gaps: (a) no upload endpoint, (b) the chat wire format and `toHistory` accept text only (`internal/chatapi/request.go:49-111`), (c) the OpenAI adapter degrades `BlockImage` to text (`internal/provider/openai/request.go:149-153`), (d) `ModelProfile.ImageInput` (`internal/provider/profiles.go:29`) is defined but never consumed, and (e) the frontend composer has no attachment UI (`web/src/components/thread.tsx:160`), though a dormant shadcn `Attachment` tree exists (`web/src/components/ui/attachment.tsx`).

Models without native image input (DeepSeek, Qwen, o1-mini, gpt-4, …) cannot reason about images today — they only ever see `[image: path]`. This design gives them real vision through a dedicated cheap vision model surfaced as a tool, while vision-capable models keep native image blocks.

## Goals / Non-Goals

**Goals:**
- Users can attach images to a chat; images persist as workspace files with path pointers.
- Vision-capable main models receive native image blocks.
- Non-vision main models get a `view_image` tool backed by a separately configured vision model.
- All new behavior is opt-in via `VISION_*` config; unset keeps today's behavior exactly.
- Backward compatible: no breaking config or API changes.

**Non-Goals:**
- No multi-image batching UX beyond the composer's native multi-select.
- No image generation, editing, or OCR tools.
- No automatic "describe every image" eager behavior — vision calls happen only when the model invokes `view_image`.
- No team-scoped vision credentials (vision calls use the platform `VISION_API_KEY`; see Decisions).
- No scheduled-task attachment ingestion (the scheduled-task trigger keeps a whitelist-filtered registry and no attachment UI).

## Decisions

### D1: `view_image` is a server-side builtin tool, not a provider feature
The vision capability is delivered as a builtin tool (`internal/toolruntime/builtin/view_image.go`) registered per-session into the run registry, exactly like `run_command`. Its `Call` resolves the image bytes through the session's `ImageResolver`, builds a `provider.Request` with the image + question, streams the vision adapter, and returns the assembled text as a `toolruntime.Result`. Tool result text flows through the normal loop → tool_result → next-turn path, so the description is persisted in `messages` and reused on resume (no repeat vision calls).

**Alternatives considered:**
- *Native image blocks always + adapter degrades* (today): model sees `[image: path]`, can't reason. Rejected — this is the gap we close.
- *Eager automatic description on upload*: costs vision tokens for every image even when unneeded, no follow-up questioning. Rejected — lazy, on-demand is cheaper and more flexible.
- *Client-side tool (suspend + hand to frontend)*: would need a vision call from the browser, leaking keys and duplicating provider logic. Rejected — server executes the vision call.

**Rationale:** the tool pattern matches the existing architecture (registry of server-side builtins), keeps the vision call server-side with the platform key, and composes with the loop's existing tool-result persistence and permission gate.

### D2: Vision gate replaces image blocks with a text hint for non-vision main models
A session-scoped middleware (`agent.ImageMW` extended, or a sibling `VisionGateMW`) decides per-send using `LookupProfile(mainProvider, model).ImageInput`:

- **ImageInput true** → leave blocks as-is; `MaterializeImages` + adapter serialize natively.
- **ImageInput false** → rewrite each `BlockImage` in the transient outgoing view to a text hint: `[已附加图片: <path> — 如需查看请调用 view_image 工具]`. This is transient-only (the durable `messages` record keeps the real `BlockImage`), mirroring how `ImageMW` mutates only the per-attempt request copy.

The gate runs on the WrapModelCall chain (transient request), so the durable record is never rewritten. When no vision model is configured, non-vision models simply see the hint with no tool — acceptable degraded UX, and the same as today plus a clearer message.

**Rationale:** gate at the model-serialization boundary so both the durable history and the tool result persistence keep the canonical `BlockImage`; the rewrite is cheap and stateless.

### D3: `VISION_*` config with a reused adapter builder
Add a `Vision` config block (`internal/config/config.go`): `VISION_PROVIDER`, `VISION_MODEL`, `VISION_API_KEY`, `VISION_BASE_URL`. The existing `buildProviderWithKey` (`cmd/server/main.go:1063`) is parameterized to build the vision adapter from these settings. Unset provider/model ⇒ no vision adapter ⇒ no `view_image` registration ⇒ today's behavior.

Vision calls use the **platform** `VISION_API_KEY`, not team keys: the tool is bound per-session at registry build time, where no per-caller key context exists, and team keys are already provider-specific (model-routing). Document that vision spend attributes to the platform key; team attribution of vision calls is a non-goal.

**Rationale:** reuses the proven adapter factory, keeps one config surface, and avoids the team-key complexity of nested tool-time credential resolution.

### D4: Upload endpoint + wire-format image part
- New authenticated route `POST /api/chat/sessions/{id}/images` (registered beside the `/files/` GET at `cmd/server/main.go:178`) that reads multipart or raw body, calls `ImageStore.Save`, and returns JSON `{"path":"<session-relative>.webp"}`. Authorization reuses `authorizeSession` (`internal/chatapi/files.go:24`). Size cap enforced (e.g. 10 MB) before decode.
- `incomingPart` (`internal/chatapi/request.go:55`) gains `{type:"image", mediaType, path}`; `extractText` ignores it; a new `extractBlocks`/extended `toHistory` turns image parts into `provider.Block{Type: BlockImage, MediaType, ImagePath}` appended to the user message. The user turn assembly at `handler.go:340-344` builds the full-block user message (text + image blocks) so the durable record persists the pointer.

The frontend uploads first (gets the path), then includes image parts in the next POST — so the chat body stays JSON, no multipart mixing.

**Rationale:** the message store already persists full `Block` content (`session.StoredMessage`), so an image part only needs to become a `BlockImage` at `toHistory` time — no new storage.

### D5: OpenAI adapter native `image_url` serialization
`openai/request.go`'s `apiMessage.Content` is currently a `string` (per the gateway contract). Change `convertMessage` so that, when the model's profile reports `ImageInput`, a message carrying `BlockImage` emits content parts `[{type:"text",text}, {type:"image_url", image_url:{url:"data:image/webp;base64,…"}}]`. When the profile reports no `ImageInput` (or the block has no materialized data), keep the existing `[image: path]` text degradation. This requires `apiMessage.Content` to become `json.RawMessage` (string or parts array) — verify the OpenAI-compat gateway accepts the parts form before wiring (fallback: keep degrade and rely on `view_image` for OpenAI).

**Rationale:** the vision model might itself be OpenAI-compatible (e.g. qwen-vl, gpt-4o-mini); `view_image` would send the image through this same adapter, so native OpenAI image parts are a prerequisite for a cheap OpenAI-family vision model. If the gateway rejects parts, the fallback is to prefer an Anthropic vision model (whose adapter already serializes images).

### D6: Frontend attachment via the dormant shadcn `Attachment` tree
In `web/src/components/thread.tsx`, wire the existing `Attachment`/`AttachmentMedia` components (`web/src/components/ui/attachment.tsx`) into the composer: a file picker that uploads to the new endpoint and adds an attachment chip; `web/src/App.tsx` includes `images: [{path, mediaType}]` in the POST; `web/src/lib/history.ts` maps `image` parts to `<img src="/api/chat/sessions/{id}/files/{path}">` for rendering. `web/src/lib/api.ts` gains a multipart upload helper.

**Rationale:** reuses the already-shipped shadcn components (no new UI dependency); rendering goes through the existing authenticated `/files/` route so the browser never needs the base64.

## Risks / Trade-offs

- **[Vision latency]** The `view_image` call is a synchronous nested LLM stream inside the loop's tool dispatch → the run pauses one vision round-trip. → Bound with a generous tool `Timeout` and a compact vision system prompt ("describe/answer briefly"); the description is cached in the durable tool result so later turns don't re-call.
- **[Vision cost]** Each `view_image` spends vision tokens, attributed to the platform key. → Lazy on-demand design (only when the model asks); cheap model encouraged in docs; risk surfaces in usage reporting, not the code path.
- **[OpenAI gateway rejects image parts]** The comment at `openai/request.go:151` warns the in-use gateway rejects image parts. → Implement with a runtime-degradation fallback: if the gateway 400s on parts, retry without images (or prefer an Anthropic vision model in docs). This risk is localized to D5 and does not block D1–D4.
- **[Model hallucinates the hint without calling view_image]** Non-vision models see a hint and may ignore the tool. → The hint text explicitly names the tool; the tool is always available when a vision model is configured; an unknown-profile model defaults to non-vision behavior (conservative).
- **[Upload abuse]** New upload surface could be used to fill the workspace. → Auth + session-owner check, size cap, WebP normalization bounds stored bytes; images are confined per session like all workspace files.

## Migration Plan

1. Backend-first, additive: config block, tool, gate middleware, upload endpoint, wire-format parts. No existing behavior changes while `VISION_*` is unset.
2. OpenAI adapter image parts behind `ImageInput` gating — degraded path unchanged for non-vision models.
3. Frontend attachment UI (feature-flagged by the endpoint existing).
4. Deploy order: config + backend, then frontend build (`WEB_DIR`). Rollback: unset `VISION_*` and stop serving the new frontend build — server remains compatible with the old client (image parts simply aren't sent).

## Open Questions

- **OpenAI-compat gateway & `image_url` parts**: confirm the actual gateway accepts parts-form content before enabling native OpenAI image serialization (D5). Until verified, document an Anthropic vision model as the supported path.
- **Upload size cap**: pick a concrete default (10 MB raw) — reasonable for screenshots/diagrams; revisit for camera-origin photos.
- **Scheduled-task attachment ingestion**: explicitly out of scope; confirm no scheduled-task need for image attachments before a follow-up change.
