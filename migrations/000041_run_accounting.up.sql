-- Durable run accounting (change durable-run-accounting): per-request usage
-- ledger decoupled from message rows, and per-step intent records with durable
-- attempt counts. Both are written BEFORE the effect they account for completes
-- (usage at settle; intents before the provider request / tool execution).

CREATE TABLE IF NOT EXISTS run_steps (
    id                BIGSERIAL PRIMARY KEY,
    run_id            UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    -- per-run monotonic; order of intent writes within the run
    seq               INT NOT NULL,
    -- assistant | tool | overflow_compact
    step_kind         TEXT NOT NULL,
    -- durable attempt count, 1-based within (run_id, step_kind)
    attempt           INT NOT NULL,
    -- pre-provisioned messages.id the step's result is expected to use.
    -- The message may legitimately never exist (failed attempt, discarded
    -- response); recovery treats an intent without its result as interrupted.
    result_message_id BIGINT,
    -- step_kind=tool: the tool call identity (assistant message's tool_use id)
    tool_call_id      TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_run_steps_run ON run_steps(run_id, seq);
CREATE INDEX IF NOT EXISTS idx_run_steps_kind ON run_steps(run_id, step_kind, seq);

CREATE TABLE IF NOT EXISTS usage_records (
    id                BIGSERIAL PRIMARY KEY,
    run_id            UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    -- assistant | tool | adjustment
    cause             TEXT NOT NULL,
    -- bound to the pre-provisioned id of the message the request was expected
    -- to produce; the message may not exist (failed/discarded request)
    result_message_id BIGINT,
    attempt           INT,
    input             INT NOT NULL DEFAULT 0,
    output            INT NOT NULL DEFAULT 0,
    cache_read        INT NOT NULL DEFAULT 0,
    cache_write       INT NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_usage_records_run ON usage_records(run_id);
