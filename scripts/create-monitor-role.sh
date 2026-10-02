#!/bin/sh
# Creates (or updates) the PostgreSQL role the metrics exporter uses:
# karta_monitor, LOGIN, member of pg_monitor (read access to statistics and
# settings only: no table data), at most 3 connections, read-only sessions.
# Idempotent; works on a new and on an existing database. The password comes
# from secrets/db_monitor_password and reaches psql on standard input, never
# on a command line; statement logging is off for the session.
#
#   scripts/create-monitor-role.sh          (COMPOSE overrides `docker compose`)
set -eu
cd "$(dirname "$0")/.."
COMPOSE=${COMPOSE:-docker compose}
f=secrets/db_monitor_password
[ -s "$f" ] || { echo "$f is missing: run make secrets" >&2; exit 2; }
pw=$(cat "$f")
case "$pw" in *[!0-9a-f]*) echo "$f must hold the generated hex password" >&2; exit 2 ;; esac
printf '%s\n' \
  "SET log_statement = 'none';" \
  "SET log_min_duration_statement = -1;" \
  "DO \$\$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'karta_monitor') THEN CREATE ROLE karta_monitor; END IF; END \$\$;" \
  "ALTER ROLE karta_monitor WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 3 PASSWORD '$pw';" \
  "GRANT pg_monitor TO karta_monitor;" \
  "GRANT CONNECT ON DATABASE postgres TO karta_monitor;" \
  "ALTER ROLE karta_monitor SET default_transaction_read_only = on;" \
  "ALTER ROLE karta_monitor SET statement_timeout = '5s';" |
  $COMPOSE exec -T db psql -v ON_ERROR_STOP=1 --no-psqlrc -q -U postgres -d postgres
echo "karta_monitor role ready (pg_monitor, 3 connections, read-only)"
