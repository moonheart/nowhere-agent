# Proposal: init-nowhere-agent

## Why

We are building a multi-user AI agent platform (B/S architecture) from scratch. Before any implementation, we need to lock the foundational architecture so that the load-bearing decisions — agent loop, provider abstraction, sandboxing, memory (including offline "dreaming"), and the skill system — are captured once, correctly, with extension seams left where external systems will later plug in. Getting these boundaries right up front avoids expensive re-architecture when we later integrate alternative sandboxes (gVisor/Firecracker) or memory frameworks (Mem0/Zep).

## What Changes

- Introduce a **self-built Go agent loop** owning think→tool→think orchestration, streaming, and context-window management.
- Define a **custom provider abstraction** (internal Message/Tool canonical model) with per-provider adapters (Anthropic, OpenAI, …). The canonical model follows the Anthropic block shape (content blocks, thinking, cache points), not OpenAI's, to preserve advanced capabilities.
- Introduce **per-session sandboxing** behind a `SandboxPort` interface; built-in implementation uses filesystem isolation + Docker. Seam left for remote sandbox protocols (gVisor, Firecracker).
- Define **workspace persistence**: containers are stateless compute units; the per-session workspace lives on an external persistent volume, materialized into the sandbox at start and solidified back at end. Abstraction follows a sync model so S3-compatible storage can replace the built-in local-directory backend without changing the interface.
- Introduce a **read/write-split memory system** behind a `MemoryPort`. Short-term memory lives in the loop's context; long-term memory is read online (fast, cached) and written exclusively by the offline **dreaming worker**.
- Introduce a **scheduled dreaming worker** (configurable frequency) that consolidates, compresses, reorganizes, and reflects on memories.
- Introduce a **scoped skill system**: general-form skills (SKILL.md + resources + scripts) with progressive disclosure (three-level loading) across system / team / user scopes. Skill and memory share one scope model.
- Introduce a **unified tool runtime** treating built-in tools, skill scripts, and (future) MCP tools behind one `Tool` interface with schema delivery, dispatch, timeout/cancel, and error feedback.
- Establish a **two-layer permission model**: resource-level access (identity-scope) plus runtime execution-permission that is permissive inside the sandbox and gates sandbox-escaping actions behind a configurable approval policy.
- Introduce **online context management** (threshold-triggered compression of short-term context), explicitly separated from offline dreaming.
- Introduce **observability** across the async system: full session/run tracing, per-user/team LLM token & cost metering, and structured logs/metrics (essential for debugging and cost control).
- Introduce a **session runtime** managing run lifecycle (queued→running→waiting_approval→done/failed), disconnect/reconnect with event replay, and run stop/cancel so browser refreshes never lose work.
- Introduce **model routing & quota**: platform-held API keys by default with per-team key override, two-level (platform/team) quota and rate limiting, and provider failover.
- Establish a shared **identity & scope model** (user / team / system) used by skills and memory for ownership, isolation, and access control.

## Capabilities

### New Capabilities
- `agent-loop`: Self-built Go think→tool→think loop, streaming output, tool dispatch, context-window management.
- `provider-abstraction`: Canonical Message/Tool model + provider adapters, event-based streaming, prompt caching, thinking round-trip.
- `sandbox`: Per-session sandbox lifecycle behind `SandboxPort`; built-in fs+Docker; seam for remote protocols.
- `workspace-persistence`: External persistent workspace, materialize/solidify lifecycle, sync-model abstraction, pluggable storage backend.
- `memory`: Read/write-split long-term memory behind `MemoryPort`; short-term memory in-loop; scoped isolation and forgetting.
- `dreaming`: Scheduled offline memory worker (extract, compress, reorganize, reflect) with configurable cadence and budget control.
- `skill-system`: General-form skills, progressive disclosure, three-level scope (system/team/user), sandboxed script execution.
- `identity-scope`: Users, teams, auth, and the shared user/team/system scope model (resource-level access control).
- `tool-runtime`: Unified tool abstraction (built-in / skill-script / MCP-seam), schema delivery to the model, dispatch, timeout/cancel, concurrency, and error feedback to the model.
- `execution-permission`: Runtime authorization of agent actions — permissive inside the sandbox, configurable approval policy for actions that escape it (network, external writes, cost-incurring calls).
- `context-management`: Online compression of short-term context (threshold-triggered), kept distinct from offline dreaming.
- `observability`: End-to-end session/run tracing, per-user/team LLM token & cost metering, structured logging, dreaming run metrics.
- `session-runtime`: Run lifecycle state machine, disconnect/reconnect with event replay, run stop/cancel, and multi-client attach to a session.
- `model-routing`: Provider/model selection policy, platform-held keys by default with per-team key override, two-level quota/rate-limiting, and provider failover.

### Modified Capabilities
<!-- None — greenfield. -->

## Impact

- **New codebase**: Go module(s), project layout, core domain types.
- **External dependencies**: Docker daemon (built-in sandbox), Postgres + vector index (built-in memory), object storage / local volume (workspace).
- **Interfaces established** (the long-lived contracts): `ProviderAdapter`, `SandboxPort`, `MemoryPort`, `SkillStore`, scope model.
- **Non-goals for this change**: actual integration of gVisor/Firecracker or Mem0/Zep (seams only); frontend UI beyond a functional chat surface; production-grade infra.
