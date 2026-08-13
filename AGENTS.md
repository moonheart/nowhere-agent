# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> Note: this file is also served as `AGENTS.md`; `CLAUDE.md` re-exports it via `@AGENTS.md`. Keep all guidance here, not in `CLAUDE.md`.

## 项目规则 (Project Rules)

1. 禁止修改 shadcn 组件 `web/src/components/ui` 目录下的文件 (Never hand-edit files under `web/src/components/ui` — they are shadcn-generated; regenerate instead).

## What this is

nowhere-agent is a self-hosted, multi-tenant streaming AI agent platform: a Go gateway (`cmd/server`) plus a React SPA (`web`). A single Postgres holds all durable state; an optional Redis fans live output across instances. The core is a hand-built think→tool→think agent loop that streams the AI SDK v6 ui-message-stream protocol to the browser.

## Build / test / run

Backend (Go 1.26, module `nowhere-agent`):

```bash
go build ./...                # build everything
go test ./...                 # full suite (REQUIRED before commit — see memory)
go vet ./...                  # vet (must stay clean)
go test ./internal/skill/     # one package
go test ./internal/chatapi/ -run TestName -count=1   # one test
go run ./cmd/migrate          # apply DB migrations (golang-migrate, migrations/)
go run ./cmd/server           # run the gateway (needs env, see below)
go run ./cmd/mockllm          # fake LLM server for local dev / e2e
```

Frontend (`web/`, React 19 + Vite 8 + TypeScript ~6, Tailwind 4, shadcn on Base UI):

```bash
cd web
pnpm dev                      # vite dev server (HMR)
pnpm build                    # tsc -b && vite build -> web/dist
pnpm lint                     # oxlint
npx tsc --noEmit -p tsconfig.app.json   # type-check only (fast)
```

Serve the SPA from Go by setting `WEB_DIR=web/dist`; Go's `spaHandler` falls back to `index.html` for client routes. React Compiler is enabled (babel-plugin-react-compiler).

## Runtime configuration

