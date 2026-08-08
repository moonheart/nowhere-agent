-- Scheduled tasks (scheduled-tasks capability): durable, owner-scoped definitions
-- of recurring agent runs. A trigger loop scans for due tasks and fires each
-- through the same run registry a human chat uses, persisting the output as a
-- normal session.
--
-- One table holds the definition; three columns on sessions tag the output:
--
--   scheduled_task   — one row per recurring task. The cron expression and IANA
--                      timezone produce next_run_at; the trigger claims a due
--                      task by atomically advancing next_run_at (design D4), so
--                      multiple instances never fire the same occurrence twice.
--                      user_id / target_session_id are FK'd rows (same idiom as
--                      sessions.user_id), while agent_def_name is an
--                      application-level identifier (agent definitions live in
--                      the in-memory agentdef store, resolved by name), so it is
--                      TEXT, not a FK (same idiom as skills/memories owner ids).
--
--   sessions.task_id / sessions.source / sessions.metadata — which task made
--                      this session, that it was machine-made, and open-ended
--                      per-run annotations. Only the relation (task_id) and the
--                      hot list filter (source) are dedicated columns; everything
--                      open-ended rides in metadata (design D7).

CREATE TABLE IF NOT EXISTS scheduled_task (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- optional team scope for credential resolution + shared visibility
    team_id            UUID REFERENCES teams(id) ON DELETE SET NULL,
    -- prompt source (design D1): exactly one of these is set. agent_def_name
    -- resolves system prompt + model from the agentdef store at fire time and
    -- prompt supplies its kickoff user turn; a standalone prompt is both the
    -- system-less run and its own user turn.
    agent_def_name     TEXT,
    prompt             TEXT,
    -- unattended permission (design D3): the run's loop is bound with exactly
    -- these tool names. Empty = a tool-free run.
    tool_whitelist     TEXT[] NOT NULL DEFAULT '{}',
    -- schedule: standard 5-field cron in the given IANA timezone
    cron               TEXT NOT NULL,
    timezone           TEXT NOT NULL DEFAULT 'UTC',
    -- session targeting (design D2): NULL = fresh session per fire; set = append
    target_session_id  UUID REFERENCES sessions(id) ON DELETE SET NULL,
    -- what to do with a freshly-created session after the run (keep|delete)
    on_run_completed   TEXT NOT NULL DEFAULT 'keep',
    -- behaviour when the target session already has an active run
    -- (reject|interrupt|enqueue)
    multitask_strategy TEXT NOT NULL DEFAULT 'reject',
    -- stop firing after this time; NULL = never expires
    end_time           TIMESTAMPTZ,
    enabled            BOOLEAN NOT NULL DEFAULT true,
    next_run_at        TIMESTAMPTZ NOT NULL,
    last_run_at        TIMESTAMPTZ,
    -- open-ended task config (future notification, retry policy, ...) (design D7)
    metadata           JSONB NOT NULL DEFAULT '{}',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- exactly one prompt source: either a standalone prompt (its own user turn,
    -- no system prompt), or an agent definition (system prompt + model) whose
    -- optional prompt is the kickoff user turn. agent_def_name never stands alone
    -- without prompt in this change's free-text form, but a bare agent reference
    -- IS allowed (kickoff defaults to a generic instruction at fire time).
    CONSTRAINT scheduled_task_prompt_source CHECK (
        prompt IS NOT NULL OR agent_def_name IS NOT NULL
    ),
    CONSTRAINT scheduled_task_on_run_completed CHECK (on_run_completed IN ('keep', 'delete')),
    CONSTRAINT scheduled_task_multitask CHECK (multitask_strategy IN ('reject', 'interrupt', 'enqueue'))
);

-- The due-scan hot path: find enabled tasks whose next_run_at has arrived.
CREATE INDEX IF NOT EXISTS idx_scheduled_task_due
    ON scheduled_task(enabled, next_run_at);
CREATE INDEX IF NOT EXISTS idx_scheduled_task_user ON scheduled_task(user_id);
CREATE INDEX IF NOT EXISTS idx_scheduled_task_team ON scheduled_task(team_id);

-- Tag sessions a task produces. task_id is the relation (join back to the task,
-- cascade-tag the output); source is the high-frequency list filter
-- (human|scheduled|subagent); metadata is the open-ended per-run bucket.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS task_id  UUID REFERENCES scheduled_task(id) ON DELETE SET NULL;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS source   TEXT NOT NULL DEFAULT 'human';
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';

CREATE INDEX IF NOT EXISTS idx_sessions_task ON sessions(task_id);
