# Proposal: redis-stream-live

## Why

Live token throughput to the web client is **gated by a synchronous Postgres write on the hot path**. Every streaming delta the loop produces goes through `Runtime.AppendEvent` (`internal/session/runtime.go:125`), which does `store.AppendEvent` (a PG `INSERT` into `run_events`) **before** `bus.Publish`. The subscriber (the web client's SSE stream) only receives a token after a full database round-trip. Measured effect: the upstream provider streams at ~116 tok/s while the web client renders ~10 tok/s — the bottleneck is our own persistence, not the model.

The same write path also produces **hundreds of redundant rows per run**: one `run_events` row per token/thinking fragment (`"99"`, `","`, `""`, …). Since `persist-raw-messages` landed, the authoritative full-block content already lives in the `messages` table — so these per-token fragments in `run_events` are pure duplication, written at the cost of the live stream's speed.

Root cause: **live delivery and durable persistence are fused into one synchronous write path.** They have different latency requirements and different consumers, and should be separate:

- **Live delivery** must be sub-millisecond fan-out to attached clients; it tolerates loss (a reconnecting client re-reads).
- **Persistence** must be durable and complete; it does not need to be on the per-token path — full content already lands in `messages` per assembled message.

A secondary, forward-looking motivation: the current `EventBus` is a single-process in-memory fan-out. When `chatapi` scales to multiple instances, a client connected to instance A must see tokens produced by a worker on instance B — which an in-memory bus cannot do. Redis Streams is the right primitive for a cross-instance, offset-resumable live channel, and its semantics (XADD/XREAD, TTL-based cleanup) map cleanly onto "a live run's temporary pipe that is discarded once the run settles."

## What

Split live fan-out from persistence behind a `StreamBroker` port. Content deltas leave the per-token DB path entirely; `run_events` is slimmed to lifecycle events only; live delivery goes through a broker with two implementations (in-memory for single-instance, Redis Streams for multi-instance). Reconnect/resume reads the live stream first (while it survives), falling back to the `messages` table once the stream is cleaned up.

## What Changes

- **`run_events` slimmed to lifecycle-only**: the loop's per-token `text`/`thinking` deltas and tool frames are no longer `INSERT`ed into `run_events`. Only lifecycle events (`running`, `done`, `error`, `cancelled`) are persisted there. Full content continues to land in the `messages` table (unchanged, one row per assembled message). A run that previously wrote ~350 fragment rows now writes ~3 lifecycle rows.
- **New `StreamBroker` port** (`internal/session`): offset-based `Publish`/`Read` with Redis-Streams-ready semantics, plus run-end cleanup (`Trim`/`Expire`). Defined so the web client's consumption path is identical whether the backend is memory or Redis — single→multi-instance is a config swap, not a code change.
- **`MemBroker`**: single-instance in-memory fan-out with a per-session ring buffer (so a reconnecting client can re-read recent deltas without a DB). No database in the live path.
- **`RedisBroker`**: multi-instance implementation over `go-redis` — `XADD` to publish, `XREAD` to consume, short TTL applied when the run settles so the stream is discarded once its live purpose ends. New config `STREAM_BROKER=mem|redis` + `REDIS_ADDR`.
- **`registryEmitter` reroutes content deltas** to the `StreamBroker` live path (sub-millisecond, non-blocking) instead of `run_events`; lifecycle events still go to `run_events` for durability.
- **Reconnect/resume reads stream-then-messages**: `attach`/`serveResume` consume the live stream while the run is active and, for a reconnecting client, prefer any surviving stream tail for in-flight partial output; once the stream is gone (settled + TTL elapsed) the authoritative content comes from the `messages` table.
- **Dreaming reads `messages`, not `run_events`**: with `run_events` slimmed to lifecycle-only, the episodes the dreaming worker consumes now come from the full-block `messages` table (a strictly richer, already-durable source). The episode contract moves from "fold the run event log" to "read the session's assembled messages".

Concretely, the new hot path is:

```
LLM delta → loop → registryEmitter ──► StreamBroker.Publish (no DB)
                                          │  XADD / chan fan-out
                                          ▼
                                     web client SSE  (~provider speed)

(assembled message) ─────────────────► messages table (PG, off hot path)
(lifecycle event) ──────────────────► run_events (PG, ~3 rows/run)
```

## Non-goals

- No change to the `messages` table schema or what gets persisted there (settled by persist-raw-messages).
- No change to the SSE wire format or the frontend; this is purely a backend delivery/persistence re-plumb. The client sees the same frames, faster.
- No cross-instance run *execution* placement/sticky routing (a run still executes on the instance that accepted it); this change only makes the live *stream* visible across instances.
- No retention/analytics use of `run_events` content (it never had one — the fragments were write-only).
- No migration of existing `run_events` fragment rows (old rows are simply no longer produced; they can be reclaimed independently).

## Capabilities

- `session-runtime`: live fan-out split from persistence behind a `StreamBroker` port; `run_events` slimmed to lifecycle; reconnect reads stream-then-messages.
- `agent-loop`: content deltas emitted to the live broker rather than the durable per-token log (no behavioural change to the loop itself, only its emitter sink).
- `dreaming`: episodes now sourced from the full-block `messages` table rather than the run event log.
- `observability`: raw LLM request/response recording (already landed) plus the throughput relationship between provider and client becomes observable once persistence is off the hot path.
