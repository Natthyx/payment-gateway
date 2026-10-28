#!/bin/sh
# Runs once, when the Postgres data volume is first initialised.
#
# Creates the login role the gateway uses at runtime. It is a member of
# gateway_app (privileges granted by migrations/010_append_only_hardening):
# it can read and write the application tables but cannot UPDATE, DELETE or
# TRUNCATE ledger_entries / audit_log, and as a non-owner it cannot disable
# their triggers. Migrations keep running as $POSTGRES_USER (the owner).
set -eu
: "${GATEWAY_RUNTIME_PASSWORD:?GATEWAY_RUNTIME_PASSWORD must be set}"

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
     -v runtime_password="$GATEWAY_RUNTIME_PASSWORD" <<'EOSQL'
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gateway_app') THEN
        CREATE ROLE gateway_app NOLOGIN;
    END IF;
END
$$;
CREATE ROLE gateway_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE IN ROLE gateway_app
    PASSWORD :'runtime_password';
EOSQL
