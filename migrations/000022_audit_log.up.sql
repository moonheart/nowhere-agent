-- Audit log (enterprise-readiness P0): an append-only record of who did what,
-- when, from where, and whether it succeeded. This is the platform's compliance
-- trail — authentication events, administrative actions, and credential changes
-- land here so a security review can reconstruct the sequence of events.
--
-- The table is write-only from the application's perspective: rows are INSERTed
-- and never UPDATEd or DELETEd by app code (retention, if any, is a DBA concern
-- handled out-of-band). actor_id is nullable and carries no foreign key so a
-- deleted account does not take its audit history with it — the trail must
-- outlive the actor.

CREATE TABLE IF NOT EXISTS audit_log (
    id          BIGSERIAL PRIMARY KEY,
    -- when
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- who: the authenticated actor. NULL for anonymous events (a failed login
    -- has no resolvable user) and for actors since deleted. actor_email is a
    -- denormalized snapshot so the trail stays readable after account deletion.
    actor_id    TEXT,
    actor_email TEXT,
    -- what: a dotted action (auth.login, admin.user.create, team.key.rotate…)
    -- and its outcome. action is indexed; the full vocabulary lives in
    -- internal/audit.
    action      TEXT NOT NULL,
    outcome     TEXT NOT NULL DEFAULT 'success',  -- success | failure
    -- on what: the target entity, when one applies (the user created, the team
    -- whose key rotated). Free-form type + id; no FK for the same reason as actor.
    target_type TEXT,
    target_id   TEXT,
    -- from where / how: client connection metadata. ua is the User-Agent.
    ip          TEXT,
    ua          TEXT,
    -- extra: small action-specific payload (e.g. the role a member was granted).
    -- NEVER secrets, tokens, or passwords — call sites must keep detail thin.
    detail      JSONB NOT NULL DEFAULT '{}'::jsonb
);

-- The two access patterns: "recent events, newest first" and "everything for an
-- action / actor". Both are append-ordered scans, so a created_at index covers
-- the primary one; action + actor_id support filtered reviews.
CREATE INDEX IF NOT EXISTS idx_audit_log_created_at ON audit_log(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_log_action     ON audit_log(action);
CREATE INDEX IF NOT EXISTS idx_audit_log_actor      ON audit_log(actor_id);
