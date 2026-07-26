-- Incremental memory injection (capability K / context-mgmt): a per-session
-- watermark recording when memories were last injected into the conversation.
-- The injector surfaces only memories created AFTER this watermark (created_at
-- > memory_injected_at), so the durable history stays append-only and the LLM
-- prompt prefix stays byte-stable for caching. Also add a created_at index on
-- memories for the incremental recall (000005 only indexed scope + FTS).

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS memory_injected_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_memories_created_at ON memories (created_at);
