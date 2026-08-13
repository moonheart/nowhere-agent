## Why

The platform's image pipeline (ingest → WebP → path pointer → byte-stable materialization) is built and tested, but no production path lets a user attach an image, and models without native image input (DeepSeek, Qwen, o1-mini, gpt-4, etc.) can only degrade images to a useless `[image: path]` text placeholder. Users need to attach images to a chat and have the agent actually reason about them, regardless of the main model's vision capability.

## What Changes

- **Upload ingest**: an authenticated upload endpoint that stores an image via `ImageStore.Save` (WebP-normalized) and returns a session-relative path; the chat wire format accepts an `image` part and turns it into a `provider.BlockImage` in the user turn.
- **Native vision for capable models**: when the main model profile reports `ImageInput`, image blocks pass through to the provider natively (Anthropic path already works; OpenAI adapter gains real `image_url` serialization instead of the current text degradation).
- **`view_image` tool for non-vision models**: when the main model profile reports no `ImageInput` AND a vision model is configured, the loop registers a `view_image` tool that sends the image (via the session's `ImageResolver`) to a cheap, separately-configured vision model and returns the description as a text tool result. Image blocks in the outgoing view are replaced with a text hint pointing the model at the tool.
- **`VISION_*` configuration**: a new config block (`VISION_PROVIDER`/`VISION_MODEL`/`VISION_API_KEY`/`VISION_BASE_URL`) builds a dedicated vision adapter. Unset keeps today's behavior (no tool, image blocks degrade to placeholders) — fully backward compatible.
- **Frontend**: composer gains an attachment picker, uploads via the new endpoint, and renders attached images (reusing the dormant shadcn `Attachment` tree and the existing `/files/` serving route).

## Capabilities

### New Capabilities
- `image-input`: end-to-end image support for chat — upload ingest, `BlockImage` in the wire format, native image blocks for vision-capable models, and the `view_image` tool that lets a non-vision main model reason about images through a configured vision model.

### Modified Capabilities
- `provider-abstraction`: adapters SHALL serialize image blocks to their provider-native image source whenever the provider supports it; the OpenAI adapter's current degrade-to-text behavior for `BlockImage` changes to native `image_url` parts.

## Impact

- **`internal/config/config.go`** — new `Vision` config block (`VISION_*`).
- **`internal/chatapi`** — upload endpoint (alongside `handler.go:178`'s file route), `dataStreamRequest`/`toHistory` accept `image` parts, produce `provider.BlockImage`.
- **`internal/toolruntime/builtin`** — new `view_image` tool.
- **`internal/agent/middleware.go`** — vision gate: replace `BlockImage` with a text hint for non-vision main models (transient view only, mirroring `ImageMW`).
- **`internal/provider/openai/request.go`** — native `image_url` content serialization.
- **`cmd/server/main.go`** — `VISION_*` adapter construction (`buildProviderWithKey` reuse), conditional `view_image` registration in `buildToolRegistry`.
- **`web/src/components/thread.tsx`, `web/src/lib/App.tsx`, `web/src/lib/api.ts`, `web/src/lib/history.ts`** — attachment UI + upload + image rendering.
- Backward compatible: no existing config or API breaks; all new behavior is opt-in via `VISION_*` (and requires the new upload endpoint).
