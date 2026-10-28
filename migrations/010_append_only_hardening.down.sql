ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM gateway_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE USAGE, SELECT ON SEQUENCES FROM gateway_app;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM gateway_app;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM gateway_app;
REVOKE USAGE ON SCHEMA public FROM gateway_app;
DROP TRIGGER IF EXISTS trg_no_truncate_audit ON audit_log;
DROP TRIGGER IF EXISTS trg_no_truncate_ledger ON ledger_entries;
DROP FUNCTION IF EXISTS prevent_truncate();
-- The gateway_app role itself is left in place: login users created by the
-- deployment may still be members of it.
