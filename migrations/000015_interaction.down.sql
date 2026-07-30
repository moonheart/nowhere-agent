-- Down for 000015_interaction: drop the documentation comments. No schema change
-- was made by the up migration, so nothing structural to revert.

COMMENT ON TABLE approvals IS NULL;
COMMENT ON COLUMN approvals.tool_input IS NULL;
COMMENT ON COLUMN approvals.answer IS NULL;
COMMENT ON COLUMN approvals.kind IS NULL;
