-- Skills (design D7/D16): general-form capability packages (SKILL.md + L2
-- resources + L2 scripts) with progressive disclosure across user/team/system
-- scopes, versioned with override review.
--
-- Two tables separate the mutable pointer from the immutable history:
--
--   skills          — one row per (name, scope, owner); the current_version
--                     pointer plus override-review bookkeeping. Scope-owner ids
--                     (user_id/team_id) are application-level identifiers, so
--                     they are TEXT, not FK'd rows (same idiom as memories).
--   skill_versions  — one immutable row per saved revision. Editing a skill
--                     appends a new version and bumps current_version; rollback
--                     copies an old version's content into a NEW version, so
--                     history is never rewritten.

CREATE TABLE IF NOT EXISTS skills (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name              TEXT NOT NULL,
    -- user | team | system
    scope             TEXT NOT NULL,
    -- scope owner ids (exactly one set for user/team; neither for system)
    user_id           TEXT,
    team_id           TEXT,
    current_version   INT  NOT NULL DEFAULT 1,
    -- which version of the lower-scope skill this override was based on; when
    -- the upstream skill is updated past this, needs_review is set (D16)
    overrides_version INT  NOT NULL DEFAULT 0,
    needs_review      BOOLEAN NOT NULL DEFAULT false,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Identity is (name, scope, owner): one skill of a given name per scope+owner.
-- COALESCE collapses the NULL owner columns so system skills (both NULL) also
-- dedupe on name alone.
CREATE UNIQUE INDEX IF NOT EXISTS idx_skills_identity
    ON skills(name, scope, COALESCE(user_id, ''), COALESCE(team_id, ''));

-- Resolution walks scopes user > team > system, so per-owner lookups dominate.
CREATE INDEX IF NOT EXISTS idx_skills_scope_user ON skills(scope, user_id);
CREATE INDEX IF NOT EXISTS idx_skills_scope_team ON skills(scope, team_id);

CREATE TABLE IF NOT EXISTS skill_versions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    skill_id    UUID NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
    version     INT  NOT NULL,
    -- L0 one-liner shown in the always-resident index
    description TEXT NOT NULL DEFAULT '',
    -- L1 full SKILL.md instructions, loaded when the skill is selected
    body        TEXT NOT NULL DEFAULT '',
    -- L2 files referenced by the body, {path: content}
    resources   JSONB NOT NULL DEFAULT '{}',
    -- L2 executable entry points run in the sandbox, {path: content}
    scripts     JSONB NOT NULL DEFAULT '{}',
    created_by  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(skill_id, version)
);

-- Version history reads newest-first for a single skill.
CREATE INDEX IF NOT EXISTS idx_skill_versions_skill
    ON skill_versions(skill_id, version DESC);
