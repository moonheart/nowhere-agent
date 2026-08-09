-- Provider registry (change provider-registry): DB-managed LLM providers and
-- models replace the env-var model selection (LLM_* / VISION_*).
--
-- Three tables:
--
--   providers                 — one registry for both scopes. system providers
--                               are platform-managed and visible to every team;
--                               team providers are owned by one team (team_id
--                               NOT NULL) and visible only to that team.
--   provider_models           — every model of a provider (name, display name,
--                               vision-capable flag, optional context-window
--                               override, default flag).
--   team_provider_settings    — one row per team: the team's selected provider
--                               (system or team-owned) and default model.
--
-- team_api_keys (migration 000003) is dropped: teams no longer supply
-- credentials; the provider row owns the key.

CREATE TABLE IF NOT EXISTS providers (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- system | team
    scope      TEXT NOT NULL CHECK (scope IN ('system', 'team')),
    -- set only for team scope
    team_id    UUID REFERENCES teams(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    -- anthropic | openai
    vendor     TEXT NOT NULL CHECK (vendor IN ('anthropic', 'openai')),
    base_url   TEXT NOT NULL DEFAULT '',
    -- encrypted envelope when SECRETS_MASTER_KEY is configured
    api_key    TEXT NOT NULL DEFAULT '',
    -- platform default (system scope only, at most one)
    is_default BOOLEAN NOT NULL DEFAULT false,
    enabled    BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((scope = 'system' AND team_id IS NULL) OR (scope = 'team' AND team_id IS NOT NULL))
);

-- Provider names are unique within a scope: globally for system providers,
-- per owning team for team providers.
CREATE UNIQUE INDEX IF NOT EXISTS idx_providers_system_name ON providers(name) WHERE scope = 'system';
CREATE UNIQUE INDEX IF NOT EXISTS idx_providers_team_name  ON providers(team_id, name) WHERE scope = 'team';

-- At most one platform default provider.
CREATE UNIQUE INDEX IF NOT EXISTS idx_providers_system_default ON providers(is_default) WHERE scope = 'system' AND is_default;

CREATE INDEX IF NOT EXISTS idx_providers_team ON providers(team_id) WHERE scope = 'team';

CREATE TABLE IF NOT EXISTS provider_models (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id    UUID NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    -- provider API model identifier, e.g. "gpt-4o-mini"
    name           TEXT NOT NULL,
    display_name   TEXT NOT NULL DEFAULT '',
    -- vision-capable: usable by the view_image tool
    vision         BOOLEAN NOT NULL DEFAULT false,
    -- optional context-window override (NULL = derive from capability table)
    context_window BIGINT,
    is_default     BOOLEAN NOT NULL DEFAULT false,
    enabled        BOOLEAN NOT NULL DEFAULT true,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_id, name)
);

-- At most one default model per provider.
CREATE UNIQUE INDEX IF NOT EXISTS idx_provider_models_default ON provider_models(provider_id, is_default) WHERE is_default;

CREATE TABLE IF NOT EXISTS team_provider_settings (
    team_id     UUID PRIMARY KEY REFERENCES teams(id) ON DELETE CASCADE,
    -- the team's selected provider: system or team-owned
    provider_id UUID NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,
    -- the team's default model; NULL means the provider's default model
    model_id    UUID REFERENCES provider_models(id) ON DELETE RESTRICT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- team_api_keys is deprecated: provider rows own credentials now. Dropping the
-- table removes the per-team key override mechanism and its data.
DROP TABLE IF EXISTS team_api_keys;
