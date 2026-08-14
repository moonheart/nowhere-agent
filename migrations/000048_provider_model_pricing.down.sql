ALTER TABLE provider_models
    DROP COLUMN IF EXISTS price_input_per_mtok,
    DROP COLUMN IF EXISTS price_output_per_mtok,
    DROP COLUMN IF EXISTS price_cache_read_per_mtok;
