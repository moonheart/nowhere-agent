-- Enforce at most one non-terminal run per session at the database level. The
-- runtime's single-active-run lock is an in-process map, so it does not cover a
-- second instance (or a run left active in the DB). This partial unique index is
-- the race-free backstop for the invariant.

-- First resolve any pre-existing duplicates (e.g. runs stranded 'running' by an
-- old process) so the index can be created: keep the latest active run per
-- session by seq, mark older active runs failed.
UPDATE runs r SET status = 'failed', finished_at = now()
WHERE status IN ('queued', 'running', 'waiting_approval')
  AND EXISTS (
    SELECT 1 FROM runs r2
    WHERE r2.session_id = r.session_id
      AND r2.status IN ('queued', 'running', 'waiting_approval')
      AND r2.seq > r.seq
  );

CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_one_active_per_session
    ON runs (session_id)
    WHERE status IN ('queued', 'running', 'waiting_approval');
