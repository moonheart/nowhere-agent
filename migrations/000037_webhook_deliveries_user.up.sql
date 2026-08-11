-- Outbox compliance + robustness follow-ups:
--  1. user_id links each delivery to the account it notifies about, with ON
--     DELETE CASCADE — deleting an account (PIPL §47 erasure) now also
--     removes its outbox rows, so no conversation summary survives the
--     account. NULL for rows whose user could not be resolved.
--  2. An index for the retention purge (failed rows older than the window).

ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS user_id UUID REFERENCES users(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_purge ON webhook_deliveries (status, created_at);
