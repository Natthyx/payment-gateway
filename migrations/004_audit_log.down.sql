DROP TRIGGER IF EXISTS trg_no_delete_audit ON audit_log;
DROP TRIGGER IF EXISTS trg_no_update_audit ON audit_log;
DROP FUNCTION IF EXISTS prevent_audit_modification();
DROP TABLE IF EXISTS audit_log;
