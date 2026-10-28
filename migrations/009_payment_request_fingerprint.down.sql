DROP INDEX IF EXISTS idx_pi_provider_provider_tx;
ALTER TABLE payment_intents DROP COLUMN IF EXISTS checkout_url, DROP COLUMN IF EXISTS request_fingerprint;
