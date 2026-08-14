-- Reverse of change durable-run-accounting: drop the per-step intent ledger
-- and the per-request usage ledger, in reverse order of creation.

DROP TABLE IF EXISTS usage_records;
DROP TABLE IF EXISTS run_steps;
