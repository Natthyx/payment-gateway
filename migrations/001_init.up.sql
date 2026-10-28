CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ── MERCHANTS ────────────────────────────────────────────────────────────────
-- A merchant is any business using our gateway (like a Stripe "account")
CREATE TABLE merchants (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT        NOT NULL,
    email        TEXT        NOT NULL UNIQUE,
    api_key_hash TEXT        NOT NULL,        -- bcrypt hash, never store plaintext
    is_active    BOOLEAN     NOT NULL DEFAULT true,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── PAYMENT INTENTS ──────────────────────────────────────────────────────────
-- A PaymentIntent represents one attempt to collect money.
-- Inspired directly by Stripe's PaymentIntent object.
CREATE TABLE payment_intents (
    id               BIGSERIAL PRIMARY KEY,
    merchant_id      BIGINT      NOT NULL REFERENCES merchants(id),
    idempotency_key  TEXT        NOT NULL,    -- prevents duplicate charges
    amount           BIGINT      NOT NULL CHECK (amount > 0),  -- always in smallest unit (cents/santim)
    currency         TEXT        NOT NULL,    -- "USD", "ETB"
    provider         TEXT        NOT NULL CHECK (provider IN ('stripe', 'chapa', 'telebirr')),
    status           TEXT        NOT NULL DEFAULT 'created'
                                 CHECK (status IN ('created','processing','succeeded','failed','refunded')),
    provider_tx_id   TEXT,                   -- ID returned by Stripe/Chapa/Telebirr
    failure_reason   TEXT,
    metadata         JSONB,                  -- arbitrary key-value for merchant use
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (merchant_id, idempotency_key)    -- core idempotency constraint
);

CREATE INDEX idx_pi_merchant_id  ON payment_intents (merchant_id);
CREATE INDEX idx_pi_status       ON payment_intents (status);
CREATE INDEX idx_pi_provider_tx  ON payment_intents (provider_tx_id);

-- ── LEDGER ───────────────────────────────────────────────────────────────────
-- Double-entry bookkeeping. Every payment creates TWO entries.
-- They must always net to zero. If they don't — money was created or lost.
-- This is how every bank and financial system on earth works.
CREATE TABLE ledger_entries (
    id                BIGSERIAL PRIMARY KEY,
    payment_intent_id BIGINT      NOT NULL REFERENCES payment_intents(id),
    account           TEXT        NOT NULL,  -- e.g. "merchant:42", "gateway:fees", "provider:stripe"
    amount            BIGINT      NOT NULL,  -- positive = credit, negative = debit
    currency          TEXT        NOT NULL,
    entry_type        TEXT        NOT NULL CHECK (entry_type IN ('debit', 'credit')),
    description       TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
    -- append-only: no updates or deletes ever
);

CREATE INDEX idx_ledger_payment_intent ON ledger_entries (payment_intent_id);
CREATE INDEX idx_ledger_account        ON ledger_entries (account);

-- ── WEBHOOKS ─────────────────────────────────────────────────────────────────
-- Every incoming webhook is stored immediately before any processing.
-- WHY: if processing crashes, we can replay from this table.
-- This is the "inbox pattern" — store first, process async.
CREATE TABLE webhooks (
    id           BIGSERIAL PRIMARY KEY,
    provider     TEXT        NOT NULL,
    event_type   TEXT        NOT NULL,       -- e.g. "payment_intent.succeeded"
    payload      JSONB       NOT NULL,       -- raw webhook body
    signature    TEXT        NOT NULL,       -- for audit trail
    status       TEXT        NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending', 'processed', 'failed')),
    attempts     INT         NOT NULL DEFAULT 0,
    last_error   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ
);

CREATE INDEX idx_webhooks_status   ON webhooks (status);
CREATE INDEX idx_webhooks_provider ON webhooks (provider);

-- ── APPEND-ONLY PROTECTION ON LEDGER ────────────────────────────────────────
CREATE OR REPLACE FUNCTION prevent_ledger_modification()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only. Modifications are not permitted.';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_no_update_ledger
    BEFORE UPDATE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION prevent_ledger_modification();

CREATE TRIGGER trg_no_delete_ledger
    BEFORE DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION prevent_ledger_modification();

-- ── AUTO updated_at ──────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN NEW.updated_at = NOW(); RETURN NEW; END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_merchants_updated_at
    BEFORE UPDATE ON merchants
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER trg_pi_updated_at
    BEFORE UPDATE ON payment_intents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();