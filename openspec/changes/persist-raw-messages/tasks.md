# Tasks: persist-raw-messages

## 1. Thinking-signature capture (prerequisite)
- [ ] 1.1 Add a signature channel to `provider.Event` (e.g. `SignatureDelta`); Anthropic decoder routes `signature_delta` there instead of into `Delta`
- [ ] 1.2 Loop accumulator assigns signature to `Block.ThinkingSignature` on finalize (leaves `Thinking` as pure text); OpenAI adapter unaffected
- [ ] 1.3 Tests: signature lands in `ThinkingSignature`, not concatenated into thinking text; round-trips via `anthropic/request.go`

## 2. MessageStore port + implementations
- [ ] 2.1 Define `MessageStore` port in `internal/session` (AppendMessage, MessagesFor(sessionID)) returning full-fidelity `[]provider.Message`
- [ ] 2.2 Migration `000006_messages` (up/down): `messages` table + `(session_id, seq)` index per design D1
- [ ] 2.3 PG implementation (`pgstore`) with per-session monotonic `seq` continuation after settle; in-memory impl for tests/dev
- [ ] 2.4 PG integration tests (live nowhere-pg, skip when unreachable): append, seq ordering, full-block round-trip

## 3. Persist full blocks from the run path
- [ ] 3.1 Persist the incoming user message as a row at run start
- [ ] 3.2 Persist each assembled assistant message (text/thinking(+sig)/tool_use) and each tool-result message (role=user, tool_result blocks) as they are appended in the loop
- [ ] 3.3 Wire MessageStore into the run-worker path (registry) so writes survive submitter disconnect; keep run_events writes unchanged
- [ ] 3.4 Tests: a run with a tool call persists an assistant row with tool_use and a user row with tool_result, full blocks intact

## 4. Image blocks: pointer storage + materialization
- [ ] 4.1 Add `provider.BlockImage` (`MediaType`, `ImagePath`) — workspace-relative path, never base64 in DB
- [ ] 4.2 WebP normalization helper (`github.com/gen2brain/webp`, CGo-free): decode PNG/JPEG/GIF via stdlib → `webp.Encode` (default `Options{Quality:80}`) on workspace write; WebP input stored as-is; decode failure rejects ingest
- [ ] 4.3 Pre-send LLM transform: rewrite each `BlockImage{path}` → provider-native base64 image source, every turn, byte-stable; confined to the session workspace root (reject `../`/absolute/symlink escapes); dangling path → text placeholder
- [ ] 4.4 Frontend file endpoint `GET /api/sessions/:id/files/<path>` (auth + owner-scoped) serving workspace images; SSE/history carries the image block as a reference
- [ ] 4.5 Tests: no base64 in DB; transform materializes every turn byte-stable; traversal rejected; WebP re-encode; endpoint serves owner / rejects others

## 5. Oversized text truncation
- [ ] 5.1 Truncate oversized `tool_result` text before persistence with an appended `... [N bytes truncated] ...` marker (size cap on the block)
- [ ] 5.2 Tests: oversized result truncated with marker; under-cap result stored verbatim

## 6. Authoritative cross-run history
- [ ] 6.1 `serveChat` rebuilds `[]provider.Message` from MessageStore for an owned `threadId`; ignores client-sent `req.Messages` for persisted sessions
- [ ] 6.2 Keep `toHistory` text fallback for no-session / unauthenticated / serveChatDirect
- [ ] 6.3 Tests: cross-run history rebuilt with blocks; a forged `req.Messages` cannot alter a persisted session's history

## 7. Verification
- [ ] 7.1 `go test ./...` green (incl. new store/loop/adapter/image/chatapi tests)
- [ ] 7.2 E2E vs mockllm: two sequential runs on one threadId → second run's request includes first run's full blocks (tool_use/tool_result/thinking) rebuilt from the store
- [ ] 7.3 `openspec validate persist-raw-messages` passes

