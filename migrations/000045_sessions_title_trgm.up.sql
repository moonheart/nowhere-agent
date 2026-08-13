-- Session title search (internal/session/pgstore.go ListSessionsByUser):
-- the query uses a leading wildcard (title ILIKE '%term%'), so a plain btree
-- index on title cannot serve it. A trigram GIN index can, with zero query
-- change. ILIKE over a low-cardinality title set is still a sequential scan
-- when the planner estimates it cheaper; for the user-title sets this serves
-- that is the right trade — the index exists for the search-heavy cases.

CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX IF NOT EXISTS idx_sessions_title_trgm ON sessions USING gin (title gin_trgm_ops);
