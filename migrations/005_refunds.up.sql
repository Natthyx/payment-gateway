CREATE TABLE IF NOT EXISTS refunds (
    id                 BIGSERIAL PRIMARY KEY,
    payment_intent_id  BIGINT      NOT NULL REFERENCES payment_intents(id),
    merchant_id        BIGINT      NOT NULL REFERENCES merchants(id),
    idempotency_key    TEXT        NOT NULL,
    amount             BIGINT      NOT NULL CHECK (amount > 0),
    currency           TEXT        NOT NULL,
    reason             TEXT,
    status             TEXT        NOT NULL DEFAULT 'pending'
                                   CHECK (status IN ('pending','succeeded','failed')),
    provider_refund_id TEXT,
    failure_reason     TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (merchant_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_refunds_payment ON refunds (payment_intent_id);

CREATE TRIGGER trg_refunds_updated_at
    BEFORE UPDATE ON refunds
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
