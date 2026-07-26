DROP INDEX IF EXISTS idx_sessions_ended_undreamed;
ALTER TABLE sessions DROP COLUMN IF EXISTS dreamed_at;
