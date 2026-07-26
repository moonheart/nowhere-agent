-- Token usage accounting (capability: usage persistence). Two granularities:
--   messages.usage_*  — per LLM call, set on the assistant message that call
--                       produced (one assistant message == one LLM call). NULL
--                       on user/tool_result rows, which are not LLM responses.
--   runs.usage_*      — the run's aggregate (sum of its LLM calls), redundant
--                       with SUM(messages) but cheap to query without a join.
-- cache_read = prompt-prefix cache hits (DeepSeek prompt_cache_hit_tokens /
-- OpenAI cached_tokens / Anthropic cache_read_input_tokens); cache_write is
-- Anthropic-only (OpenAI/DeepSeek use an automatic cache, no explicit write).

ALTER TABLE messages
    ADD COLUMN IF NOT EXISTS usage_input       INT,
    ADD COLUMN IF NOT EXISTS usage_output      INT,
    ADD COLUMN IF NOT EXISTS usage_cache_read  INT,
    ADD COLUMN IF NOT EXISTS usage_cache_write INT;

ALTER TABLE runs
    ADD COLUMN IF NOT EXISTS usage_input       INT,
    ADD COLUMN IF NOT EXISTS usage_output      INT,
    ADD COLUMN IF NOT EXISTS usage_cache_read  INT,
    ADD COLUMN IF NOT EXISTS usage_cache_write INT;
