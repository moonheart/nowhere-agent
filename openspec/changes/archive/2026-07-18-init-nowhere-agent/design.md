# Design: init-nowhere-agent

## Context

Greenfield multi-user AI agent platform. B/S architecture: browser frontend over WebSocket/SSE to a Go backend. The backend owns all agent logic. The four load-bearing decisions are already locked (see Decisions). This document fixes the architecture and, most importantly, the **interface boundaries** that let external systems plug in later without re-architecture.

## Goals / Non-Goals

**Goals**
- Self-built Go agent loop with full control over orchestration, streaming, context.
- Canonical provider model that preserves advanced per-provider features (thinking, prompt caching).
- Per-session sandbox isolation, workspace persistence across sessions.
- Long+short-term memory with offline consolidation ("dreaming").
- General-form skills with progressive disclosure and scoped sharing.
- Clean seams (ports) for future sandbox & memory backends.

**Non-Goals**
- Integrating real gVisor/Firecracker or Mem0/Zep now (interfaces only).
- Rich frontend; production infra; multi-region.

## High-Level Architecture

```
   Browser ──WS/SSE──┐
                     ▼
   ┌────────────────────────────────────────────────────┐
   │  Go Gateway (HTTP + WS)   multi-user / auth / team │
   │  ┌──────────────────────────────────────────────┐  │
   │  │ Agent Loop                                    │  │
   │  │  · canonical Message/Tool model               │  │
   │  │  · Provider adapters: Anthropic / OpenAI /…   │  │
   │  │  · streaming · prompt caching · thinking      │  │
   │  └──────────────────────────────────────────────┘  │
   │       │                │                  │        │
   │       ▼                ▼                  ▼        │
   │  ┌─────────┐     ┌──────────┐      ┌───────────┐   │
   │  │ Skill   │     │ Memory   │      │ Sandbox   │   │
   │  │ Engine  │     │ System   │      │ Manager   │   │
   │  └────┬────┘     └────┬─────┘      └────┬──────┘   │
   └───────┼───────────────┼───────────────┼──────────┘
           │               │               │
   ┌───────▼──────┐ ┌──────▼───────┐ ┌─────▼──────────┐
   │ SkillStore    │ │ MemoryPort   │ │ SandboxPort    │   ← seams
   │ sys/team/user │ │ read/write   │ │                │
   └───────────────┘ │  split       │ │ builtin:Docker │
                     └──────┬───────┘ │ [→gVisor/…]    │
                            │         └─────┬──────────┘
                            ▼               │ workspace volume
                     ┌──────────────┐       │ (materialize/solidify)
                     │ Dreaming      │       ▼
                     │ Worker (cron) │   Persistent store
                     │ extract/      │   (local dir → S3-compatible)
                     │ compress/     │
                     │ reorganize/   │
                     │ reflect       │
                     └──────────────┘
```

## Decisions

### D1 — Self-built Go agent loop
The loop is the soul of the product; we keep it in-house for control over memory injection, skill loading, tool dispatch, and streaming. Loop owns the context window; short-term memory IS the in-context conversation.

### D2 — Canonical provider model follows Anthropic's block shape
Do NOT model on OpenAI `chat.completions` (a string content + deltas), or we lose thinking blocks, prompt caching, and structured tool results. The canonical model:

```
Message { role: user|assistant, content: [Block] }
Block = Text | ToolUse | ToolResult | Thinking | CachePoint
```

- `content` is a **structured block array**, never a plain string.
- `Thinking` must round-trip (assistant thinking is sent back on subsequent turns).
- `CachePoint` marks cacheable prefixes — the cost lever (Anthropic caching saves ~90% input tokens).
- `ToolResult` may embed images/files, not just text.

**Streaming** is an **event stream** (`block_start / block_delta / block_stop / message_stop`), not cumulative deltas — required to interleave thinking + tool_use.

