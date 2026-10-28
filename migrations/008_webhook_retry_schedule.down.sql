DROP INDEX IF EXISTS idx_webhooks_due;
ALTER TABLE webhooks DROP COLUMN IF EXISTS next_attempt_at;
