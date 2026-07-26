-- Dreaming incremental (capability-gap K1, take 2): replace the boolean
-- "has this session been dreamed" marker with a monotonically increasing
-- watermark. dreamed_seq = the messages.id the worker has consolidated up to,
-- so the worker can dream a session INCREMENTALLY — learn from the new messages
-- since the last pass while the conversation stays open and resumable, instead
-- of waiting for the session to end (and becoming un-chat-able) before learning.
-- A session is eligible when it has messages with id > COALESCE(dreamed_seq, 0);
-- that includes active sessions, so learning no longer requires ending the chat.

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS dreamed_seq BIGINT NOT NULL DEFAULT 0;

-- Swap the eligibility index from the ended+boolean form (migration 000008) to
-- the watermark form: a session needs dreaming while it still has undreamed
-- messages. (The end-status predicate is gone — open sessions are learnable.)
DROP INDEX IF EXISTS idx_sessions_ended_undreamed;
CREATE INDEX IF NOT EXISTS idx_sessions_undreamed ON sessions (dreamed_seq);
