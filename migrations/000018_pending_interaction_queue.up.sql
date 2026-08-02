-- Multi-approval queue (sequential gated batch): a single model turn can emit
-- several permission-gated tool calls (and ask_user / client_tool calls). Each
-- becomes its OWN pending interaction; the client decides them one at a time,
-- and a fresh run resumes only once the whole batch (the run's interactions) is
-- resolved. This replaces the old "one pending interaction per session" model,
-- under which only the first gated call of a batch was surfaced and the rest
-- were silently dropped (folded as "[Tool use interrupted]").

-- Allow multiple pending interactions per session.
DROP INDEX IF EXISTS idx_approvals_one_pending_per_session;

-- The per-session pending lookup (queue echo to a reloading client) keeps the
-- existing non-unique index from 000010/000011.
-- (idx_approvals_session_pending is unchanged.)

-- Batch-complete check: "are any of this run's interactions still pending?"
-- A batch is all interactions sharing one run_id (the run that surfaced them).
CREATE INDEX IF NOT EXISTS idx_approvals_run_pending
    ON approvals (run_id)
    WHERE status = 'pending';
