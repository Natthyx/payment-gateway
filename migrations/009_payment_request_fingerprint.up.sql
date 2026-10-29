-- Idempotency: remember what a key was first used for, so reusing the key
-- with different parameters is rejected instead of silently replayed, and
-- the original response can be replayed from Postgres when Redis has
-- forgotten it.
ALTER TABLE payment_intents
    ADD COLUMN IF NOT EXISTS request_fingerprint TEXT,
    ADD COLUMN IF NOT EXISTS checkout_url        TEXT;

-- Webhooks are matched to intents by (provider, provider_tx_id).
CREATE INDEX IF NOT EXISTS idx_pi_provider_provider_tx ON payment_intents (provider, provider_tx_id);
