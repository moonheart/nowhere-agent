-- Runtime-settable platform settings (no restart to change): operator-facing
-- knobs that previously lived only in the environment (tool allowlists, the
-- global webhook target, the system-prompt language, rate limits) now default
-- from env at boot and can be overridden here — the admin console writes the
-- table, the settings runtime reloads the in-memory snapshot, and the change
-- takes effect on the next use without restarting the process.
--
-- Values are JSONB so each key carries its own shape (string, int, list).
-- Boot semantics: env provides the default; a row here wins.

CREATE TABLE platform_settings (
    key        TEXT PRIMARY KEY,
    value      JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
