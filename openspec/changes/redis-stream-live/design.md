# Design: redis-stream-live

## D1 — Separate live delivery from persistence; they have different latency budgets

The defect is structural: `Runtime.AppendEvent` does `store.AppendEvent` (PG INSERT) **then** `bus.Publish`, so the live stream inherits a database round-trip per token. Live delivery and durable persistence are different concerns with different SLAs:

| | live delivery | persistence |
|---|---|---|
| latency budget | sub-millisecond | best-effort, off hot path |
| loss tolerance | yes (reconnect re-reads) | no |
| consumer | attached SSE clients | history rebuild, compression, dreaming |
| volume | every token | one row per assembled message |

So we split them. Content deltas go to a live broker with **no database on the path**; the assembled message still lands in `messages` (persist-raw-messages, unchanged); lifecycle events still land in `run_events` (durable, but only ~3 per run so a synchronous write is fine).

The ordering guarantee we gave up and why it's safe: today "persist then publish" means a replayed client never sees a gap. After the split, a live event is not independently durable — but it doesn't need to be, because (a) the *content* is durable in `messages` once assembled, and (b) the broker retains recent deltas for reconnect re-read (D4). The durability boundary moves from "every token" to "every message", which is the correct granularity.

## D2 — `StreamBroker` port with Redis-Streams semantics, two implementations

We define the port with Redis Streams semantics up front (not a Go-chan-shaped interface), so the **web client's consumption path is identical for Mem and Redis** — single→multi-instance becomes a config swap with no client or handler change.

```go
// StreamEvent is one live frame: a content delta or tool frame for an active run.
type StreamEvent struct {
    Offset  int64  // monotonic within the session's live stream
    RunID   string
    Kind    string // "text" | "thinking" | "tool_use" | "tool_result" | ...
    Payload []byte // encoded frame, same shape the SSE emitter renders
}

type StreamBroker interface {
    // Publish appends a live frame and returns its offset. Non-blocking-ish:
    // must not wait on a database. Slow consumers are dropped, not the run.
    Publish(ctx, sessionID string, ev StreamEvent) (int64, error)
    // Read returns frames after the given offset (for reconnect catch-up) and/or
    // blocks for new frames (live follow). offset 0 = from the start of the buffer.
    Read(ctx, sessionID string, after int64) ([]StreamEvent, error)
    // Settle marks the run's stream finished and schedules cleanup (TTL on Redis,
    // buffer reset on Mem). Called once when the run reaches a terminal status.
    Settle(ctx, sessionID string) error
}
```

Why offset-based `Read(after)` rather than the current push-only `Subscribe`: a reconnecting client must re-read the deltas it missed while disconnected. Redis `XREAD` gives this natively via stream IDs; the Mem broker keeps a small ring buffer keyed by offset to serve the same `Read(after)` contract. Push (`Subscribe`) is layered on top of `Read` by the handler's live-follow loop, unchanged in shape from today.

- **`MemBroker`** — per-session ring buffer (bounded, e.g. last N frames) + subscriber fan-out. No DB. This is what runs in single-instance and restores full throughput immediately.
- **`RedisBroker`** — `XADD session:{id}:stream` on publish, `XREAD ... BLOCK` to consume, and on `Settle` an `EXPIRE` (short TTL, e.g. 60s) so the stream self-destructs once its live purpose ends. Key is per-session so a reconnecting client on any instance reads the same stream.

## D3 — run_events slims to lifecycle-only; registryEmitter reroutes content

`registryEmitter.Emit` currently sends every loop event through `Runtime.AppendEvent` → PG. After this change it routes by kind:

- **content frames** (`text`, `thinking`, `tool_use`, `tool_result`) → `StreamBroker.Publish` (no DB).
- **lifecycle** (`running`, `done`, `error`, `cancelled`) → `run_events` via `AppendEvent` (durable, ~3 rows/run, synchronous write acceptable).
- **`message`** (assembled message) → `MessageStore` (unchanged from persist-raw-messages).

`run_events` keeps its existing role for **run state** (active-run lock, settle detection, cross-tab "session is running" sync) but stops being a content log. The 350-row fragment problem disappears because those rows are simply never written.

One subtlety: `attach`'s settle detection and gap-fill today rely on `run_events` offsets. With content no longer there, the offset domain for the live path becomes the **broker's** offsets, and the run-events offsets only cover lifecycle. The handler's replay/fill logic is rewired to read content from the broker and lifecycle from run_events (D4).

## D4 — Reconnect reads stream-then-messages

A client attaches in one of three states; each has a clear source:

1. **Run active, client live** → follow `StreamBroker` (Mem ring / Redis XREAD). Fast path.
2. **Run active, client reconnecting after a gap** → `Read(after=lastSeen)` on the broker to recover the missed deltas from the ring/stream tail, then live-follow. (This replaces today's `Replay` from run_events for content.)
3. **Run settled** (stream TTL elapsed or buffer reset) → authoritative content from the `messages` table via the existing history rebuild; the client renders the final assembled messages.

The "stream-then-messages" rule: while the stream survives it is the low-latency source for in-flight partial output; once it is gone the `messages` table is the fallback that always has the final content. There is no state in which content is unrecoverable — the boundary case (reconnect exactly as the stream expires) resolves to `messages`, which is complete by then.

## D5 — Config + dependency footprint

- `STREAM_BROKER=mem|redis` (default `mem`), `REDIS_ADDR` (default `localhost:6379`), optional `REDIS_PASSWORD`/`REDIS_DB`. Selecting `redis` introduces the only new runtime dependency.
- Library: `github.com/redis/go-redis/v9` (mature, supports XADD/XREAD/XEXPIRE, context-aware). MemBroker adds no dependency.
- `cmd/server` builds the broker from config and injects it into the `RunRegistry`/`Handler`. When `redis` is selected but unreachable at startup, we fail fast at boot (a multi-instance deploy with a dead broker is a misconfiguration worth surfacing) — not silently downgrade, which would hide split-brain fan-out.
- Backpressure: `Publish` to a full Mem ring drops for the slowest subscriber (same contract as today's memBus); Redis `XADD` is bounded by `MAXLEN ~` trimming so an abandoned stream can't grow unbounded.

## D6 — Dreaming episodes move to the messages table

`run_events` previously doubled as the dreaming worker's episode source ("Persisted runs as episodes"). Once it slims to lifecycle-only, episodes come from the **`messages` table** — a strictly richer source (full blocks: thinking, tool_use, tool_result) that is already durable and already the conversation's authoritative record. The episode query changes from "fold a session's run_events into a transcript" to "read the session's `messages` in `seq` order", which is simpler and lossless. Dreaming's eligibility signal is unchanged (session ended); only the content source moves.

## D7 — What we deliberately did NOT do

- No per-token durability: content durability boundary is the assembled message (D1). This is the trade that buys the throughput.
- No change to SSE wire format or frontend frames.
- No run-placement/sticky routing (out of scope; only the stream crosses instances).
- No retroactive cleanup of historical `run_events` fragment rows (they're just no longer produced; a separate ops task can `DELETE`/partition them).
