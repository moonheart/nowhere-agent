-- Conversation retention (conversation_retention_days): the hourly sweep
-- lists ENDED sessions whose ended_at predates the cutoff with a keyset scan
-- ordered by (ended_at, id) (ListEndedSessionsEndedBefore). That query filters
-- status and sorts by ended_at; without a matching index every sweep pass
-- scans and sorts the whole ended set. The composite (status, ended_at, id)
-- index serves the filter, the sort, and the keyset cursor in one bounded
-- range scan. The workspace image retention sweep runs the same query, so it
-- benefits too.

CREATE INDEX IF NOT EXISTS idx_sessions_ended_ended_at
    ON sessions (status, ended_at, id);
