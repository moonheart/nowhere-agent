-- Revert to one-pending-per-session. First drop the batch-pending index, then
-- collapse any session that has more than one pending interaction down to its
-- earliest (the rest are abandoned), so the unique index can be recreated.

DROP INDEX IF EXISTS idx_approvals_run_pending;

-- Abandon all but the earliest pending interaction per session.
DELETE FROM approvals a
USING approvals b
WHERE a.session_id = b.session_id
  AND a.status = 'pending'
  AND b.status = 'pending'
  AND b.created_at < a.created_at;

CREATE UNIQUE INDEX IF NOT EXISTS idx_approvals_one_pending_per_session
    ON approvals (session_id)
    WHERE status = 'pending';