Each provider has an adapter translating canonical ↔ provider-native.

### D3 — Per-session sandbox behind `SandboxPort`
Isolation granularity is the **session**. Built-in = filesystem isolation + Docker container. Seam left for remote protocols (gVisor/Firecracker) — the interface hides "local Docker vs remote microVM".

Minimal verb set (what the AI can actually do):

```go
type SandboxPort interface {
    Create(ctx, SessionID, opts) (Handle, error)   // opts includes NetworkPolicy
    Destroy(ctx, Handle) error          // deferred stop on session end
    Exec(ctx, Handle, cmd) (Result, error)
    ReadFile / WriteFile / ListDir(...)  // file ops
    MaterializeWorkspace(ctx, Handle, WorkspaceRef) error  // pull in
    SolidifyWorkspace(ctx, Handle) (WorkspaceRef, error)   // push out
}

// NetworkPolicy is set at Create and enforced at the container layer via an
// egress proxy; this is what makes execution-permission's network gate real.
type NetworkPolicy struct {
    Mode         string   // open | allowlist | deny
    AllowedHosts []string // for allowlist mode
}
```

**Network egress is controlled at the container layer, not the Go layer.** Once `Exec` can run, code inside could call `curl` directly — Go-level checks cannot stop it. So `Create` applies a `NetworkPolicy` enforced by an egress proxy / firewall in front of the container. This gives D10's "gate network egress" an actual enforcement point.

### D4 — Workspace persistence: container stateless, volume external
Containers are **disposable compute**; the workspace is **external durable data**. On start the workspace is materialized into the container; on idle/end it is solidified back. The container is destroyed after a configurable delay; the workspace persists and is restored on reactivation (even after a long gap).

**Sync model (chosen), not bind-mount.** Abstract as push/pull so the persistent backend can be S3-compatible. Bind-mount ties us to mountable storage and breaks remote sandboxes. Built-in backend = local directory sync; future = S3-compatible (MinIO). Treat the workspace as a versionable data volume.

**Atomic solidify via two-stage commit.** Solidify writes the new snapshot to a staging version, validates completeness, then atomically repoints `current` to it. If the container is killed mid-solidify, `current` still points at the last complete version — the persistent store never holds a half-written workspace.

### D5 — Read/write-split memory behind `MemoryPort`
Short-term memory = in-loop context (NOT behind the port). Only **long-term** memory is abstracted, and it is asymmetric:

```go
type MemoryPort interface {
    // read side — online, called by loop, must be fast/cacheable
    Recall(ctx, query, scope) ([]Memory, error)

    // write side — offline, called ONLY by dreaming
    Store(ctx, Memory) error
    Forget(ctx, id) error                 // GDPR delete
    ListByScope(ctx, scope) ([]Memory, error)  // dreaming scan
}
```

Rationale: online is read-only (low latency, cacheable, non-blocking); all memory evolution is centralized in dreaming; swapping in Mem0/Zep later means swapping read-retrieval and write-extraction independently.

### D6 — Scheduled dreaming worker
Offline, cron-style, **configurable frequency**. Dreaming is the only writer to long-term memory. Its input is the **persisted run records (episodes)** written by the session runtime (D13): each run iteration is flushed to the database, and dreaming reads episodes for sessions that have ended. Pipeline over recent episodes:

1. **Extract** — episode → facts/preferences (semantic memory)
2. **Compress** — old episodes → summaries, raw discarded
3. **Reorganize** — conflict detection, update/deprecate stale memories
4. **Reflect** — cross-session patterns → insights

Dreaming consolidates memories into the shared scope model — it can produce **user-scoped and team-scoped** long-term memories from the episodes it processes.

Dreaming itself calls an LLM → needs **budget control**. Trigger: scheduled (not per-session-immediate), favoring cost efficiency.

### D7 — General-form skill system, progressive disclosure, three scopes
Skills = SKILL.md (prompt + process) + resources + scripts, like Claude Code skills. **Progressive disclosure:**

