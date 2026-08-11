-- Persistent webhook delivery outbox (enterprise integration): run-completion
-- notifications are no longer fire-and-forget. Each delivery is a row here,
-- attempted immediately, and retried by a background sweeper with backoff
-- until delivered or dead-lettered — a process crash or a slow consumer no
-- longer loses a run-completion event. Statuses: pending → delivered |
-- failed (final, dead-lettered).

CREATE TABLE webhook_deliveries (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id          TEXT NOT NULL,
    session_id      TEXT NOT NULL,
    target_url      TEXT NOT NULL,
    payload         JSONB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending', -- pending | delivered | failed
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at    TIMESTAMPTZ
);

CREATE INDEX idx_webhook_deliveries_pending ON webhook_deliveries (status, next_attempt_at);
