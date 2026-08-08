-- Run attribution (enterprise-readiness P1-3): who to bill a run to, and which
-- model produced it. Until now runs recorded only the owning account (via the
-- session), so team usage was reconstructed as the sum over a team's CURRENT
-- members — an approximation that double-counts members of several teams and
-- lets a departed member take their history with them. Stamp the attributing
-- team and the model on the run itself at submit time and the report becomes
-- exact, and per-model breakdown / cost estimation stops having to guess.

-- team_id is nullable and deliberately NOT a foreign key: deleting a team must
-- not take its historical runs with it, and the column records attribution, not
-- membership. NULL means the run was billed to the platform key (no team
-- override applied), so team-grouped reports naturally exclude platform spend.
ALTER TABLE runs
    ADD COLUMN IF NOT EXISTS team_id UUID,
    ADD COLUMN IF NOT EXISTS model   TEXT;

-- Backfill is intentionally NOT attempted: runs recorded before this column
-- existed have no reliable attributing team (the membership at the time is not
-- recoverable), and a guessed team is worse than none. Historical rows simply
-- read as unattributed, exactly as they did when usage summed current members.

CREATE INDEX IF NOT EXISTS idx_runs_team ON runs(team_id) WHERE team_id IS NOT NULL;
