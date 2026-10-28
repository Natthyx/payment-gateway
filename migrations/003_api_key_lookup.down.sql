DROP INDEX IF EXISTS uq_merchants_api_key_lookup;
ALTER TABLE merchants DROP COLUMN IF EXISTS api_key_lookup;
