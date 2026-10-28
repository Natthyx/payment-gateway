-- Merchant-configured webhook endpoints
CREATE TABLE IF NOT EXISTS merchant_webhook_endpoints (
    id           BIGSERIAL PRIMARY KEY,
    merchant_id  BIGINT      NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,
    url          TEXT        NOT NULL,
    is_active    BOOLEAN     NOT NULL DEFAULT true,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (merchant_id, url)
);

CREATE INDEX IF NOT EXISTS idx_mwe_merchant ON merchant_webhook_endpoints (merchant_id)
    WHERE is_active = true;

CREATE TRIGGER trg_mwe_updated_at
    BEFORE UPDATE ON merchant_webhook_endpoints
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Delivery queue for outbound events. The worker drains this with the same
-- FOR UPDATE SKIP LOCKED pattern as the inbox.
CREATE TABLE IF NOT EXISTS outbound_webhook_deliveries (
    id            BIGSERIAL PRIMARY KEY,
    endpoint_id   BIGINT      NOT NULL REFERENCES merchant_webhook_endpoints(id) ON DELETE CASCADE,
    merchant_id   BIGINT      NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,
    event_type    TEXT        NOT NULL,        -- "payment.succeeded" | "refund.created" | ...
    payload       JSONB       NOT NULL,
    status        TEXT        NOT NULL DEFAULT 'pending'
                              CHECK (status IN ('pending','delivered','failed','dead')),
    attempts      INT         NOT NULL DEFAULT 0,
    last_status   INT,                          -- last HTTP status seen
    last_error    TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_owd_due
    ON outbound_webhook_deliveries (status, next_attempt_at)
    WHERE status = 'pending';
