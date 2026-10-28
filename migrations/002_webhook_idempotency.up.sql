-- Webhook provider-event idempotency.
--
-- Adds a provider_event_id column so a re-delivered webhook from Stripe/Chapa/Telebirr
-- is stored exactly once. Combined with INSERT ... ON CONFLICT DO NOTHING in the
-- ingest path, this gives us at-most-once processing per provider event.
--
-- provider_event_id is best-effort: each provider extracts a stable key from the
-- payload (Stripe: event.id; Chapa: tx_ref:status; Telebirr: out_trade_no:timestamp).
-- For payloads we cannot fingerprint we fall back to a SHA-256 of the raw body.
ALTER TABLE webhooks
    ADD COLUMN IF NOT EXISTS provider_event_id TEXT;

-- Partial unique index so historical NULL rows (pre-migration) don't conflict.
CREATE UNIQUE INDEX IF NOT EXISTS uq_webhooks_provider_event
    ON webhooks (provider, provider_event_id)
    WHERE provider_event_id IS NOT NULL;

-- Ledger idempotency at the row level: at most one entry per (payment_intent, account).
-- Prevents duplicate ledger writes if a webhook is somehow processed twice
-- and the provider_event_id dedupe is bypassed (defense in depth).
CREATE UNIQUE INDEX IF NOT EXISTS uq_ledger_payment_account
    ON ledger_entries (payment_intent_id, account);
