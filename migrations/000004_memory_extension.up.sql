-- Enable the vector extension when available (pgvector image). On a plain
-- Postgres image the extension isn't installed; catch that and continue so
-- the memories table can still be created with a jsonb embedding column.
DO $$
BEGIN
    CREATE EXTENSION IF NOT EXISTS vector;
EXCEPTION
    WHEN OTHERS THEN
        RAISE NOTICE 'pgvector extension unavailable; embeddings will be unindexed';
END $$;
