-- Per-submit hot path: internal/session/batch.go looks up a session's
-- pending snapshots by session_id on every chat submit
-- (SuspendedBatchesForSession), but suspended_batches (000026) is keyed only
-- by run_id, so the lookup is a sequential scan of every row the session's
-- team has ever suspended.

CREATE INDEX IF NOT EXISTS idx_suspended_batches_session ON suspended_batches(session_id);
