-- Usage/budget hot path: the 8 created_at-range queries in internal/usage
-- (totals, per-user/team, by-user/team, daily buckets, budget) scan runs by
-- created_at, and the budget check runs on every chat submit and scheduled
-- fire. idx_runs_created_at serves the range scans; idx_runs_session_created
-- serves per-session range scans (runs list, history rebuilds).

CREATE INDEX IF NOT EXISTS idx_runs_created_at ON runs(created_at);
CREATE INDEX IF NOT EXISTS idx_runs_session_created ON runs(session_id, created_at);
