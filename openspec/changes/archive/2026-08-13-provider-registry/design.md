## Context

Today a single provider+model is fixed by env (`LLM_PROVIDER`, `LLM_MODEL`, `LLM_API_KEY`, `LLM_BASE_URL`) and the vision model by `VISION_*`. The only runtime-varied piece is the credential: `routing.PGKeyStore` resolves a per-team key (`team_api_keys`) with platform-key fallback, and `adapterForCaller` (main.go) rebuilds the adapter per request with that key. Model selection is constant per deployment; scheduled tasks / agent definitions may override the model **string**, but the provider, key, and capability assumptions still come from env. The `provider.Registry`/`routing.Router` abstractions exist but are unwired.

This change replaces env-driven model selection with a Postgres provider registry: system-level providers (each with many models) + a per-team provider/model assignment. Provider keys replace team keys; `team_api_keys` and the `routing` team-key path are removed. The `view_image` vision model becomes a vision-flagged model in the same registry.

## Goals / Non-Goals

**Goals:**
- Provider+model is data, configured via admin console APIs, taking effect on the next request (no restart).
- A team selects a provider and default model from the enabled set; no team credentials.
- Vision model for `view_image` resolves from the assigned provider's vision-capable model.
- `team_api_keys` is removed; existing `LLM_*`/`VISION_*` no longer drive selection.

**Non-Goals:**
- Per-user or per-session model selection (team + platform default only).
- Automatic provider failover/fallback chains (the aspirational `model-routing` failover requirement stays out of scope).
- Two-level (team+platform) quota rework.
- Model capability editing beyond the existing `LookupProfile` table plus a per-model context-window override.

## Decisions

### D1. Schema (migration 000028, plus drop of 000003 table)
```
providers          id uuid pk, scope text check (system|team), team_id uuid null fk->teams
                   (NOT NULL when scope=team), name text, vendor text check (anthropic|openai),
                   base_url text null, api_key text null (encrypted envelope),
                   is_default bool, enabled bool, created_at, updated_at
provider_models    id uuid pk, provider_id uuid fk->providers on delete cascade,
                   name text, display_name text, vision bool default false,
                   context_window bigint null, is_default bool, enabled bool,
                   created_at, updated_at, unique(provider_id, name)
team_provider_settings  team_id uuid pk fk, provider_id uuid fk (system or team-owned),
                   model_id uuid null fk, created_at, updated_at
```
One registry table with a `scope` column rather than separate tables: providers and models share one store/code path, and a team's assignment can point at a system provider or a team-owned provider through the same FK. Name uniqueness is scoped: a partial unique index enforces `unique(name)` for system providers and `unique(team_id, name)` for team providers. Single-default invariants enforced with partial unique indexes:
`CREATE UNIQUE INDEX ON providers (is_default) WHERE is_default` (system default only) and
`CREATE UNIQUE INDEX ON provider_models (provider_id, is_default) WHERE is_default`.
`team_api_keys` is dropped in the same migration.

*Alternative considered:* separate `team_providers`/`team_provider_models` tables mirroring the system ones. Rejected — duplicated store logic and resolution paths for zero schema benefit; the `scope`+`team_id` single-table form keeps visibility in one WHERE clause.

