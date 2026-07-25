-- Dreaming worker wiring (capability-gap K1): mark which ended sessions have
-- already been dreamed over, so the offline worker consumes each session's
-- episodes exactly once and resumes cleanly after a restart. Idempotency of a
-- dreaming pass rests on this column, NOT on the scheduler's in-memory last-run
-- map, so re-running the job (including the catch-up run at every boot) never
-- reprocesses a session.

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS dreamed_at TIMESTAMPTZ;

-- Backs the worker's "ended and not yet dreamed" scan each pass. Partial, so it
-- stays small: a row leaves the index the moment dreamed_at is set.
CREATE INDEX IF NOT EXISTS idx_sessions_ended_undreamed
    ON sessions (ended_at)
    WHERE status = 'ended' AND dreamed_at IS NULL;
