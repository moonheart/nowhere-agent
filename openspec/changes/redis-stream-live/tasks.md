# Tasks: redis-stream-live

## 1. StreamBroker port + MemBroker
- [x] 1.1 Define `StreamEvent` + `StreamBroker` port in `internal/session` (offset-based Publish/Read/Settle per design D2)
- [x] 1.2 `MemBroker`: per-session bounded ring buffer keyed by offset + subscriber fan-out; `Read(after)` serves reconnect catch-up; `Settle` resets the buffer
- [x] 1.3 Tests: publish/read offsets, reconnect catch-up via Read(after), slow-consumer drop (no run block), Settle clears buffer

## 2. RedisBroker
- [x] 2.1 Add `github.com/redis/go-redis/v9`; `RedisBroker` XADD publish / XREAD consume, per-session key `session:{id}:stream`
- [x] 2.2 `Settle` applies short TTL (EXPIRE) so the stream self-destructs after the run ends; `MAXLEN ~` cap on publish to bound growth
- [x] 2.3 Tests (miniredis): publish/read round-trip, offsets, TTL applied on Settle, reconnect Read(after), stream cap, session isolation

## 3. Slim run_events to lifecycle-only
- [x] 3.1 `Runtime.AppendEvent` routes content frames (text/thinking/tool_use/tool_result) to `StreamBroker.Publish`; lifecycle (running/done/error/cancelled/user) stays on `run_events`; `message` still → MessageStore
- [x] 3.2 Confirm no per-token `run_events` INSERT on the hot path (verified live: a 599-delta run wrote 3 lifecycle rows)
- [x] 3.3 Update/trim tests that asserted content replay from run_events

## 4. Reconnect reads stream-then-messages
- [x] 4.1 `attach`/`serveResume`: live-follow from `StreamBroker` (dual-channel: broker content + bus lifecycle); reconnecting client does `Read(after=lastSeen)` for the in-flight tail; durable lifecycle replayed so a late attacher still sees `running`
- [x] 4.2 Settled run → content from `messages` table via serveHistory; settled resume no longer re-streams content
- [x] 4.3 Tests: reconnect mid-run recovers deltas from the broker; settled resume terminates without re-streaming; cancel broadcast reaches attached clients

## 5. Dreaming episodes read messages
- [x] 5.1 `dreaming.EpisodeSource.Episodes` returns `[]session.StoredMessage` (full blocks) instead of `[]session.Event`; extract builds the prompt from message blocks (text/thinking/tool_use/tool_result)
- [x] 5.2 Tests: worker consolidates episodes from message blocks (durable PG EpisodeSource + `dreamed_at` marker deferred to the dreaming-worker wiring task)

## 6. Wiring + verification
- [x] 6.1 Config `STREAM_BROKER=mem|redis`, `REDIS_ADDR`; `cmd/server` builds + injects the broker; `redis` unreachable at boot fails fast (PingRedis)
- [x] 6.2 `go test ./...` green
- [x] 6.3 E2E vs real LLM: live SSE not gated by PG (~267 delta/s, up from ~10); run_events lifecycle-only; messages table complete
- [x] 6.4 `openspec validate redis-stream-live` passes