### D2. New package `internal/providerreg` (ports & adapters, matching repo convention)
- `store.go` — `Store` interface: providers/models CRUD, `SetDefaultProvider`, `SetDefaultModel`, team assignment get/set/clear, plus resolver queries (`PlatformDefault`, `ModelsFor`, `TeamAssignment`, `VisibleToTeam` — system providers plus the team's own).
- `pgstore.go` — PG implementation. Provider keys encrypted with the existing `secrets.Encryptor` (same gradual-migration semantics as `PGKeyStore`); list/read endpoints mask keys via a moved `MaskKey`; `Resolve` decrypts and returns plaintext only to the adapter builder. Visibility is enforced in SQL (system rows OR `team_id = $me`), so a cross-team read returns nothing.
- `resolver.go` — `Resolver` (built over `Store` + team-membership query):
  - `Resolve(userID) (Target, error)` → team assignment (a system or team-owned provider → its default model) else platform default; error when no provider (`ErrNoProvider`).
  - `ResolveForTeam(teamID) (Target, error)` → used by the schedule trigger (a task's `TeamID`, else platform default).
  - `ResolveModel(target, name string) (string, error)` → resolves an explicit model reference against the target provider's enabled models by name; fail-closed on unknown.
  - `VisionModel(target) (string, bool)` → the provider's default vision-capable model (or first vision model), for `view_image`.
  - `Target` carries provider vendor/baseURL/APIKey + model name; the adapter factory in main.go builds the `anthropic`/`openai` adapter from it (reusing the `providerSettings` refactor), so `providerreg` stays free of adapter construction and import cycles.

`internal/routing` is deleted (its team-key `PGKeyStore` and unused `Router`); `MaskKey` moves into `providerreg`. `internal/adminapi`'s team-key endpoints (`listKeys`/`putKey`/`deleteKey`) are removed; `supportedProvider` is no longer needed.

### D3. Runtime wiring (main.go)
- `cfg.LLM.Provider/Model/APIKey/BaseURL` and `Vision` are removed from config; `LLM.ContextWindow/ThinkingBudget/RawLogDir/StreamIdleTimeout` remain (non-selection knobs).
- The huge `if adapter != nil` chat block becomes `if keyStore != nil`-style unconditional: the **loop factory** resolves per request. `newChatLoopWithModel(ctx, system, modelOverride)` → `resolver.ResolveForTeam(user's team)` (chat: caller user; schedule: `task.TeamID`) → build adapter → `ResolveModel` for the override → `agent.New(...)`. Resolution failure returns a clear error to chat (503 "no provider configured") and marks the scheduled fire failed.
- `adapterForCaller` is replaced by the resolver path. The per-request adapter build cost is unchanged from today.
- **Context window / compression**: window derivation moves into the per-request path using the resolved model (`LookupProfile(vendor, model)` + model-row override, `LLM_CONTEXT_WINDOW` still wins explicitly). The compressor is already built per-loop over the caller adapter/model, so this is a change of inputs, not shape.
- **Vision**: `VISION_*` gone. The tool binder (per-session) resolves the vision adapter lazily via `resolver.VisionModel` against the session's resolved target and registers `view_image` only when one exists (as today's `visionAdapter != nil && imageStore != nil` gate).
- **Subagent/scheduled model strings** (agentdef `Model`, task model): passed as `modelOverride` through `ResolveModel`; unknown name → fail-closed error instead of silent substitution.

### D4. Admin API + UI
- Platform routes (`requireAdmin`), new `internal/adminapi/providers.go`: `GET/POST /api/admin/providers`, `GET/PATCH/DELETE /api/admin/providers/{id}`, `POST /api/admin/providers/{id}/models`, `PATCH/DELETE /api/admin/providers/{id}/models/{mid}`, `POST /api/admin/providers/{id}/default`, `POST /api/admin/providers/default`. Keys masked in all reads; writes accept a new key or omit to keep. System-scoped rows only.
- Team routes (`requireTeamRole(RoleOwner|RoleAdmin)`): `GET/POST /api/teams/{id}/providers`, `GET/PATCH/DELETE /api/teams/{id}/providers/{pid}`, models under them (visibility: system providers read-only + the team's own editable), and `GET/PUT /api/teams/{id}/model` (provider+model assignment choosing among system and team-owned enabled providers). Removes `GET/PUT/DELETE /api/teams/{id}/keys`.
- Frontend: `web/src/components/admin/ProvidersPage.tsx` (platform: system providers list, expandable model rows, default/vision toggles), `TeamDetailPage` gains a team-provider section + a provider/model assignment picker (replacing the API-key panel), new `web/src/lib/providers.ts` typed client. Follows existing admin page patterns (`PlatformPages.tsx`, `TeamsPage.tsx`).

### D5. Bootstrap
Chat is disabled until an enabled provider exists. The admin console boots without LLM (unchanged behavior). To avoid a UI dance on first deploy, `go run ./cmd/migrate -seed-from-env` imports `LLM_*`/`VISION_*` into `providers`/`provider_models` once when the table is empty; otherwise operator creates the first provider via the console.

## Risks / Trade-offs

- **Chat dead until first provider** → Console boots LLM-free; `-seed-from-env` one-liner for existing deployments; clear 503 message when a provider is missing.
- **Per-request DB resolution adds latency to every run setup** → One indexed query per resolution on the request path (same tier as existing per-request key lookup today); store query results in a small TTL cache if profiling warrants it.
- **Encrypted provider keys at scale** → Reuse `secrets.Encryptor`; decrypt only on the resolution path; listing always masked; decrypt failure fails loud (spec) rather than silently substituting.
- **Model-name references (agentdef/scheduled tasks) can break after provider reassignment** → Fail-closed resolution with a clear run error; UI shows the provider's model list so operators pick valid names; default (empty reference) never breaks.
- **Dropping `team_api_keys` loses team keys on upgrade** → Migration drops the table; release notes instruct operators that provider keys must be (re)entered on providers (seeded from env once via `-seed-from-env`).
- **Single-default invariants** → Partial unique indexes enforce at the DB; API returns 409 on violations with a message to clear/repoint the current default first.

## Migration Plan

1. Add migration 000028 (create three tables + partial indexes, drop `team_api_keys`).
2. Land `internal/providerreg` (store + resolver) behind the existing encryptor; keep env path working in the same commit where feasible.
3. Switch chat wiring to the resolver; remove `LLM_*` model fields and `Vision`; remove `routing`/team-key code and admin endpoints.
4. Admin API + frontend pages.
5. Deploy order: run migrate → `-seed-from-env` (or create provider via console) → restart server. Rollback: revert code + restore `team_api_keys` migration is not automatic; operators with team keys should export them before upgrade (documented).

## Open Questions

- Should a provider's `base_url` default to the vendor's official endpoint when blank (yes, vendor adapters already handle empty endpoint) — confirm no per-provider raw-log override needed (out of scope; global `LLM_RAW_LOG_DIR` applies).
- Whether `provider_models` needs a `sort_order` for UI ordering or `display_name` ordering suffices (decide during implementation; low risk).
