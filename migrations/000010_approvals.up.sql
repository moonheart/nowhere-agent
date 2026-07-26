-- HITL tool approval (capability-gap O2): persist a pending human decision for
-- a dangerous tool call so a run can SUSPEND (release its worker goroutine and
-- the single-active-run lock) and later RESUME from durable state — surviving
-- process restarts and working across instances. The run itself is marked
-- waiting_approval (RunWaitingApproval, already an Active status); this table is
-- the durable record of WHICH tool call awaits a verdict and what the verdict was.
--
-- Lifecycle: pending → (approved | rejected) by the user, or → expired by a
-- cleanup sweep (future). A run carries at most one pending approval at a time
-- (the loop suspends on the first Ask verdict), enforced by the partial unique
-- index below.

CREATE TABLE IF NOT EXISTS approvals (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id       UUID NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    session_id   UUID NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    tool_call_id TEXT NOT NULL,
    tool_name    TEXT NOT NULL,
    tool_input   JSONB NOT NULL DEFAULT '{}',
    -- kind: 'approval' (a dangerous call needing yes/no) or 'ask_user' (the
    -- model asking structured questions). The suspended run + resume path is
    -- shared; kind only changes what the user is shown and what resume feeds back.
    kind         TEXT NOT NULL DEFAULT 'approval',
    status       TEXT NOT NULL DEFAULT 'pending',
    -- answer: the user's structured response (ask_user) e.g. {"answers":{...}}.
    -- NULL for a permission approval (the verdict is in status).
    answer       JSONB,
    decided_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Idempotent for databases where an earlier 000010 already created the table
-- without these columns (a forward-fix within the same migration version).
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'approval';
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS answer JSONB;

-- One pending approval per run: a second Ask while one is outstanding is a bug.
CREATE UNIQUE INDEX IF NOT EXISTS idx_approvals_one_pending_per_run
    ON approvals (run_id)
    WHERE status = 'pending';

-- Find a session's outstanding approval fast (the decision endpoint + resume).
CREATE INDEX IF NOT EXISTS idx_approvals_session_pending
    ON approvals (session_id)
    WHERE status = 'pending';
