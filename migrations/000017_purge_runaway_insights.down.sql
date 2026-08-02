-- Irreversible by nature: the up migration deletes rows, and deleted rows
-- cannot be reconstructed. This is a deliberate no-op rather than a missing
-- file, so `migrate down` past this point succeeds instead of failing on an
-- absent script — and so the irreversibility is stated where someone rolling
-- back will read it, not discovered afterwards.
--
-- Rolling back the CODE is safe and restores the previous consolidation
-- behaviour; it simply starts from a store with no insights, which is where the
-- new behaviour would have brought it anyway.
SELECT 1;
