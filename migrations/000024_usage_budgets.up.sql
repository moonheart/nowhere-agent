-- Usage-budget enforcement (enterprise-readiness P1-1): a monthly token budget
-- per account and per team. Until now usage was recorded but never enforced —
-- nothing stopped one account from burning through a shared team key. A budget
-- row makes the limit explicit; the gate checks it at run submit and rejects
-- with 429 once the month's spend reaches it.

-- One row per scope. scope is 'user' or 'team'; owner_id is the account or team
-- UUID (kept as TEXT with no FK, so deleting the account/team neither cascades
-- nor orphans a constraint — the row simply stops matching any live owner).
-- monthly_tokens is the budget for a calendar month, measured on billable
-- tokens (usage_input + usage_output, the pair providers price). NULL would
-- mean "no limit"; the column is NOT NULL instead so an unset budget is the
-- ABSENCE of a row, never an ambiguous NULL to misread as zero-or-unlimited.
CREATE TABLE IF NOT EXISTS usage_budgets (
    scope          TEXT NOT NULL CHECK (scope IN ('user', 'team')),
    owner_id       TEXT NOT NULL,
    monthly_tokens BIGINT NOT NULL CHECK (monthly_tokens > 0),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, owner_id)
);

-- Lookups are by (scope, owner_id) — the primary key covers it; no extra index.
