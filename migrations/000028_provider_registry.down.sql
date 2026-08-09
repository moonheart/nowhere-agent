DROP TABLE IF EXISTS team_provider_settings;
DROP TABLE IF EXISTS provider_models;
DROP TABLE IF EXISTS providers;

-- Restore team_api_keys (migration 000003).
CREATE TABLE IF NOT EXISTS team_api_keys (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id    UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    -- anthropic | openai | ...
    provider   TEXT NOT NULL,
    api_key    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (team_id, provider)
);

CREATE INDEX IF NOT EXISTS idx_team_api_keys_team ON team_api_keys(team_id);
