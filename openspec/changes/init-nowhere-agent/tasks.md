# Tasks: init-nowhere-agent

## Implementation sequencing

**Critical path** (shortest line to an end-to-end run):
`1 → 2 → 3 → 5 → 10 → 13 → 4 → 17`

| Phase | Groups | Notes |
|---|---|---|
| 0 · Foundation | 1 scaffolding | module layout, config, DI, PG+migrations |
| 1 · Platform base | 2 identity-scope | user/team/auth/scope — everything hangs off this |
| 2 · Providers & execution | 3 provider-abstraction · 6 workspace-persistence → 5 sandbox · 15 observability (skeleton) | 3 / 6→5 / 15-skeleton are parallel lines; workspace before sandbox (materialize depends on it); observability skeleton now so routing has metering |
| 3 · Runtime core | 10 tool-runtime · 13 session-runtime · 4 agent-loop · 14 model-routing · 11 execution-permission | session-runtime designed early (it produces episodes + owns session lifecycle + single-run lock) |
| 4 · Intelligence | 12 context-management · 9 skill-system · 7 memory → 8 dreaming | 12 / 9 / 7 parallel; dreaming last (needs memory + episodes + real data) |
| 5 · Hardening & ship | 16 cross-cutting · 17 integration | some of 16 is inlined into its owning component (16.1→sandbox, 16.2→workspace) |

**Parallelizable**: phase 2 lines (3 | 6→5 | 15-skeleton); phase 4 (12 | 9 | 7).

---

## 1. Project scaffolding
- [ ] 1.1 Initialize Go module(s) and project layout (cmd/, internal/, pkg/)
- [ ] 1.2 Config loading, logging, and dependency-injection wiring
- [ ] 1.3 Postgres setup + migrations tooling

## 2. identity-scope
- [ ] 2.1 User & team domain models + persistence
- [ ] 2.2 Auth (signup/login, session/token)
- [ ] 2.3 Shared scope model (user/team/system) + access-control helpers

## 3. provider-abstraction
- [ ] 3.1 Canonical Message/Block model (Text/ToolUse/ToolResult/Thinking/CachePoint)
- [ ] 3.2 Event-based streaming contract (block_start/delta/stop, message_stop)
- [ ] 3.3 Anthropic adapter (with prompt caching + thinking round-trip)
- [ ] 3.4 OpenAI adapter
- [ ] 3.5 Adapter conformance tests

## 4. agent-loop
- [ ] 4.1 Think→tool→think loop orchestration
- [ ] 4.2 Tool dispatch + registry
- [ ] 4.3 Streaming output to gateway (WS/SSE)
- [ ] 4.4 Context-window management (short-term memory in-loop)
- [ ] 4.5 Memory recall injection (read side) + skill L0/L1 loading

## 5. sandbox
- [ ] 5.1 `SandboxPort` interface (create/destroy/exec/file-ops/materialize/solidify)
- [ ] 5.2 Built-in fs+Docker implementation, per-session lifecycle
- [ ] 5.3 Deferred stop on session end + scheduled destroy
- [ ] 5.4 Seam documented for remote sandbox protocols (gVisor/Firecracker)

## 6. workspace-persistence
- [ ] 6.1 Workspace volume model + version refs
- [ ] 6.2 Materialize-on-start / solidify-on-end sync (local-directory backend)
- [ ] 6.3 Restore on reactivation after long idle
- [ ] 6.4 Storage-backend seam for S3-compatible (MinIO)

## 7. memory
- [ ] 7.1 `MemoryPort` interface (Recall / Store / Forget / ListByScope)
- [ ] 7.2 Built-in implementation: Postgres + vector index
- [ ] 7.3 Scoped isolation (user/team/system) + access rules
- [ ] 7.4 Forgetting (GDPR delete) path

## 8. dreaming
- [ ] 8.1 Scheduled worker with configurable frequency
- [ ] 8.2 Extract: episode → facts/preferences
- [ ] 8.3 Compress: old episodes → summaries
- [ ] 8.4 Reorganize: conflict detection / deprecation
- [ ] 8.5 Reflect: cross-session insights
- [ ] 8.6 LLM budget control

## 9. skill-system
- [ ] 9.1 Skill model (SKILL.md + resources + scripts)
- [ ] 9.2 Progressive disclosure: L0/L1/L2 loading
- [ ] 9.3 Three-scope store (system/team/user) with merge + priority override
- [ ] 9.4 L2 script execution routed through sandbox

## 10. tool-runtime
- [ ] 10.1 `Tool` interface (name/schema/risk/timeout/call)
- [ ] 10.2 Schema delivery to model in function-calling format
- [ ] 10.3 Dispatch with timeout/cancel + concurrency within a turn
- [ ] 10.4 Error/stderr feedback to model for self-correction
- [ ] 10.5 Built-in tools (file/command/web) + MCP seam

## 11. execution-permission
- [ ] 11.1 Execution-permission policy engine (allow/ask/deny per risk level)
- [ ] 11.2 Default-permissive inside sandbox; gate sandbox-escaping actions (network/external writes/cost)
- [ ] 11.3 Approval UX flow over WS/SSE + audit log

## 12. context-management
- [ ] 12.1 Threshold-triggered compression (configurable % of window)
- [ ] 12.2 Sliding-window summary strategy
- [ ] 12.3 Boundary tests vs dreaming (no double-write, offline recovery)

## 13. session-runtime
- [ ] 13.1 Run state machine (queued/running/waiting_approval/done/failed/cancelled)
- [ ] 13.2 Durable run event log (append-only)
- [ ] 13.3 WS/SSE subscription + reconnect/replay from offset
- [ ] 13.4 Run stop/cancel propagating to tools + sandbox
- [ ] 13.5 Multi-client attach to one session

## 14. model-routing
- [ ] 14.1 Credential resolution: platform key default, team key override
- [ ] 14.2 Routing policy (provider+model selection)
- [ ] 14.3 Two-level quota + rate limiting (platform/team)
- [ ] 14.4 Provider failover on error/rate-limit

## 15. observability
- [ ] 15.1 Run tracing across model/tool/permission/sandbox spans
- [ ] 15.2 Per-user/team token + cost metering feeding quota
- [ ] 15.3 Structured logs + dreaming run metrics
- [ ] 15.4 Session replay view

## 16. Cross-cutting hardening
- [ ] 16.1 Sandbox egress proxy + NetworkPolicy enforcement (allowlist/deny)
- [ ] 16.2 Workspace two-stage atomic solidify
- [ ] 16.3 Run-per-iteration flush to DB (episodes for dreaming)
- [ ] 16.4 Session lifecycle: N-minute idle end, single signal to sandbox/workspace/dreaming
- [ ] 16.5 Single-active-run + multi-writer block with client state sync
- [ ] 16.6 Unified scheduler (dreaming/sandbox-destroy/quota-rollover) + UTC + catch-up
- [ ] 16.7 Skill versioning + override review/rollback

## 17. Integration
- [ ] 17.1 Gateway wiring: auth + session runtime + loop + WS/SSE
- [ ] 17.2 Minimal browser chat UI
- [ ] 17.3 End-to-end: user → session → agent → sandboxed skill script → memory → dreaming, traced + metered
