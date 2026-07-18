-- Long-term memory (design D5): scoped, read/write-split. The agent loop
-- reads online via Recall; the dreaming worker is the only writer.
-- The embedding column type adapts to whether pgvector is installed:
--   - pgvector present → native `vector` column (+ ivfflat index below)
--   - plain Postgres   → `jsonb` array (recall falls back to keyword match)

DO $$
DECLARE
    has_vector boolean;
BEGIN
    SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') INTO has_vector;

    IF has_vector THEN
        CREATE TABLE IF NOT EXISTS memories (
            id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
            -- user | team | system
            scope      TEXT NOT NULL,
            -- scope owner ids are application-level identifiers (not FK'd
            -- rows), so they are stored as text
            user_id    TEXT,
            team_id    TEXT,
            -- fact | preference | insight | summary
            kind       TEXT NOT NULL,
            content    TEXT NOT NULL,
            embedding  vector,
            deprecated BOOLEAN NOT NULL DEFAULT false,
            created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
            updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
        );
    ELSE
        CREATE TABLE IF NOT EXISTS memories (
            id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
            scope      TEXT NOT NULL,
            user_id    TEXT,
            team_id    TEXT,
            kind       TEXT NOT NULL,
            content    TEXT NOT NULL,
            embedding  JSONB,
            deprecated BOOLEAN NOT NULL DEFAULT false,
            created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
            updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
        );
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_memories_scope_user ON memories(scope, user_id);
CREATE INDEX IF NOT EXISTS idx_memories_scope_team ON memories(scope, team_id);
CREATE INDEX IF NOT EXISTS idx_memories_content_fts ON memories USING gin (to_tsvector('simple', content));

-- Recall within a scope is small (per user/team), so exact cosine search over
-- the scoped rows is sufficient; no approximate (ivfflat/hnsw) index is added.
-- An ANN index would also force a fixed embedding dimension, while different
-- embedding providers produce different dimensions — the column stays
-- dimension-agnostic on purpose.
