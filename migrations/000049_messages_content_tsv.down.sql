-- Message-content search vector for session listing (see the up migration):
-- a generated column, so dropping the index and column restores the prior
-- schema. Dropping the index first avoids a dependency error.

DROP INDEX IF EXISTS idx_messages_content_tsv;
ALTER TABLE messages DROP COLUMN IF EXISTS content_tsv;
