-- Drop the suspended-batch snapshot. Interactions pending at downgrade time
-- keep their rows; a downgraded binary falls back to the legacy history scan
-- for them (their snapshots disappear with the table).

DROP TABLE IF EXISTS suspended_batches;
