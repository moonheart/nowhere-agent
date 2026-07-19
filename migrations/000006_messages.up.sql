-- Faithful conversation messages (design: persist-raw-messages D1). One row per
-- canonical provider.Message, with the full block array stored verbatim as JSONB
-- (text, thinking incl. signature, tool_use, tool_result; image blocks store a
-- workspace-relative path pointer, never the payload). This is the authoritative
-- record of conversation content — distinct from run_events, which records the
-- lifecycle/streaming frames for attach & replay.

CREATE TABLE IF NOT EXISTS messages (
    id          BIGSERIAL PRIMARY KEY,
    session_id  UUID NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    run_id      UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    -- per-session monotonically increasing, stable across runs
    seq         INT NOT NULL,
    -- user | assistant
    role        TEXT NOT NULL,
    -- []provider.Block, full fidelity (pointer-sized for images)
    content     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (session_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_messages_session_seq ON messages(session_id, seq);
CREATE INDEX IF NOT EXISTS idx_messages_run ON messages(run_id);
