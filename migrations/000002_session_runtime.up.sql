-- Session runtime (design D13): sessions, runs, and persisted run events
-- (episodes). Runs are flushed to the DB per iteration; the durable run
-- records are the episodes the dreaming worker consumes.

CREATE TABLE IF NOT EXISTS sessions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title       TEXT NOT NULL DEFAULT '',
    -- active | ended
    status      TEXT NOT NULL DEFAULT 'active',
    ended_at    TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);

CREATE TABLE IF NOT EXISTS runs (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id  UUID NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    -- run number within the session
    seq         INT NOT NULL,
    -- queued|running|waiting_approval|done|failed|cancelled
    status      TEXT NOT NULL DEFAULT 'queued',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    UNIQUE (session_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_runs_session ON runs(session_id);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);

-- Persisted run events: the durable run/event log (source of truth for
-- streaming replay) AND the episodes for dreaming. One row per loop event.
CREATE TABLE IF NOT EXISTS run_events (
    id          BIGSERIAL PRIMARY KEY,
    run_id      UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    session_id  UUID NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    -- per-run monotonically increasing
    seq_offset  INT NOT NULL,
    -- message|tool_use|tool_result|thinking|error|...
    kind        TEXT NOT NULL,
    payload     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, seq_offset)
);

CREATE INDEX IF NOT EXISTS idx_run_events_run ON run_events(run_id, seq_offset);
CREATE INDEX IF NOT EXISTS idx_run_events_session ON run_events(session_id);
