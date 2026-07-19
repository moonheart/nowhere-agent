# Design: persist-raw-messages

## D1 — One row per canonical message; `messages` is the content source of truth

We introduce a `messages` table holding canonical `provider.Message` rows, and keep `run_events` for what it already does well (lifecycle, cancel, attach fan-out). The two stores answer different questions:

- `run_events` — "what happened in this run, as an ordered event stream?" (drives attach/replay/resume, already built).
- `messages` — "what is the conversation, as faithful `[]Message` with full blocks?" (drives history rebuild, and later compression/dreaming).

Why not overload `run_events.payload` with whole messages? `run_events` is an *event* log keyed by `(run_id, offset)`; a conversation message is not an event — an assistant turn is assembled from many streaming deltas, and tool results arrive as a separate user-role message. Reconstructing messages by folding an event stream is exactly the fragile aggregation we are removing. Storing messages directly is the CC transcript model and is strictly simpler to read back.

**Schema (migration 000006):**
```sql
CREATE TABLE messages (
  id          BIGSERIAL PRIMARY KEY,
  session_id  TEXT NOT NULL,
  run_id      TEXT NOT NULL,
  seq         INTEGER NOT NULL,            -- monotonic within a session
  role        TEXT NOT NULL,               -- user | assistant
  content     JSONB NOT NULL,              -- []provider.Block, full fidelity
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX messages_session_seq ON messages(session_id, seq);
```
- `content` stores the `[]provider.Block` verbatim: `{type, text?, thinking?, thinking_signature?, tool_use_id?, tool_name?, tool_input?, tool_result_id?, tool_content?, is_error?}`. JSONB keeps it dimension/shape-agnostic and matches how we already persist `run_events.payload`.
- Image blocks are the one exception to "verbatim": they store a **pointer**, never the payload — `{type:"image", media_type, image_path}` where `image_path` is workspace-relative (see D6). A stored row stays KB-sized even for large images.
- `seq` is a per-session monotonic sequence so history order is stable across runs. (Compare `run_events.offset`, which is per-run.)
- Scope owner ids are TEXT app-level identifiers, consistent with `memories` (no FK rows), per the existing convention.

## D2 — Persist full blocks from the loop's assembled messages

The loop already assembles complete `assistant` messages (`loop.go:118`) and tool-result messages (`loop.go:142`). We persist these as `messages` rows at the point they are appended to `produced`:

- Each finished **assistant** message → one row (text + thinking(+signature) + tool_use blocks, in order).
- Each **tool-result** message → one row (`role=user`, tool_result blocks). This is the canonical shape providers expect on the wire and is exactly what compression must see.
- The **incoming user** message → one row at run start (today only its text is persisted as a `KindUser` event; we now persist the full message row too).

`run_events` continues to receive the same lifecycle + streaming frames it does today (attach/resume unchanged). The `messages` write is additive and happens on the same run-worker path, so it inherits run-ownership (survives submitter disconnect) for free.

## D3 — Fix thinking-signature capture (prerequisite for D2)

Signature must round-trip for Anthropic thinking. Today it is destroyed:

- `anthropic/stream.go` `deltaText("signature_delta")` returns the signature as ordinary `Delta` text.
- `loop.go` `accumulator.append` does `a.text += delta` for thinking blocks, so the signature is appended into the thinking body and `Block.ThinkingSignature` is never set.

Fix: distinguish the signature at the event level. `provider.Event` gains a `SignatureDelta string` (or a dedicated event type); the Anthropic decoder routes `signature_delta` there instead of into `Delta`. The loop's accumulator assigns it to `block.ThinkingSignature` on `finalize`, leaving `Thinking` as pure reasoning text. The OpenAI adapter is unaffected (it has no signature concept). `request.go` already round-trips `ThinkingSignature` back out (`anthropic/request.go:110`), so once captured it flows correctly on the next turn.

## D4 — Authoritative cross-run history from the store

`serveChat` currently does `history := toHistory(req)` (text-only). We replace the source of truth:

- When the request carries a `threadId` that resolves to an existing session the caller owns → rebuild `[]provider.Message` from the `messages` table (`ORDER BY seq`), full blocks intact.
- The client-sent `req.Messages` are **ignored** for a persisted session (the server record is authoritative). This is also a security/consistency win: a client cannot rewrite its own past.
- Fallback (no session / unauthenticated / no store): keep `toHistory` text path so `serveChatDirect` and dev flows still work.
- The frontend needs **no change** to keep working: it already sends `threadId` in `body`; the extra `messages` it sends are simply no longer trusted. A later cleanup can slim the payload, but that is optional.

History rebuild and `serveHistory`/`serveResume` remain the render-facing reads; they now read from `messages` for content where richer than the event log (or continue reading events — see Edge Cases). The *loop input* is the part that must become full-fidelity.

## D5 — Port shape: `MessageStore` alongside `Store`

Add a `MessageStore` port in `internal/session` (append + list-by-session), with a PG implementation (`pgstore`) and an in-memory one for tests/dev. `RunRegistry`/`Runtime` gain the message-append on the run path; `chatapi` gains the history-rebuild read. Kept as a separate port (not folded into `Store`) so the event log and message log can evolve independently and tests can stub either.

## D6 — Large content: store pointers, materialize on the way out

A naive "store every block verbatim" breaks on large payloads: a multi-MB file read or a base64 image would make a single `messages.content` JSONB row explode, bloat PG TOAST, and force every history rebuild to haul megabytes back into memory. Claude Code's answer (confirmed in source) is that large content **never enters the message stream full-size** — it is truncated or spilled to a file, and the message holds a small reference (`BashTool/utils.ts:147` truncate; `BashTool.tsx:494,989` spill-to-file + path reference; images re-encoded + read back at send time `utils.ts:110-130`). We adopt the same principle, adapted to our B/S workspace model.