- **L0 (resident)**: name + one-line description of every skill (~50 tokens each)
- **L1 (on select)**: full SKILL.md body when the AI chooses it
- **L2 (on demand)**: referenced resources/scripts; **scripts execute in the sandbox**

Scopes: **system / team / user**, merged at load with priority override (user > team > system). Skill engine and sandbox are coupled via L2 script execution.

### D8 — One shared scope model
`identity-scope` defines user/team/system and is reused by both skills and memory for ownership, isolation, and access. Memory scope model mirrors skill scope.

### D9 — Unified tool runtime
All tools — built-in (file/command/web), skill L2 scripts, and future MCP tools — are exposed to the loop through one `Tool` interface; the loop never cares about a tool's origin.

```go
type Tool interface {
    Name() string
    Schema() JSONSchema        // delivered to the model as function-calling schema
    RiskLevel() Risk           // drives execution-permission
    Timeout() time.Duration
    Call(ctx, args) (Result, error)  // Result may embed stderr/files for model self-correction
}
```

Key points: schema is delivered in the provider's function-calling format (via adapter); errors (incl. stderr) are fed back to the model as tool-results so it can self-correct; concurrent tool calls within one turn are supported; MCP integration is a seam (a Tool adapter over a tool server), not built now.

### D10 — Two-layer permission: resource + execution
Permission is split into two distinct concerns:

- **Resource permission** (already `identity-scope`): who can see a skill/memory. Static, data-ownership.
- **Execution permission** (`execution-permission`): whether the agent may perform an action *right now*. Dynamic, per-action.

Execution policy decision: **permissive inside the sandbox; gate sandbox-escaping actions.** Because the sandbox already isolates execution, actions contained within it (run code, write to the session workspace) are allowed by default — avoiding confirmation fatigue. Only actions that reach outside the sandbox boundary require approval per a configurable policy:

- network egress
- writes outside the session workspace (e.g., shared/persistent areas)
- external API calls / cost-incurring operations

Approval policy is configurable per tool/risk-level (allow / ask / deny), keeping UX smooth while containing real risk at the sandbox boundary.

### D11 — Online context management, distinct from dreaming
Online compression of short-term context is its own concern, separated from offline dreaming:

| | context-management (online) | dreaming (offline) |
|---|---|---|
| Goal | keep the current session within context budget | consolidate long-term knowledge across sessions |
| Timing | real-time, when context nears the limit | scheduled, after sessions end |
| Input | current context window | finished episodes |
| Output | compressed context (conversation continues) | long-term memories (feed future sessions) |
| Loss tolerance | acceptable (drops transient detail) | must be faithful (becomes durable knowledge) |

Boundary: compression only decides "can we keep talking now"; dreaming decides "what is worth remembering later". Detail dropped by compression, if important, is recovered by dreaming offline — they do not conflict. Compression triggers at a configurable threshold (e.g., ~80% of the window) using a sliding-window summary strategy; compression may itself call an LLM.

### D12 — Observability as a first-class capability
The system is heavily async (loop, tools, dreaming, sandbox) and multi-tenant, so debugging and cost control require built-in observability rather than bolted-on logging.

- **Tracing**: every run emits a trace spanning model calls, tool executions, permission checks, and sandbox ops; sessions can be replayed step-by-step.
- **Cost metering**: every LLM call records tokens (in/out, cached) and computed cost, attributed to user AND team; this feeds quota (D14) and spend dashboards.
- **Structured logs + metrics**: per-component logs; dreaming emits run metrics (episodes processed, memories written, LLM spend vs budget).

### D13 — Session runtime: run lifecycle + resilient streaming
A "run" (one agent turn-chain) has an explicit state machine, decoupled from the transport, so a dropped browser connection never loses work.

```
queued → running ⇄ waiting_approval → done
                  ↘ failed / cancelled
```

