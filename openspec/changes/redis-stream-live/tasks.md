# Tasks: redis-stream-live

## 1. StreamBroker port + MemBroker
- [ ] 1.1 Define `StreamEvent` + `StreamBroker` port in `internal/session` (offset-based Publish/Read/Settle per design D2)
- [ ] 1.2 `MemBroker`: per-session bounded ring buffer keyed by offset + subscriber fan-out; `Read(after)` serves reconnect catch-up; `Settle` resets the buffer
- [ ] 1.3 Tests: publish/read offsets, reconnect catch-up via Read(after), slow-consumer drop (no run block), Settle clears buffer

## 2. RedisBroker
- [ ] 2.1 Add `github.com/redis/go-redis/v9`; `RedisBroker` XADD publish / XREAD consume, per-session key `session:{id}:stream`
- [ ] 2.2 `Settle` applies short TTL (EXPIRE) so the stream self-destructs after the run ends; `MAXLEN ~` cap on publish to bound growth
- [ ] 2.3 Tests (miniredis or live redis, skip when unreachable): publish/read round-trip, offsets, TTL applied on Settle, reconnect Read(after)

## 3. Slim run_events to lifecycle-only
- [ ] 3.1 `registryEmitter` routes content frames (text/thinking/tool_use/tool_result) to `StreamBroker.Publish`; lifecycle (running/done/error/cancelled) stays on `run_events`; `message` still → MessageStore
- [ ] 3.2 Confirm no per-token `run_events` INSERT on the hot path (a run writes ~3 lifecycle rows)
- [ ] 3.3 Update/trim tests that asserted content replay from run_events

## 4. Reconnect reads stream-then-messages
- [ ] 4.1 `attach`/`serveResume`: live-follow from `StreamBroker`; reconnecting client does `Read(after=lastSeen)` for the in-flight tail before following
- [ ] 4.2 Settled run → content from `messages` table (existing history rebuild); boundary case (reconnect as stream expires) resolves to messages
- [ ] 4.3 Tests: reconnect mid-run recovers missed deltas from the broker; reconnect after settle renders final content from messages

## 5. Dreaming episodes read messages
- [ ] 5.1 Dreaming worker's episode source switches from `run_events` to the session's `messages` (seq order), per design D6
- [ ] 5.2 Tests: an ended session's episodes are read from messages with full blocks

## 6. Wiring + verification
- [ ] 6.1 Config `STREAM_BROKER=mem|redis`, `REDIS_ADDR`; `cmd/server` builds + injects the broker; `redis` unreachable at boot fails fast
- [ ] 6.2 `go test ./...` green
- [ ] 6.3 E2E vs mockllm: live SSE no longer waits on PG (throughput not gated by per-token INSERT); run_events holds lifecycle-only; messages table still complete
- [ ] 6.4 `openspec validate redis-stream-live` passes
