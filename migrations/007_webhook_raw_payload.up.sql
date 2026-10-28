-- Keep the exact bytes the provider sent.
--
-- `payload` is JSONB, and Postgres normalises JSONB (key order, whitespace),
-- so an HMAC over the bytes read back never matches a signature computed over
-- the original body (Stripe, Chapa). The worker verifies against raw_payload;
-- rows written before this migration have NULL and fall back to payload.
ALTER TABLE webhooks
    ADD COLUMN IF NOT EXISTS raw_payload BYTEA;
