-- Audit log down: drop the trail and its indexes. This destroys compliance
-- history; it exists for symmetry and local dev resets, not for production use.
DROP TABLE IF EXISTS audit_log;
