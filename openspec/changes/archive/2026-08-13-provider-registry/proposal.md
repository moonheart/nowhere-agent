## Why

Provider/model configuration is hard-wired to environment variables: one `LLM_PROVIDER` + `LLM_MODEL` for the whole platform and a separate `VISION_*` pair for the `view_image` tool. Teams can only override the credential (via `team_api_keys`), never the model, so every team must share the platform model even when their workload wants a cheaper or stronger one — and any change means editing env and restarting. We need providers and models to be first-class, per-team-selectable configuration stored in Postgres.

## What Changes

- **New `providers` table**: system-level providers (vendor `anthropic`|`openai`, `base_url`, encrypted `api_key`, default model, enabled) managed at the platform-admin scope.
- **New `provider_models` table**: each provider has many models (name, display name, vision-capable flag, optional context-window override, default flag), enabling the platform to expose e.g. `gpt-4o-mini` (fast) and `o3` (reasoning) under one OpenAI provider.
- **Two provider scopes**: providers are either **system** (platform-admin managed, visible to every team) or **team** (a team's owner/admin manages its own providers/models with its own keys). A team can use a system provider directly or run its own.
- **New team-level assignment**: a team selects one provider + default model, which may reference a system provider or the team's own provider, replacing the per-team key override. The team's own provider keys stay team-private (encrypted, masked).
- **Runtime resolution**: chat (and scheduled-task / agent-definition runs) resolve provider+model per request from the team's assignment — which may point at a team-owned provider or a system provider — falling back to the platform default provider/model. The `view_image` tool's vision model resolves from the assigned provider's vision-capable model. **BREAKING** — `LLM_PROVIDER`/`LLM_MODEL`/`LLM_API_KEY`/`LLM_BASE_URL` and all `VISION_*` no longer drive model selection or credentials; chat is disabled until at least one enabled system provider exists.
- **Deprecate `team_api_keys`**: **BREAKING** — the team-key override mechanism and its admin UI are removed; providers (system or team) carry their own key.
- **Admin console**: new platform-admin system Providers/Models management page; teams gain their own provider/model management plus a provider+model assignment picker (replacing the API-key panel).
- Capability profiles (`LookupProfile`, models.dev-style table) still derive context-window/`ImageInput` by vendor+model name; model rows may override the context window.

## Capabilities

### New Capabilities
- `provider-registry`: system- and team-scoped provider and model registry — schema, encrypted-key storage, default provider/model selection, vision-capable flag, team-visibility boundaries.
- `model-resolution`: per-request provider+model resolution (team assignment over system or team-owned providers → platform default), vision-model resolution for `view_image`, and model-reference resolution for scheduled tasks / agent definitions.

### Modified Capabilities
- `model-routing`: Credential resolution requirement changes — team-key override is removed (provider owns the key); Routing policy requirement changes — provider/model are chosen from the DB registry (team assignment → platform default) rather than config/env.
- `admin-console`: Team provider credential management requirement is replaced by team provider+model assignment; a new platform-admin provider/model management surface is added.

## Impact

- **Schema**: new migration (providers, provider_models, team assignment); drop `team_api_keys`.
- **Backend**: new `internal/providerreg` (store + resolver); `cmd/server/main.go` wiring replaces `buildProvider`/`buildVisionProvider`/`adapterForCaller` with DB-backed resolution; `internal/routing` loses the team-key path; `internal/config` drops model-selection env vars; `internal/chatapi` (loop factory, tool binder) and `internal/schedule` (task model) resolve via the new resolver.
- **APIs**: new admin endpoints for providers/models and team assignment; removal of team-key endpoints.
- **Frontend**: admin console Providers page + team assignment UI; `web/src/lib/*` typed clients.
- **Ops**: operators bootstrap the first provider via the admin console (chat stays disabled until one exists); a `cmd/migrate --seed-from-env` convenience imports current `LLM_*`/`VISION_*` into the registry once.
