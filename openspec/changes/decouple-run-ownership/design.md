# Design: decouple-run-ownership

## Context

A run's lifecycle is currently bound to the submitter's HTTP request
(`handler.go`): `runCtx, cancelRun := context.WithCancel(r.Context())`, the
cancel func is registered into the session, and `loop.Run` executes on that
request goroutine. Attached clients are read-only subscribers fanned out from
`Runtime.AppendEvent`. This makes the submitter special: it alone can cancel,
its disconnect kills the run, and its stream is privileged (direct, lossless)
while attachers get a drop-prone fan-out plus replay.

This design replaces that with a symmetric model: **the run is owned by a
registry, every client attaches through the same bus, and the durable log is the
only source of truth.**

## Goals / Non-Goals

**Goals**
- Run executes independently of any HTTP connection.
- One attach code path shared by submitter and reconnector.
- Uniform cancel from any attached client.
- Terminal state provably delivered to all attached clients (no settle-before-
  terminal-event race).
- An `EventBus` seam that a Redis implementation can satisfy later without
  touching session/chat logic.

**Non-Goals**
- Multi-instance deployment, Redis bus, cross-instance routing (follow-up).
- Run queueing, priority, or resource pools (workers are unbounded goroutines
  for now; admission control stays the single-active-run lock).
- Changing the durable log schema or replay semantics.

## Decisions

### D1. Run ownership moves to a `RunRegistry` (goroutine-per-run, not a fixed pool)

`SubmitRun` spawns a dedicated goroutine that owns the run's `context` and calls
`loop.Run`. The registry maps `sessionID → *runHandle{ctx, cancel, runID, done}`.

*Why goroutine-per-run over a fixed N-worker pool*: chat runs are long (minutes);
a small fixed pool would head-of-line block the (N+1)th run. We want the
"worker" semantics (unified schedule/cancel/lifecycle) without an artificial
concurrency ceiling. A semaphore can cap global concurrency later; resource
isolation (sandbox CPU/mem quota, tasks 14.3/16.x) is where pooling will
actually matter. The registry is the seam for both.

*Alternative considered*: keep the run on the request goroutine and only fix the
cancel/attach asymmetries. Rejected — it leaves "submitter disconnect kills run"
and the privileged/attacher stream split in place, which is the core defect.

### D2. `EventBus` is a port; in-memory impl now, Redis later

```go
type EventBus interface {
    Publish(sessionID string, e Event)
    Subscribe(sessionID string, buffer int) (<-chan Event, func())
}
```

The in-memory implementation is essentially today's `Runtime.Subscribe` fan-out,
extracted behind the interface. The Redis implementation (Pub/Sub for live
fan-out, PG replay as the gap-filler) is a drop-in for the multi-instance
increment — the session and chatapi layers never change.

*Why Pub/Sub and not Redis Streams*: attach is broadcast (every client needs
every event). PG `run_events` is already the replayable, ordered log; a Redis
Stream would duplicate that persistence and force log trimming. Streams/consumer
groups are the right tool for *competing consumers* (e.g. the dreaming worker
pool), a different link.

### D3. One attach path; the submitter is just the first attacher

`serveChat` = `registry.Submit` + `attach(w, sessionID, after=0)`.
`serveResume` = `attach(w, sessionID, after=N)`.
The `attach` helper: subscribe to the bus first, replay the durable gap from
`after`, then live-follow — re-checking run state after each event so a run that
settles in the subscribe/replay window still terminates cleanly. This is exactly
the current `serveResume` flow, promoted to serve both endpoints.

The public HTTP/SSE contract is unchanged: `/api/chat` still returns the same
ui-message-stream frames.

### D4. Cancel is uniform and transport-independent

`CancelRun` (called by any client's Stop) invokes the registry-held cancel func.
Because the worker owns `runCtx`, cancellation no longer depends on which HTTP
connections are open. The frontend's attacher Stop path is changed to call
`POST /api/chat/cancel` exactly like the submitter's — the `onCancel` vs
`resumeRun` asymmetry disappears.

### D5. Terminal event is published before settle

The worker publishes the terminal lifecycle event (`done`/`failed`/`cancelled`)
to the bus **and persists it** before calling `CompleteRun`. Ordering:
persist → publish → settle. Attachers therefore always observe the terminal
event (live or via the replay-tail) before the run reads as settled, removing
the race where an attacher finishes with `stop` instead of `cancelled`.

### D6. Bus is lossy live; the durable log fills gaps

All clients now read through the bus, including the submitter, so any client can
drop a live delta under burst (slow consumer). The attach path must tolerate
this: track the highest offset seen, and on the terminal event do a final
`Replay(after=lastSeen)` to fill any gap before finishing. (Today only the
reconnect path does this; it becomes universal.) Persistence ordering (D5)
guarantees the gap is always fillable.

### D7. Submitter disconnect no longer cancels; idle-end reaps abandoned runs

With `runCtx` no longer derived from `r.Context()`, closing the submitting tab
leaves the run running (the intentional behaviour change). Session idle-end
(task 16.4) becomes the reaper for runs no client is watching. For this change
we take the conservative option: no new auto-cancel on disconnect; a generous
idle window is left to 16.4.

## Multi-instance preview (follow-up increment, not implemented here)

With the bus seam in place, multi-instance needs no run-affinity routing table:

- **Attach anywhere**: an attach request landing on instance B subscribes to the
  session's Redis channel and replays from PG — it never needs to know the run
  lives on instance A.
- **Cancel anywhere**: B publishes `{sessionID, runID, op:cancel}` to a control
  channel every instance's registry subscribes to; A recognises its own runID
  and cancels locally. Durability backstop: B first sets a `cancel_requested`
  flag on the run row in PG so a dropped Pub/Sub message is still honoured on
  the worker's next check.

## Risks / Trade-offs

- **Slower submitter stream** (now bus-mediated): negligible; same process, and
  the lossy-live + replay-fill (D6) is already proven.
- **Registry/bus duplication of Subscribe logic**: acceptable; the in-mem bus is
  a thin extraction and the old path is deleted.
- **Behaviour change on disconnect**: must be called out to users; mitigated by
  idle-end reaping (16.4).

## Migration Plan

1. Add `EventBus` + in-mem impl and `RunRegistry` in `internal/session` (with
   unit tests), no behaviour change yet.
2. Rewire `chatapi` serveChat/serveResume/cancel onto the registry + bus
   (adapter change); keep HTTP/SSE contract identical.
3. Update the frontend attacher Stop to call cancel; remove the now-unneeded
   polling/resume special-casing where symmetric attach covers it.
4. Regression: rerun the attach/cancel/multi-tab e2e against mockllm.

Rollback: the change is confined to `internal/session` + `internal/chatapi` +
`web`; revert the rewiring commit to restore request-bound runs.
