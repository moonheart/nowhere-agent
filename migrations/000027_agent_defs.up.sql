-- Agent definitions (change persist-agent-defs): durable, scoped subagent
-- type definitions (markdown frontmatter + system-prompt body) across
-- user/team/system scopes, versioned like skills.
--
-- Two tables mirror the skill versioning model (migration 000019):
--
--   agent_defs          — one row per (name, scope, owner); the
--                         current_version pointer. Scope-owner ids are
--                         application-level identifiers, so TEXT, not FK'd
--                         rows (same idiom as skills/memories).
--   agent_def_versions  — one immutable row per saved revision. A save
--                         appends a version and bumps the pointer, so
--                         history is never rewritten.

CREATE TABLE IF NOT EXISTS agent_defs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL,
    -- user | team | system
    scope           TEXT NOT NULL,
    -- scope owner ids (exactly one set for user/team; neither for system)
    user_id         TEXT,
    team_id         TEXT,
    current_version INT  NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Identity is (name, scope, owner): one definition of a given name per
-- scope+owner. COALESCE collapses the NULL owner columns so system
-- definitions (both NULL) also dedupe on name alone.
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_defs_identity
    ON agent_defs(name, scope, COALESCE(user_id, ''), COALESCE(team_id, ''));

-- Resolution walks scopes user > team > system, so per-owner lookups dominate.
CREATE INDEX IF NOT EXISTS idx_agent_defs_scope_user ON agent_defs(scope, user_id);
CREATE INDEX IF NOT EXISTS idx_agent_defs_scope_team ON agent_defs(scope, team_id);

CREATE TABLE IF NOT EXISTS agent_def_versions (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    def_id           UUID NOT NULL REFERENCES agent_defs(id) ON DELETE CASCADE,
    version          INT  NOT NULL,
    -- frontmatter `description`: the one-line when-to-use shown to the model
    when_to_use      TEXT NOT NULL DEFAULT '',
    -- tool allow/deny lists and declared skills (NULL/empty = inherit/deny-none)
    tools            JSONB NOT NULL DEFAULT '[]',
    disallowed_tools JSONB NOT NULL DEFAULT '[]',
    skills           JSONB NOT NULL DEFAULT '[]',
    model            TEXT NOT NULL DEFAULT '',
    max_turns        INT  NOT NULL DEFAULT 0,
    -- the document body: the child loop's system prompt
    system           TEXT NOT NULL DEFAULT '',
    -- the source markdown document, retained for the editor
    raw_document     TEXT NOT NULL DEFAULT '',
    created_by       TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(def_id, version)
);

-- Version history reads newest-first for a single definition.
CREATE INDEX IF NOT EXISTS idx_agent_def_versions_def
    ON agent_def_versions(def_id, version DESC);
