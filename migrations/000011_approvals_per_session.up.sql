-- Run-stateless HITL (capability-gap O2 refactor): an approval is now a
-- THREAD-level pending decision, not tied to a suspended run. At most one
-- pending approval per SESSION (a run ends when it surfaces a gated call; the
-- verdict drives a fresh run). Replace the per-run unique index with a
-- per-session one.

DROP INDEX IF EXISTS idx_approvals_one_pending_per_run;

CREATE UNIQUE INDEX IF NOT EXISTS idx_approvals_one_pending_per_session
    ON approvals (session_id)
    WHERE status = 'pending';
