-- Skill enable/disable + move-to-team (skill-console): an enabled flag that
-- takes a skill out of agent resolution without deleting it.
--
-- `enabled` gates ONLY the agent's progressive-disclosure reads (PGStore.Get /
-- List) — a disabled skill drops out of the L0 index and L1/L2 loads, so the
-- model stops seeing it — but it stays fully visible and editable in the
-- management surface (ListByScope / ByID), so it can be reviewed, edited, and
-- re-enabled. Disabling is the reversible alternative to Delete.
ALTER TABLE skills ADD COLUMN IF NOT EXISTS enabled BOOLEAN NOT NULL DEFAULT true;
