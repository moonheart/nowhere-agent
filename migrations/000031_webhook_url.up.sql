-- Outbound run-completion notifications (enterprise integration): a scheduled
-- task may name a webhook URL that receives a POST when one of its runs reaches
-- a terminal state. NULL keeps the task silent (or falls back to the global
-- WEBHOOK_URL, when configured).

ALTER TABLE scheduled_task ADD COLUMN webhook_url TEXT;
