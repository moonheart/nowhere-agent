-- Composite index for keyset pagination of the sidebar conversation list:
-- WHERE user_id / status filter plus ORDER BY updated_at DESC, id DESC
-- (id breaks ties between sessions updated in the same instant). The old
-- per-column indexes still serve the other status/user-only scans.

CREATE INDEX IF NOT EXISTS idx_sessions_user_status_updated
    ON sessions (user_id, status, updated_at DESC, id DESC);