- **Event-sourced streaming**: loop events are appended to a durable run log; the WS/SSE is a *subscription* to that log, not the source of truth.
- **Reconnect & replay**: a client that disconnects reconnects and replays from the last received event offset.
- **Stop/cancel**: a run can be cancelled; cancellation propagates to in-flight tools and sandbox exec.
- **Multi-attach**: multiple clients (tabs/devices) can attach to the same session and receive the same event stream.
- **Persisted runs are the episodes**: each run iteration is flushed to the database as it happens. A session has one or more runs; the durable run records ARE the episodes the dreaming worker consumes (see D6). There is no separate episode store.
- **Single active run / no multi-writer**: a session runs at most one run at a time. If one client starts a run (state → running), all other attached clients are synced to that state and are blocked from submitting a new run until it completes — preventing conflicting concurrent writes.

This D13 shapes the gateway: the gateway holds subscriptions; the session runtime holds state.

**Session lifecycle (vs run lifecycle):** a session is *active* while any client is attached or a run is in progress; it is considered *ended* after **N minutes (configurable) with no active run and no attached client**. Session-end is the single signal that triggers sandbox deferred-stop (D3), workspace solidify (D4), and makes the session's episodes eligible for dreaming (D6).

### D14 — Model routing & two-level quota, platform-held keys by default
Provider/model selection, credentials, and quota are centralized.

- **Credential resolution**: platform-held keys are the default; a team MAY configure its own key, which then takes precedence for that team's calls.
- **Routing policy**: choose provider+model per request based on configured policy, then check quota, then apply failover to an alternate provider/model on failure or rate-limit.
- **Two-level quota**: platform-level (backstop) and team-level (when the team has its own key/quota); usage is metered per D12 and enforced before dispatch.

```
  resolve key:  team key? ──yes──▶ team key, team quota
                     └────no────▶ platform key, platform/user quota
  then: pick provider+model (policy) → check quota → dispatch → on fail, failover
```

### D15 — Unified scheduler for all timers
All periodic/delayed work is driven by one scheduler component rather than ad-hoc timers scattered across subsystems:

- **dreaming** runs (cron, D6)
- **sandbox deferred destroy** (delay after session-end, D3/D13)
- **quota rollover** (period reset, D14)

The scheduler persists jobs, uses **UTC** for storage (display layer converts), and performs **catch-up**: after a restart it scans for jobs that should have fired while down and runs them. This prevents missed dreaming runs or leaked sandboxes after a deploy.

### D16 — Skill versioning
Skills are versioned. A user-scoped skill overriding a team/system one is pinned to a version; when the underlying team/system skill is updated, the override does NOT silently break — resolution records which version was overridden, and updates surface as a reviewable change. Version history is retained so a skill can be rolled back.

## Risks / Trade-offs

- **Provider drift**: adapters are ongoing maintenance as APIs evolve. Mitigation: canonical model is the single contract; adapters are thin.
- **Dreaming cost**: unbounded LLM spend. Mitigation: budget caps + configurable cadence.
- **Workspace sync conflicts**: concurrent writers / partial sync. Mitigation: single-writer-per-session, version refs, eventual consistency acceptable for MVP.
- **Over-abstraction**: ports designed but only one impl. Mitigation: keep ports minimal (verbs above), don't speculatively add methods.

## Open Questions (deferred, not blocking)

- Exact workspace versioning/retention policy.
- Team-scope memory governance (who can write shared memories) — dreaming writes; human curation TBD.
- Sandbox warm-pool sizing.

## Backlog (post-MVP, not in this change)

- **Security governance**: prompt-injection defenses, skill provenance/trust, output content filtering.
- **Runtime config / feature flags**: change model params, thresholds, approval policy without restart.
- **Billing / subscription**: quota source and plans (if commercialized).
- **Frontend session management**: multi-session list, history, sharing.
