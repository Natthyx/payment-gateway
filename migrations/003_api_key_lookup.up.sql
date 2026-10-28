-- API key authentication: split-key lookup so authenticate becomes O(1).
--
-- Old scheme: gw_live_<32 random bytes hex>, bcrypt over the whole key,
--             auth required scanning every merchant row.
-- New scheme: gw_live_<lookup_16hex>_<secret_48hex>
--             lookup is indexed; bcrypt is computed against secret only.
--
-- All historical rows have a NULL api_key_lookup until the merchant rotates.
-- After rotation, api_key_lookup is set to a 16-char lookup that uniquely
-- identifies the row, and api_key_hash holds bcrypt(secret).
ALTER TABLE merchants
    ADD COLUMN IF NOT EXISTS api_key_lookup TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS uq_merchants_api_key_lookup
    ON merchants (api_key_lookup)
    WHERE api_key_lookup IS NOT NULL;
