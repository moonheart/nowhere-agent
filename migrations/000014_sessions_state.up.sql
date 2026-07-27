-- Generic session-level key/value state (capability-gap O1). One JSONB column
-- holds an open dictionary of session-scoped state, so any number of features
-- (plan/todo now; progress, scratch, config, ... later) can store state under
-- their own key without a schema change. Written per-key via jsonb_set (no
-- whole-column clobber), read back for live SSE fan-out and history recovery.
-- This is the durable record; the real-time push is a live-only broker event.

ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS state JSONB NOT NULL DEFAULT '{}';
