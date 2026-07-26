ALTER TABLE runs
    DROP COLUMN IF EXISTS usage_input,
    DROP COLUMN IF EXISTS usage_output,
    DROP COLUMN IF EXISTS usage_cache_read,
    DROP COLUMN IF EXISTS usage_cache_write;

ALTER TABLE messages
    DROP COLUMN IF EXISTS usage_input,
    DROP COLUMN IF EXISTS usage_output,
    DROP COLUMN IF EXISTS usage_cache_read,
    DROP COLUMN IF EXISTS usage_cache_write;
