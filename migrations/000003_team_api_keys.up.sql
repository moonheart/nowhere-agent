-- Team-scoped provider API keys (design D14): teams may configure their own
-- provider credentials, which override the platform-held key for members.
-- One key per (team, provider); api_key holds the secret (encrypt at rest in
-- production via pgcrypto or an external KMS — plaintext only for local dev).

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
