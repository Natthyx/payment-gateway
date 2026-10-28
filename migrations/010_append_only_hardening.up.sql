-- Append-only hardening for ledger_entries and audit_log.
--
-- 1. Row-level triggers do not fire on TRUNCATE, so TRUNCATE emptied both
--    tables. Block it with statement-level triggers.
-- 2. The app used to connect as the table owner (a superuser in the default
--    docker-compose), which can disable triggers. Create a least-privilege
--    group role for the runtime connection: it can INSERT and SELECT the
--    append-only tables but never UPDATE, DELETE or TRUNCATE them, and as a
--    non-owner it cannot disable their triggers. Login users are created by
--    the deployment (see deploy/postgres/initdb) and granted this role.
CREATE OR REPLACE FUNCTION prevent_truncate()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only. TRUNCATE is not permitted.', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_no_truncate_ledger ON ledger_entries;
CREATE TRIGGER trg_no_truncate_ledger
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION prevent_truncate();

DROP TRIGGER IF EXISTS trg_no_truncate_audit ON audit_log;
CREATE TRIGGER trg_no_truncate_audit
    BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION prevent_truncate();

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gateway_app') THEN
        CREATE ROLE gateway_app NOLOGIN;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO gateway_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO gateway_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO gateway_app;
REVOKE UPDATE, DELETE, TRUNCATE ON ledger_entries, audit_log FROM gateway_app;
REVOKE ALL ON schema_migrations FROM gateway_app;

-- Tables created by later migrations (run as the owner) get the same grants;
-- append-only tables added later must REVOKE explicitly.
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO gateway_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO gateway_app;
