DROP INDEX IF EXISTS idx_sessions_undreamed;
ALTER TABLE sessions DROP COLUMN IF EXISTS dreamed_seq;
-- Restore the migration 000008 eligibility index (ended + not yet dreamed).
CREATE INDEX IF NOT EXISTS idx_sessions_ended_undreamed
    ON sessions (ended_at)
    WHERE status = 'ended' AND dreamed_at IS NULL;
