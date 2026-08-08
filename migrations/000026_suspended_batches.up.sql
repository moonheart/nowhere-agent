-- Suspended batch snapshot (capability suspend-batch-snapshot): when the
-- interaction gate suspends a tool batch, record the batch's identity — the
-- run, the suspending assistant message, and the full ordered tool_call ID set
-- (gated AND ungated siblings) — so a later fold resolves the batch from this
-- snapshot instead of re-deriving it from a session-wide history scan. The
-- snapshot is written in the same transaction as the batch's first interaction
-- row (CreateInteractionBatch), so the suspension is bound into durable state.

CREATE TABLE IF NOT EXISTS suspended_batches (
    run_id        UUID PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    session_id    UUID NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    -- seq of the suspending assistant message in messages; NULL until known
    -- (the fold locates the message by run_id, seq is informational).
    message_seq   INT,
    -- full batch in assistant-message block order — the fold's answer key
    tool_call_ids JSONB NOT NULL,
    -- messages.seq of the folded tool_result message; NULL = not folded yet
    folded_seq    INT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Backfill: every run with pending interactions gets a snapshot from its own
-- last tool_use-bearing assistant message, so interactions parked before this
-- migration still fold under the new snapshot-driven path.
INSERT INTO suspended_batches (run_id, session_id, message_seq, tool_call_ids)
SELECT a.run_id, a.session_id, m.seq, ids.tool_call_ids
FROM (SELECT DISTINCT run_id, session_id FROM approvals WHERE status = 'pending') a
JOIN LATERAL (
    SELECT seq, content
    FROM messages
    WHERE run_id = a.run_id AND role = 'assistant'
      AND EXISTS (
          SELECT 1 FROM jsonb_array_elements(content) b
          WHERE b ->> 'type' = 'tool_use' AND b ->> 'tool_use_id' IS NOT NULL
      )
    ORDER BY seq DESC
    LIMIT 1
) m ON true
CROSS JOIN LATERAL (
    SELECT jsonb_agg(b ->> 'tool_use_id' ORDER BY ord) AS tool_call_ids
    FROM jsonb_array_elements(m.content) WITH ORDINALITY AS t(b, ord)
    WHERE b ->> 'type' = 'tool_use' AND b ->> 'tool_use_id' IS NOT NULL
) ids
ON CONFLICT (run_id) DO NOTHING;

-- Orphans: a pending interaction whose run has no tool_use-bearing assistant
-- message can never fold under any semantics. Reject it so it cannot block the
-- submission gate forever.
UPDATE approvals
SET status = 'rejected', decided_at = now()
WHERE status = 'pending'
  AND run_id NOT IN (SELECT run_id FROM suspended_batches);
