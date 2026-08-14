-- Per-model pricing (usage cost reporting): the usage report is tokens-only
-- because turning tokens into money needs per-model prices, and none were
-- recorded. These three optional columns hold the price in USD per MILLION
-- tokens for each billable counter (input, output, cache-read) — the units the
-- LLM providers quote. NULL = unpriced: cost estimation counts it as zero and
-- the admin console shows no price. Prices are config, not accounting: editing
-- a row does not rewrite historical runs; the report joins runs.model to the
-- CURRENT price, so backdated price changes revalue history (a documented
-- approximation — runs record the model name only, not the provider, so two
-- providers sharing a model name use the first matching price row).

ALTER TABLE provider_models
    ADD COLUMN IF NOT EXISTS price_input_per_mtok     DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS price_output_per_mtok    DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS price_cache_read_per_mtok DOUBLE PRECISION;
