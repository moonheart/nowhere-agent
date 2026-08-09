-- User-level image uploads (change user-image-uploads): images uploaded
-- independently of any session, so the first message of a chat can carry an
-- image. The WebP blob lives in the workspace store under
-- <root>/__uploads__/<user_id>/<id>.webp; this table is the metadata index —
-- listing, cleanup, and reference protection all read it. Messages reference an
-- upload by path "uploads/<id>.webp"; deletion is rejected while any stored
-- message still references the id (the check lives in the service, not here,
-- because message content is a JSON document rather than a normalized relation).

CREATE TABLE IF NOT EXISTS uploads (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    filename   TEXT NOT NULL,
    size       BIGINT NOT NULL,
    media_type TEXT NOT NULL DEFAULT 'image/webp',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The per-user listing (newest first) is the console's primary access pattern.
CREATE INDEX IF NOT EXISTS idx_uploads_user_created ON uploads(user_id, created_at DESC);
