-- Failed-run visibility (change failed-run-retry): messages gain an optional
-- metadata JSONB column carrying per-message extras beyond the block content.
-- Currently used to attach a failed run's terminal error text to its last
-- assistant message ({error: "..."}), so a reloaded client can show why the run
-- stopped and offer a retry. NULL on rows without metadata; history rebuild
-- only echoes the error key back to the client.

ALTER TABLE messages ADD COLUMN IF NOT EXISTS metadata JSONB;
