-- General interrupt (capability generic-interrupt): the approvals table becomes
-- the durable record of ANY client interaction a run suspends on, not just a
-- permission approval or ask_user. A third kind — 'client_tool' (a tool the
-- client executes, not the server) — joins 'approval' and 'ask_user'. The kind
-- column was already an open TEXT, so no schema change admits the new kind;
-- this migration documents the widened semantics and the field generalization.
--
-- Field semantics (Go struct renames; the COLUMNS are unchanged):
--   tool_input → Payload  (what the client is shown: gated args / question set /
--                          client-tool input)
--   answer     → Result   (what the client returns: ask_user answers / client-tool
--                          output-or-error; NULL for a permission approval)
--   kind       → open string interpreted by a registered InteractionHandler.
--
-- The one-pending-per-session partial unique index (000011) is unchanged: a run
-- still carries at most one outstanding interaction per session.

-- No DDL required: kind is already open TEXT and existing rows stay valid. The
-- statements below are idempotent no-ops that pin the documented semantics.

COMMENT ON TABLE approvals IS 'Durable record of a run suspended waiting on a client interaction (general interrupt). kind: approval | ask_user | client_tool | ...';
COMMENT ON COLUMN approvals.tool_input IS 'Payload shown to the client (gated args / question set / client-tool input).';
COMMENT ON COLUMN approvals.answer IS 'Result returned by the client (ask_user answers / client-tool output-or-error); NULL for a permission approval.';
COMMENT ON COLUMN approvals.kind IS 'Open interaction kind, interpreted by a registered InteractionHandler.';
