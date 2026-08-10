-- Programmatic access credentials (enterprise integration): long-lived machine
-- tokens for external systems calling the agent API. Unlike auth_tokens (30-day
-- TTL, user sessions), a service key is issued by an admin, scoped to a user
-- (inheriting that user's permissions), optionally never expires, and can be
-- revoked independently of the account's other tokens.

CREATE TABLE service_keys (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    token_hash  TEXT NOT NULL UNIQUE,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ
);

CREATE INDEX idx_service_keys_user ON service_keys (user_id);
