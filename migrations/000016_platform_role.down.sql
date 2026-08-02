DROP INDEX IF EXISTS idx_users_platform_role;

ALTER TABLE users
    DROP COLUMN IF EXISTS platform_role,
    DROP COLUMN IF EXISTS disabled_at;
