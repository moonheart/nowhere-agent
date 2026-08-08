-- Revert scheduled tasks: drop the sessions tags, then the definition table.
DROP INDEX IF EXISTS idx_sessions_task;
ALTER TABLE sessions DROP COLUMN IF EXISTS metadata;
ALTER TABLE sessions DROP COLUMN IF EXISTS source;
ALTER TABLE sessions DROP COLUMN IF EXISTS task_id;

DROP TABLE IF EXISTS scheduled_task;
