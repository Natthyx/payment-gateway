-- Tamper-evident audit trail for sensitive operations
-- (key rotation, refunds, merchant deactivation, etc).
--
-- Append-only via trigger so a compromised app cannot rewrite history.
CREATE TABLE IF NOT EXISTS audit_log (
    id           BIGSERIAL PRIMARY KEY,
    merchant_id  BIGINT,             -- nullable: some events are system-level
    actor        TEXT NOT NULL,      -- "merchant:42" | "system:reconciler" | "admin:alice"
    action       TEXT NOT NULL,      -- "api_key.rotate" | "refund.create" | "payment.succeeded"
    target       TEXT,               -- "payment_intent:123" | "merchant:42"
    ip           INET,
    user_agent   TEXT,
    request_id   TEXT,
    details      JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_audit_merchant ON audit_log (merchant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_action   ON audit_log (action, created_at DESC);

CREATE OR REPLACE FUNCTION prevent_audit_modification()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only. Modifications are not permitted.';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_no_update_audit ON audit_log;
CREATE TRIGGER trg_no_update_audit
    BEFORE UPDATE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION prevent_audit_modification();

DROP TRIGGER IF EXISTS trg_no_delete_audit ON audit_log;
CREATE TRIGGER trg_no_delete_audit
    BEFORE DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION prevent_audit_modification();
