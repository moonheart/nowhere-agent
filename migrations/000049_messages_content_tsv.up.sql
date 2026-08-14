-- Message-content search for session listing (internal/session/pgstore.go
-- ListSessionsByUser): a session is returned by the sidebar search when q
-- matches its title OR the text of any of its messages. messages.content is a
-- JSONB block array (provider.Block), so the search vector is extracted from
-- the free-text fields only — text, thinking, and tool_content (including
-- nested tool_messages via recursive descent) — never the JSON keys or tool
-- input payloads. The 'simple' config (no stemming/stopwords, matching the
-- memories index in 000005) tokenizes as written, so verbatim words like
-- "sessions" are findable. GENERATED ALWAYS ... STORED keeps the column
-- immutable; the GIN index serves the @@ match.
--
-- NOTE: the jsonb_path_query_array expression is IMMUTABLE only because it
-- never references any volatile function and its arguments are constants
-- plus the content column — Postgres validates this at ALTER time and the
-- migration fails loudly (rather than silently) if a future PG version
-- disagrees.

ALTER TABLE messages
    ADD COLUMN IF NOT EXISTS content_tsv tsvector
    GENERATED ALWAYS AS (
        to_tsvector('simple',
            (jsonb_path_query_array(content, '$.**.text')
             || jsonb_path_query_array(content, '$.**.thinking')
             || jsonb_path_query_array(content, '$.**.tool_content'))::text)
    ) STORED;

CREATE INDEX IF NOT EXISTS idx_messages_content_tsv ON messages USING gin (content_tsv);
