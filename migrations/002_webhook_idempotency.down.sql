DROP INDEX IF EXISTS uq_ledger_payment_account;
DROP INDEX IF EXISTS uq_webhooks_provider_event;
ALTER TABLE webhooks DROP COLUMN IF EXISTS provider_event_id;
