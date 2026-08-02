-- Platform role and account disablement (admin-console). Two orthogonal
-- concepts land on `users`:
--
--   platform_role — administers the PLATFORM (users, teams, all scopes). This
--                   is distinct from team_memberships.role, which governs a
--                   single team's resources. An account can be a platform
--                   admin while belonging to no team at all.
--   disabled_at   — the account exists and keeps its data, but cannot
--                   authenticate. Disabling also revokes outstanding tokens
--                   (application layer), so live sessions do not survive it.
--
-- The first account created on an empty platform is made admin by the
-- application (serialized under a transaction advisory lock); deployments whose
-- accounts predate this migration designate one via BOOTSTRAP_ADMIN_EMAIL.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS platform_role TEXT NOT NULL DEFAULT 'user',
    ADD COLUMN IF NOT EXISTS disabled_at   TIMESTAMPTZ;

-- Partial index: "who are the admins" is the only query on this column, and
-- 'user' is the overwhelmingly common value — indexing it would be dead weight.
CREATE INDEX IF NOT EXISTS idx_users_platform_role
    ON users(platform_role) WHERE platform_role = 'admin';