**Rule: the `messages` table always holds pointer-sized content. Full payloads live in the session workspace; each consumer materializes on demand.**

### Images: workspace path pointer, materialized per consumer

`provider.Block` gains `BlockImage` with `{ MediaType, ImagePath }` — a workspace-relative path, **never base64 in the DB**. The single full-size copy lives as a file in the session workspace (ties into the workspace-persistence capability; until S3/6.4 lands it is the local session workspace). Two consumers materialize it:

- **To the LLM — every turn, as base64.** A pre-send transform walks the request's messages and rewrites each `BlockImage{path}` into the provider-native image source (Anthropic `{type:"image", source:{data: <base64>, media_type}}`; confirmed `recover/claude-v2.1.165.js:38334`). This **must** happen every turn and be byte-stable: prompt caching keys on a byte-identical prefix, so the image cannot be swapped for a cheaper placeholder on later turns without invalidating the cache and re-billing the whole prefix. Repetition is therefore intentional, not waste.
- **WebP normalization.** On first ingest (when an image enters the workspace) it is decoded and re-encoded to **WebP** (`media_type: image/webp`) to cut payload size. Materialization then reads an already-WebP file. Dimension downscaling is a later refinement; WebP re-encode is the size lever we take now.
  - **Dependency:** `github.com/gen2brain/webp` — CGo-free (wasm2go-transpiled libwebp, optional purego dynamic fallback; `nodynamic` build tag disables the fallback). It handles only WebP: `webp.Encode(w io.Writer, m image.Image, o ...webp.Options) error` and `webp.Decode(r io.Reader, ...) (image.Image, error)`, with `webp.Options{Quality, Lossless, Method, Exact, AutoRotate}`.
  - **Ingest path:** source formats (PNG/JPEG/GIF) are decoded with the stdlib (`image/png`, `image/jpeg`, `image/gif`), then re-encoded via `webp.Encode` (sensible default e.g. `Options{Quality: 80}`). WebP inputs are stored as-is (no decode/re-encode). Decode failures fail ingest closed (reject the image) rather than storing an unverifiable blob.
- **To the web client — a reference, not the payload.** The SSE/history stream carries the image block as a reference; the frontend resolves it through a **dedicated authenticated file endpoint** (e.g. `GET /api/sessions/:id/files/<path>`), scoped to the session owner. This keeps SSE and history bodies small and lets the browser cache the image — far better than inlining multi-MB data-URIs into every stream frame in a B/S multi-tenant setting.

### Oversized text (file reads, command output): truncate, defer spill

Large `tool_result` text is **truncated before persistence** with an appended marker (`... [N bytes truncated] ...`), mirroring CC's `formatOutput`. The full text is **not** retained in this change: spilling to durable storage and letting the model re-read it depends on the workspace capability (tasks 6.3/6.4). Truncation keeps rows bounded and unblocks compression now; full-text spill is a follow-up once workspace persistence is in place.

### Path safety (multi-tenant hard requirement)

`image_path` is workspace-relative and **must** be resolved and confined to the owning session's workspace root at materialization time (reject `../` escapes, absolute paths, symlinks out). A stored path is a claim, not a capability — resolving it never crosses tenant/session boundaries. This applies to both the LLM transform and the frontend file endpoint.

## Edge cases

- **Signature absent (OpenAI / non-thinking models):** `ThinkingSignature` is empty; round-trip is a no-op. Fine.
- **Partial thinking at cancel:** a cancelled run may leave a thinking block without a signature. We persist what was assembled; on the next turn Anthropic may reject an unsigned trailing thinking block — handled by the 4.4 follow-up's send-time hygiene (`ensureToolResultPairing`-style), not here.
- **Old sessions have no `messages` rows:** history rebuild returns empty → treated as a fresh session; the run starts new rows. No data migration.
- **Ordering across a cancel/restart:** `seq` assigned from the durable max keeps appends monotonic even after a run settles mid-stream (mirrors the `AppendEvent` offset-continuation fix).
- **`serveHistory`/`serveResume` unchanged in this change:** they keep reading `run_events` (already sufficient for the UI). Switching them to read `messages` is a follow-up only if we want tool blocks rendered in history — not required for 4.4.
- **Image file deleted from workspace after being referenced:** materialization of a dangling path fails closed — the LLM transform substitutes a text placeholder (`[image unavailable: <path>]`) rather than erroring the whole request; the frontend endpoint 404s. A stored path outliving its file is tolerated.
- **Image every turn grows the request:** re-sending base64 each turn is required for prompt-cache stability (see D6), but it does count against the context window each turn. Managing image token budget (e.g. aging very old images to a placeholder when cache is already cold) is deferred to 4.4; this change only guarantees faithful storage + materialization.

## Testing

- PG integration tests (live `nowhere-pg`, skip when unreachable) for `MessageStore`: append, ordering by `seq`, full-block round-trip (thinking+signature, tool_use, tool_result).
- Loop-level test: a run with a tool call persists an assistant row with the tool_use block and a user row with the tool_result block.
- Adapter test: `signature_delta` lands in `ThinkingSignature`, not in `Thinking` text.
- Image tests: `BlockImage` stores `{media_type, image_path}` with no base64; LLM transform materializes path → base64 image source every turn (byte-stable); path traversal (`../`, absolute) rejected; WebP re-encode produces `image/webp`; frontend file endpoint serves the image to the owner and 403s/404s others.
- Truncation test: oversized `tool_result` text is truncated with the marker before persistence.
- chatapi test: cross-run history is rebuilt from the store with blocks intact; a forged `req.Messages` does not alter a persisted session's history.
- `go test ./...` green; openspec validate passes.
