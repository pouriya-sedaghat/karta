#!/bin/sh
# Runs once, when the PostgreSQL data directory is first initialised.
# Creates Karta's roles and databases with least privilege:
#   karta_reader    NOLOGIN group: CONNECT to the registry and release databases, SELECT only
#   karta_api       LOGIN, member of karta_reader, read-only sessions, bounded statements
#   karta_importer  LOGIN, CREATEDB (not superuser): owns the registry and the release databases it builds
#   karta_template  PostGIS + pg_trgm template the importer clones for each release (no connections)
#   karta_registry  control-plane database (release list and active pointer)
# Passwords are read from Docker secrets inside psql (\set with backticks),
# so they never appear in a process argument list.
set -eu

for f in /run/secrets/db_importer_password /run/secrets/db_api_password; do
  if [ ! -s "$f" ]; then
    echo "karta initdb: missing or empty secret $f" >&2
    exit 1
  fi
done

psql -v ON_ERROR_STOP=1 --no-psqlrc --username "$POSTGRES_USER" --dbname postgres <<'SQL'
\set importer_pw `cat /run/secrets/db_importer_password`
\set api_pw `cat /run/secrets/db_api_password`

CREATE ROLE karta_reader NOLOGIN;
CREATE ROLE karta_importer LOGIN CREATEDB PASSWORD :'importer_pw';
CREATE ROLE karta_api LOGIN PASSWORD :'api_pw' IN ROLE karta_reader CONNECTION LIMIT 60;
ALTER ROLE karta_api SET default_transaction_read_only = on;
ALTER ROLE karta_api SET statement_timeout = '5s';
ALTER ROLE karta_api SET idle_in_transaction_session_timeout = '10s';
ALTER ROLE karta_importer SET idle_in_transaction_session_timeout = '1h';

REVOKE ALL ON DATABASE postgres FROM PUBLIC;
REVOKE ALL ON DATABASE template1 FROM PUBLIC;

CREATE DATABASE karta_template;
\connect karta_template
CREATE EXTENSION postgis;
CREATE EXTENSION pg_trgm;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
\connect postgres
REVOKE ALL ON DATABASE karta_template FROM PUBLIC;
ALTER DATABASE karta_template WITH IS_TEMPLATE true ALLOW_CONNECTIONS false;

CREATE DATABASE karta_registry OWNER karta_importer;
REVOKE ALL ON DATABASE karta_registry FROM PUBLIC;
GRANT CONNECT ON DATABASE karta_registry TO karta_reader;
SQL
echo "karta initdb: roles, template and registry created"
