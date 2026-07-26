DROP INDEX IF EXISTS idx_approvals_one_pending_per_session;

CREATE UNIQUE INDEX IF NOT EXISTS idx_approvals_one_pending_per_run
    ON approvals (run_id)
    WHERE status = 'pending';
