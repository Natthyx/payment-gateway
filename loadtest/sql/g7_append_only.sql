-- G7: is ledger_entries / audit_log append-only, and by what mechanism?
-- run.sh runs this file twice: connected as the app's runtime role
-- (gateway_runtime) and as the table owner (gateway, also the image superuser).
-- Every attempt runs in its own transaction and is ROLLED BACK, so even an
-- attempt that succeeds leaves the database unchanged.
-- "ERROR:" after an attempt = blocked; "UPDATE n" / "DELETE n" / "TRUNCATE TABLE" = NOT blocked.
\set ON_ERROR_STOP off
\pset pager off

\echo '== 1. Connected role, ownership, protections'
SELECT current_user AS connected_as, r.rolsuper AS is_superuser,
       pg_has_role(current_user, 'gateway_app', 'member') AS member_of_gateway_app
FROM pg_roles r WHERE r.rolname = current_user;
SELECT tablename, tableowner FROM pg_tables WHERE tablename IN ('ledger_entries', 'audit_log') ORDER BY 1;
SELECT tgrelid::regclass AS table_name, tgname,
       CASE WHEN tgtype & 2 = 2 THEN 'BEFORE' ELSE 'AFTER' END AS timing,
       CASE WHEN tgtype & 1 = 1 THEN 'ROW' ELSE 'STATEMENT' END AS level
FROM pg_trigger WHERE NOT tgisinternal AND tgrelid::regclass::text IN ('ledger_entries', 'audit_log') ORDER BY 1, 2;
SELECT t AS table_name,
       has_table_privilege(current_user, t, 'INSERT')   AS can_insert,
       has_table_privilege(current_user, t, 'UPDATE')   AS can_update,
       has_table_privilege(current_user, t, 'DELETE')   AS can_delete,
       has_table_privilege(current_user, t, 'TRUNCATE') AS can_truncate
FROM unnest(ARRAY['ledger_entries', 'audit_log']) AS t;
SELECT 'ledger_entries' AS t, count(*) FROM ledger_entries UNION ALL SELECT 'audit_log', count(*) FROM audit_log;

\echo '== 2. ledger_entries'
\echo '-- 2a UPDATE'
BEGIN; UPDATE ledger_entries SET amount = amount + 1 WHERE id = (SELECT min(id) FROM ledger_entries); ROLLBACK;
\echo '-- 2b DELETE'
BEGIN; DELETE FROM ledger_entries WHERE id = (SELECT min(id) FROM ledger_entries); ROLLBACK;
\echo '-- 2c TRUNCATE'
BEGIN; TRUNCATE ledger_entries; ROLLBACK;
\echo '-- 2d DISABLE TRIGGER, then UPDATE'
BEGIN; ALTER TABLE ledger_entries DISABLE TRIGGER trg_no_update_ledger;
UPDATE ledger_entries SET amount = amount + 1 WHERE id = (SELECT min(id) FROM ledger_entries); ROLLBACK;
\echo '-- 2e session_replication_role=replica (skips ordinary triggers), then DELETE'
BEGIN; SET LOCAL session_replication_role = replica;
DELETE FROM ledger_entries WHERE id = (SELECT min(id) FROM ledger_entries); ROLLBACK;

\echo '== 3. audit_log'
\echo '-- 3a UPDATE'
BEGIN; UPDATE audit_log SET action = 'tampered' WHERE id = (SELECT min(id) FROM audit_log); ROLLBACK;
\echo '-- 3b DELETE'
BEGIN; DELETE FROM audit_log WHERE id = (SELECT min(id) FROM audit_log); ROLLBACK;
\echo '-- 3c TRUNCATE'
BEGIN; TRUNCATE audit_log; ROLLBACK;
\echo '-- 3d DISABLE TRIGGER, then UPDATE'
BEGIN; ALTER TABLE audit_log DISABLE TRIGGER trg_no_update_audit;
UPDATE audit_log SET action = 'tampered' WHERE id = (SELECT min(id) FROM audit_log); ROLLBACK;
\echo '-- 3e session_replication_role=replica, then DELETE'
BEGIN; SET LOCAL session_replication_role = replica;
DELETE FROM audit_log WHERE id = (SELECT min(id) FROM audit_log); ROLLBACK;

\echo '== 4. INSERT still works (rolled back)'
BEGIN; INSERT INTO audit_log (actor, action, target) VALUES ('system:g7', 'g7.probe', 'g7'); ROLLBACK;

\echo '== 5. State after all attempts (must equal section 1)'
SELECT 'ledger_entries' AS t, count(*) FROM ledger_entries UNION ALL SELECT 'audit_log', count(*) FROM audit_log;
