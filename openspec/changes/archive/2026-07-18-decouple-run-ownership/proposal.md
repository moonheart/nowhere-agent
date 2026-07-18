# Proposal: decouple-run-ownership

## Why

Today a run is **parasitic on the submitter's HTTP request**: `serveChat` derives
`runCtx` from `r.Context()`, registers that cancel func, and runs `loop.Run` on
the request goroutine. Every other client merely subscribes via
`Runtime.Subscribe`. This single design decision is the root cause of a cluster
of bugs we have been patching one at a time:

- **tab2 can't cancel** — only the submitter's `onCancel` reaches
  `/api/chat/cancel`; an attacher's Stop just aborts its local resume fetch and
  the run keeps going.
- **terminal event lost on attach** — `CancelRun` settles the run (releasing the
  active-run lock) before the loop's terminal `cancelled` event lands, so an
  attacher's replay-tail can see `!stillActive` and finish with `stop` instead
  of `cancelled`.
- **duplicate assistant message on attach** — the snapshot/follow split only
  exists because the submitter owns the live stream and everyone else replays.
- **submitter closing a tab kills the run** — the run context is a child of the
  submitter's request context, so a disconnect is indistinguishable from a
  cancel.

These are not independent bugs; they are the same ownership confusion surfacing
in four places. Fixing them individually leaves the model broken.

## What Changes

- Promote the run to a **first-class, connection-independent entity**. A run is
  executed by a **run worker** (a dedicated goroutine owned by a `RunRegistry`),
  not by the submitter's HTTP request goroutine.
- Introduce an **`EventBus` port** (per hexagonal convention): `Publish` /
  `Subscribe`. The built-in implementation is in-memory; the seam allows a
  Redis-backed bus for multi-instance fan-out later.
- **`serveChat` becomes "start run + attach"**: it enqueues the run to the
  registry, then streams by attaching to the bus — the exact same code path as
  `serveResume`. The submitter and every attacher are now symmetric consumers.
- **Cancel becomes uniform**: any client (submitter or attacher) calls
  `POST /api/chat/cancel`; `CancelRun` cancels the worker's context regardless
  of which HTTP connections are open. Terminal events are published to the bus
  before the run is settled, eliminating the attach-side race.
- The **durable run log (Postgres `run_events`) remains the single source of
  truth**; the bus is a live fan-out layer only. Reconnect/replay semantics are
  unchanged.
- Multi-instance support (Redis bus + cross-instance cancel routing via a
  control channel) is a **follow-up increment**, not part of this change's
  implementation; this change only fixes the ownership model and establishes the
  bus seam.

## Capabilities

### New Capabilities
<!-- None — this refactors the session-runtime capability's internals. -->

### Modified Capabilities
- `session-runtime`: run execution moves off the HTTP request into a registry-
  owned worker behind an EventBus; attach and cancel become transport-symmetric.

## Impact

- **Affected code**: `internal/session` (RunRegistry, EventBus interface + in-mem
  impl), `internal/chatapi` (serveChat/serveResume unified attach path, cancel),
  `web/src` (uniform Stop → cancel for attachers; the App.tsx polling/resume
  asymmetry simplifies).
- **Interfaces established**: `EventBus` (long-lived contract; Redis impl later).
- **Behaviour change (intentional)**: a submitter disconnecting no longer cancels
  its run. Session idle-end (task 16.4) becomes the mechanism that reaps
  abandoned runs.
- **Non-goals for this change**: multi-instance Redis bus, distributed cancel
  routing, run queueing/prioritisation, resource isolation pools.
- **Risk**: the unified attach path must keep replay-then-live gap-free (already
  proven in `serveResume`); the `persistEmitter` tee ordering (persist → publish)
  must be preserved so replay never misses an event that was published.