Everything is env-driven via `internal/config` (envconfig + optional `.env`; real env wins over `.env`). Key vars: `DB_DSN` (Postgres; pool tuning `DB_MAX_OPEN_CONNS`/`DB_MAX_IDLE_CONNS`/`DB_CONN_MAX_LIFETIME`), `HTTP_ADDR` + timeouts (`HTTP_READ_TIMEOUT`/`HTTP_WRITE_TIMEOUT`/`HTTP_SHUTDOWN_TIMEOUT`) and per-IP rate limits (`HTTP_RATE_LIMIT_RPS`/`HTTP_RATE_LIMIT_BURST`) + `HTTP_TRUSTED_PROXY_CIDRS` (comma-separated reverse-proxy CIDRs whose X-Forwarded-For/X-Real-IP are honoured for client-IP resolution — rate-limit keys, audit, login throttle; empty = socket peer only, the secure default) + `HTTP_COOKIE_SECURE` (Secure attribute on the OIDC state cookie; default true, set false for a plain-HTTP deployment without TLS — a Secure cookie is never sent by the browser, silently breaking SSO login), `LOG_LEVEL`/`LOG_FORMAT`, `WEB_DIR` (built SPA), `STREAM_BROKER=mem|redis` + `REDIS_ADDR`, `SANDBOX_BACKEND=off|local|docker` (+ `SANDBOX_NETWORK`/`SANDBOX_LOCAL_EXEC`/`SANDBOX_WORKSPACE_DIR`/`SANDBOX_SHELL`), `WORKSPACE_DIR` (image payloads) + `WORKSPACE_RETENTION_DAYS` (how long ended sessions' image dirs are kept before the hourly sweep; default 30, <=0 disables), `UPLOAD_MAX_FILES_PER_USER`/`UPLOAD_MAX_BYTES_PER_USER` (per-user image-upload quota, defaults 200 files/200 MiB; 0 = unlimited, 413 at the cap), `SECRETS_MASTER_KEY` (encrypts stored provider credentials), `MCP_SERVERS` (JSON server list; legacy `MCP_ENABLED`/`MCP_SEARXNG_URL` map to one "searxng" server), `HTTP_TOOL_ALLOWLIST`/`HTTP_TOOL_TIMEOUT`, `QUERY_DB_DSNS`/`QUERY_DB_TIMEOUT`, `SUBAGENT_ENABLED`/`SUBAGENT_MAX_DEPTH`/`SUBAGENT_MAX_TOTAL`/`SUBAGENT_MAX_CONCURRENT`, `PERMISSION_READ_ONLY`/`PERMISSION_SANDBOX_WRITE`/`PERMISSION_NETWORK`/`PERMISSION_EXTERNAL_WRITE`, `REDACT_ENABLED`/`REDACT_STRATEGY`/`REDACT_CATEGORIES`, `WEBHOOK_URL`/`WEBHOOK_TIMEOUT`/`WEBHOOK_RETRIES`/`WEBHOOK_SSRF_ALLOWLIST`/`WEBHOOK_SIGNING_SECRET`, `DREAMING_ENABLED`/`DREAMING_INTERVAL`/`DREAMING_MAX_FACTS`/`DREAMING_MAX_INSIGHTS`/`DREAMING_MAX_SUMMARIES`/`DREAMING_PURGE_AFTER`, `SCHEDULE_ENABLED`/`SCHEDULE_SCAN_INTERVAL` (scheduled-task trigger; CRUD works with it off), `BOOTSTRAP_ADMIN_EMAIL`, `OIDC_*` (SSO; disabled unless `OIDC_ISSUER` is set), `PHONE_SMS_URL`/`PHONE_SMS_TIMEOUT`, and the model-independent LLM tunables `LLM_CONTEXT_WINDOW` (enables in-loop compression), `LLM_TEMPERATURE`, `LLM_THINKING_BUDGET`, `LLM_STREAM_IDLE_TIMEOUT`, `LLM_SYSTEM_LANG`, `LLM_RAW_LOG_DIR`. **LLM providers, models, and API keys are NOT env-config**: they are DB-managed data in the provider registry (`provider_registry`, `internal/providerreg`) and configured from the admin console, resolved per request — so edits and reassignments apply without a restart. Many env vars above are also the boot defaults for runtime-settable settings (`internal/settings`), which the admin console can override live (persisted in `platform_settings`); the runtime snapshot reloads periodically so multi-instance deployments converge. Defaults are documented on each struct in `internal/config/config.go`.

## Big-picture architecture

The design is strict **ports & adapters** — every boundary is a Go interface with a symmetric in-memory and PG/Redis implementation: `session.Store`/`MessageStore`, `session.StreamBroker`/`EventBus`, `sandbox.Port`, `memory.Port`, `skill.Store`, `provider.Adapter`, `identity.Store`. `cmd/server/main.go` is the single wiring point that picks implementations from config and registers HTTP routes; read it first to see what is actually reachable (openspec specs describe intent, not runtime truth).

Request flow, end to end:

1. **chatapi** (`internal/chatapi`) — the HTTP/SSE transport. Builds an agent loop per request, streams frames in ui-message-stream format (`data: <json>\n\n` + `data: [DONE]`), handles attach/resume/history. Emission must stay conformant with `assistant-stream` chunk-types; there is a conformance decoder test (`uimessage_stream_conformance_test.go`) that pins this.
2. **agent loop** (`internal/agent/loop.go`) — orchestration. Drives a `provider.Adapter`, dispatches tool calls, emits canonical `EventKind` events (text/thinking/tool_use/tool_result/message/step/error/done/...). Step frames (`KindStepStart`/`KindStepFinish`) carry per-iteration `finishReason`/`usage`/`isContinued`.
3. **session runtime** (`internal/session`) — durable runs. Two tracks that must stay cleanly separated: **live content** deltas go to the broker (hot path, never persisted), **assembled messages** go to `messages`, **lifecycle** to `run_events`. `RunRegistry` decouples a run from its submitting connection (run ctx derives from `context.Background()`), so submitters and later attachers share one symmetric `attach` path with offset-high-watermark dedup. Terminal events persist before `CompleteRun` to close the "inactive but no terminal frame" race.
4. **provider** (`internal/provider`, `anthropic`/`openai`) — neutral block model (thinking+signature round-trip, cache points, lazy image materialization). Providers/models/keys are DB-managed (`provider_registry`, `internal/providerreg`): `providerreg.NewResolver` resolves a system or team-scoped provider per request (`ResolveForTeam`), so registry edits apply without a restart; any resolution failure degrades to the platform key rather than failing chat.

Supporting subsystems, all feeding the loop's system prompt / tool registry per run: **identity** (users/teams, bcrypt+token auth, `RequireAuth`), **permission** (risk-based tool gate; `Ask` suspends via the `ApprovalReasonPrefix` marker), **toolruntime/builtin** (file tools, run_command, ask_user, plan_write) bound per session, **skill** (L0/L1/L2 progressive-disclosure skills, three scope tiers user>team>system, two-table versioning), **memory** (PG+vector recall; `recall_memory` tool) and **dreaming** (offline consolidation of episodes→long-term memory on a scheduler), **subagent** (`spawn_agent` tool; children draw from a scoped view of the parent's registry), **mcp** (SearXNG over Streamable HTTP, reconnect-in-background), **sandbox** (local/docker isolation for tools), **contextmgmt** (LLM compression near the window), **schedule** (scheduled tasks — recurring agent runs; a trigger claims due rows atomically and fires each through the same `RunRegistry.Submit` path a human chat uses, binding a whitelist-filtered tool registry), **scheduleapi** (self-service task CRUD) and **adminapi/skillapi** (management consoles, all behind the same auth).

Frontend: `useDataStreamRuntime` (assistant-ui) consumes the ui-message-stream; `web/src/lib/*` are typed clients for the HTTP APIs; admin console + skill editor live under `web/src/components/admin`.

## Conventions worth knowing

- **Go tests**: write `*_test.go` for all new Go code; `go test ./...` must be green before committing. PG tests (`internal/skill`, `internal/skillapi`, etc.) run against the **real dev Postgres** — use unique random names and delete only rows you created, by ID; never an unscoped `DELETE`/`UPDATE`. All PG tests share ONE instance (the CI `test-postgres` job runs every package's PG tests against a single postgres:16 service), so `TRUNCATE`, fixed ids, and unscoped deletes are forbidden: they corrupt sibling packages' tests and fail only under CI.
- **`run_events` is deprecated** for new features — record via slog or the `messages`/`sessions` tables instead.
- **Commits**: no Co-Authored-By / Claude attribution trailer.
- **openspec** (`openspec/specs`, `openspec/changes`) holds capability specs; treat as design intent. The trustworthy source for "what is wired" is `cmd/server/main.go`.
- `docs/` has architecture reviews (Chinese) useful for deep context, but they age — verify against current code.
