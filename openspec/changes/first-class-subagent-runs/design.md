# first-class-subagent-runs — design

## Context

`SpawnTool.Call` (`internal/subagent/tool.go`) runs a child `agent.Loop` inside the
parent's tool dispatch and collapses its output into a tool result. Nothing about
the child is durable: no row, no id, no status — the only trace is the parent's
`messages`/`Nested` blocks. The session runtime (`internal/session`) already knows
how to persist run-scoped facts (runs table, usage via `KindUsage`), and the
recently-added `UsageScope` gives us per-child usage at the spawn site. Parent
resume re-runs the model from durable messages, so a previously-issued
`spawn_agent` tool call can be re-planned and re-executed; today nothing connects
the re-issue to the earlier child. Next free migration: **000028** (after
persist-agent-defs' 000027; if that change hasn't landed, this one takes 000027).

## Goals / Non-Goals

**Goals:**
- Durable, queryable record per spawn with lifecycle, outcome code, usage.
- Structured outcome codes on every spawn result (success and all failure modes).
- Idempotent replay of completed spawns on parent resume (kill duplicated child cost).
- Session-scoped read API for inspection; panel can drill into a child post-hoc.
- Bound activity frame rate; per-type fan-out caps.

**Non-Goals:**
- Suspending/resuming a gated CHILD mid-loop (LangGraph-style child checkpoints):
  gated children stop with outcome `gated`; resuming the child itself is future work.
- Attaching to a live child's event stream independently of the parent run stream.
- Cross-session subagent analytics/reporting.

## Decisions

### D1: A `subagent_runs` table owned by a Recorder port
Table (migration 000028): `id uuid pk`, `session_id`, `parent_run_id`,
`spawn_call_id` (the spawn_agent tool-call id), `agent_type`, `depth`,
`status`, `outcome`, `prompt`, `result_content`, `result_nested jsonb`,
`usage_*` ints, `error`, `started_at`, `finished_at`. Unique index on
`(session_id, spawn_call_id)` — the replay lookup key. `internal/subagent`
gains a `Recorder` interface (`Start`, `Finish`) with a PG implementation and a
no-op default; `main.go` wires the PG one, tests use fakes. Rationale: the
subagent package already takes its dependencies as injected interfaces
(`LoopFactory`, `Sink`); a port keeps that shape and honors "no unscoped writes"
conventions. Alternative — write through `session.MessageStore`/`run_events`:
rejected, `run_events` is deprecated and messages are conversation content, not
run telemetry.

### D2: Outcome codes ride the tool result, not a new wire type
`toolruntime.Result` gains no field; the code is emitted as a machine-readable
first line of `Content` on ERROR results (`outcome: budget_exhausted`) and
recorded on the subagent_runs row; success keeps clean content (code
`completed` lives on the record only). Rationale: the model already reads
`Content`; clients that want the structured code read the records API. A new
Result field would ripple through provider adapters for zero model benefit.

### D3: Replay by (session, spawn_call_id) match, verified by prompt+type
`SpawnTool.Call` already receives the tool-call id via
`toolruntime.CallIDFrom(ctx)`. Before starting a child, it asks the Recorder
for a completed record with this session+call id; hit with matching
prompt/type → return the stored collapsed result (no loop, no budget charge).
Miss, non-completed outcome, or mismatch → run fresh. Session id comes from
`agent.SessionIDFromContext(ctx)`. Rationale: the tool-call id is the natural
idempotency key — the resumed parent re-issues the same assistant tool_use
block. Alternative — content-hash dedup across different call ids: rejected,
two genuinely separate spawns may share a prompt.

### D4: Throttle at the activityEmitter, per child
`activityEmitter` buffers `stream` deltas and flushes on a timer
(`SUBAGENT_ACTIVITY_FLUSH_MS`, default ~100ms) or on any non-stream signal; the
buffer is per child (the emitter already is), so no cross-child coupling. The
final flush is forced by the next lifecycle signal, so `done` never precedes
buffered text. Rationale: coalescing at the emitter is the single choke point —
broker, runtime, and UI need no changes. Alternative — broker-level rate
limiting: rejected, it can't merge deltas without understanding their shape.

### D5: Per-type budgets as a map on SpawnTool, same mechanics as global
Config adds `SUBAGENT_TYPE_BUDGETS` (e.g. `researcher=4/2,general-purpose=16/8`).
`SpawnTool` keeps `map[agentType]{sem, count}` consulted after the global
checks; the per-type semaphore waits exactly like the global one (select on
ctx.Done → `cancelled`). Rationale: reuses proven mechanics and error wording;
a type without an entry skips straight to global.

### D6: Read API next to chatapi, session-scoped
`GET /api/sessions/{id}/subagent-runs[/{subID}]` in a small handler (or folded
into chatapi's session routes), behind `RequireAuth`, ownership check identical
to history/attach (own session or platform admin). The run panel fetches on
demand when a subagent card is expanded after the fact; live frames stay the
primary path mid-run.

## Risks / Trade-offs

- [Replay returns stale results when the world changed between runs] → Documented semantics: replay only for `completed` records with exact prompt+type match; anything else re-runs.
- [Recorder write contention under wide fan-out] → Writes are small, one per lifecycle transition; best-effort policy means a slow DB degrades records, not spawns.
- [Throttle adds up-to-one-window latency to child text] → Window is small (100ms default) and lifecycle signals bypass it; configurable.
- [Outcome-code-in-content is a convention, not a type] → First-line marker is greppable and documented in the spec; clients wanting structure use the records API.
- [Migration numbering collision with persist-agent-defs] → Coordinate at apply time; this change takes the next free number.

## Migration Plan

1. Apply the `subagent_runs` migration (down drops the table).
2. Deploy server (recorder wired, replay + codes + throttle + type budgets live).
3. Deploy web (panel drill-down). Rollback: revert binaries; the table is additive and can stay.

## Open Questions

- Whether the inspection endpoints live in chatapi or a new `subagentapi` package — decide at apply by route-count; default to a small new handler to keep chatapi focused.
- Whether per-type budgets should also be settable per agent definition (frontmatter) later — plausible follow-up, deliberately out of scope here.
