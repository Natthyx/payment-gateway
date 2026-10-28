-- Inbox retries were picked up again immediately, so all 5 attempts ran
-- within milliseconds. The worker now only claims rows whose next attempt
-- is due, and pushes next_attempt_at out with exponential backoff on failure.
ALTER TABLE webhooks
    ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

CREATE INDEX IF NOT EXISTS idx_webhooks_due
    ON webhooks (next_attempt_at)
    WHERE status = 'pending';
