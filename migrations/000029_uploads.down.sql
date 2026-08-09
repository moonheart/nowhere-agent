-- User-level image uploads (change user-image-uploads): rolled back wholesale —
-- the blobs under the workspace store are left on disk (harmless orphans), only
-- the metadata index is dropped.

DROP TABLE IF EXISTS uploads;
