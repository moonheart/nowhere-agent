DROP INDEX IF EXISTS idx_memories_created_at;

ALTER TABLE sessions DROP COLUMN IF EXISTS memory_injected_at;
