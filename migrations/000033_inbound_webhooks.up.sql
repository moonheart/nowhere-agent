-- Inbound webhooks (enterprise integration): a per-user endpoint that lets
-- external systems (ERP, OA, ITSM, IM bots) trigger an agent run with an
-- HTTP POST — no interactive client and no SSE connection required. The run
-- itself goes through the same RunRegistry path a human chat uses, and its
-- completion can be pushed back to a per-webhook notify_url (or the global
-- WEBHOOK_URL) via the outbound webhook notifier.
--
-- Auth: each webhook carries a random secret, returned once at creation and
-- stored AES-256-GCM encrypted at rest (internal/secrets, the same protection
-- team provider keys get). The trigger request must sign its body with
-- HMAC-SHA256 over "<timestamp>.<body>" (X-Nowhere-Signature), so the secret
-- never travels in a header that logs may capture.

CREATE TABLE inbound_webhooks (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name              TEXT NOT NULL,
    user_id           UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    team_id           UUID,
    secret_cipher     TEXT NOT NULL,
    agent_def         TEXT,
    system_prompt     TEXT,
    target_session_id UUID,
    notify_url        TEXT,
    enabled           BOOLEAN NOT NULL DEFAULT true,
    last_used_at      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_inbound_webhooks_user ON inbound_webhooks (user_id);
