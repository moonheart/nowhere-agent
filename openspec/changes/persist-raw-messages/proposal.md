# Proposal: persist-raw-messages

## Why

Today the durable record of a conversation **loses information at three points**, so anything downstream that needs the real conversation — online context compression (task 4.4), offline dreaming, replay, future summarization — is starved of input:

1. **`toHistory` flattens the client-sent messages to plain text** (`internal/chatapi/request.go:32`): thinking blocks and tool_use/tool_result blocks are dropped before the loop ever sees them.
2. **The loop emits tool calls/results only as transient SSE frames** (`internal/agent/loop.go:134-141, 216`): they are never reassembled into the assistant/user messages they belong to, so the produced side of a run is not persisted as messages at all.
3. **`run_events.payload` is a flat event log, not a message list**: there is no notion of "this is one complete assistant `Message` with its blocks". Cross-run history can therefore only be rebuilt as text, and the compressor (when wired for 4.4) would read a crippled history.

Claude Code's compression quality rests on one foundation: its transcript stores **complete messages with full blocks** (thinking incl. signature, tool_use, tool_result), and every consumer (compact / resume / fork) reads that faithful record. We currently have no equivalent.

A secondary defect this exposes: the Anthropic adapter folds the thinking `signature_delta` into the ordinary delta stream (`internal/provider/anthropic/stream.go:124` via `deltaText`), and the loop's `accumulator.append` appends every delta into the thinking text (`internal/agent/loop.go:256-263`). The signature is therefore **merged into the thinking text and never written back to `Block.ThinkingSignature`** — so even if we stored signatures today, we could not capture a clean one. Persisting raw messages forces us to fix this properly.

## What

Adopt the decision already made: **store every conversation message in its original, full-block form, in a dedicated message table, and rebuild cross-run history from that store (authoritative), ignoring client-sent history.**

Concretely:

- **New `messages` table** (migration `000006`): one row per canonical `provider.Message` — `id`, `session_id`, `run_id`, `seq` (monotonic within session), `role`, `content jsonb` (the full `[]Block`: text, thinking **with signature**, tool_use, tool_result), `created_at`. Indexed by `(session_id, seq)`.
- **Persist full blocks**: the loop's assembled assistant messages and the tool-result messages are written as complete `Message` rows (thinking signature included), alongside the existing event stream. `run_events` stays as the lifecycle/attach/fan-out log; `messages` becomes the content source of truth.
- **Authoritative cross-run history**: `serveChat` rebuilds `[]provider.Message` for a `threadId` from the `messages` table instead of trusting `toHistory(req.Messages)`. The client keeps sending its history (protocol-compatible); the server ignores it when a session exists. Unauthenticated / no-session paths keep the text fallback.
- **Fix signature capture**: surface the thinking signature as a distinct block-level value so it round-trips (`ThinkingSignature`) instead of being concatenated into thinking text.
- **Image blocks stored as workspace pointers, materialized on demand**: a new `BlockImage` carries a workspace-relative path (never base64 in the DB). On send to the LLM the path is materialized to a base64 data block **every turn** (prompt-cache stability requires the byte-identical prefix); images are normalized to **WebP** to shrink payload. On send to the web client the block carries a reference the frontend resolves via a **dedicated authenticated file endpoint** (keeps SSE/history bodies small).
- **Oversized text tool results are truncated** before persistence (marker appended); full-text spill storage is deferred to the workspace capability.
- This is the **prerequisite for task 4.4** (context-window compression): the compressor will read this faithful, full-block history. Compression itself is a separate follow-up change and is out of scope here.

## Non-goals

- No online compression / compaction logic (that is task 4.4, a separate change).
- No change to `run_events` lifecycle semantics, attach, cancel, or fan-out (already settled by decouple-run-ownership).
- No replay/history **rendering** change for the web client beyond what falls out naturally (it already consumes history/resume; the backend now backs it with richer data).
- No migration of pre-existing `run_events` rows into `messages` (old sessions simply have no message rows; new runs populate the table).

## Capabilities

- `session-runtime`: persist conversation messages in original form; message store as content source of truth.
- `agent-loop`: capture and emit complete assembled messages (incl. tool blocks and thinking signature) for persistence.
- `provider-abstraction`: carry thinking signature as a distinct, round-trippable block value (not merged into text); new image block type materialized to provider-native base64 on send.
- `workspace-persistence`: image payloads live as files in the session workspace; the message store holds pointers to them.
- `context-management`: (enabling change) a faithful full-block history exists for the compressor to consume — no behaviour change yet.
