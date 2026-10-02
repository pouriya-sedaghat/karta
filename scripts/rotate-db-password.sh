#!/bin/sh
# Database role passwords, without data loss and without a restart.
#
#   scripts/rotate-db-password.sh api|importer|monitor|superuser
#       generates a new random password, sets it on the role, writes it into
#       the role's secret file in place, and logs in with it to prove it
#   scripts/rotate-db-password.sh --current api|importer|monitor|superuser
#       sets the role's password to the secret file's current content (after
#       a restore: a physical backup carries the backed-up cluster's roles)
#
# The services read the password file for every new database connection
# (internal/dbconn), so nothing restarts and open connections stay
# authenticated; a connection opened in the instant between the database
# change and the file change fails and is retried. The file is rewritten in
# place because Docker bind-mounts it: a replaced file would not reach the
# running containers. The password reaches psql on standard input (never a
# command line), and statement logging is off for that session.
# Run it on the database host; COMPOSE overrides `docker compose`.
set -eu
cd "$(dirname "$0")/.."
COMPOSE=${COMPOSE:-docker compose}
mode=rotate
if [ "${1:-}" = "--current" ]; then
  mode=current
  shift
fi
case "${1:-}" in
  api)       role=karta_api;      file=secrets/db_api_password;       db=karta_registry ;;
  importer)  role=karta_importer; file=secrets/db_importer_password;  db=karta_registry ;;
  monitor)   role=karta_monitor;  file=secrets/db_monitor_password;   db=postgres ;;
  superuser) role=postgres;       file=secrets/db_superuser_password; db=postgres ;;
  *) echo "usage: $0 [--current] api|importer|monitor|superuser" >&2; exit 2 ;;
esac
[ -s "$file" ] || { echo "$file is missing" >&2; exit 2; }

# Local socket as the database superuser (trusted inside the db container).
psql_su() { $COMPOSE exec -T db psql -v ON_ERROR_STOP=1 --no-psqlrc -q -U postgres -d postgres "$@"; }

exists=$(printf "SELECT count(*) FROM pg_roles WHERE rolname = '%s';\n" "$role" | psql_su -tA)
if [ "$exists" != 1 ]; then
  echo "role $role does not exist here (the monitor role is created by make monitoring-role)" >&2
  exit 3
fi
if [ "$mode" = rotate ]; then
  umask 077
  pw=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
else
  pw=$(tr -d '\r\n' < "$file")
fi
# Only characters that need no quoting; generated secrets are hex.
case "$pw" in
  *[!A-Za-z0-9._~+/=-]*) echo "$file holds characters other than [A-Za-z0-9._~+/=-]" >&2; exit 2 ;;
esac
[ ${#pw} -ge 16 ] || { echo "$file holds a password shorter than 16 characters" >&2; exit 2; }

printf "SET log_statement = 'none';\nSET log_min_duration_statement = -1;\nALTER ROLE %s PASSWORD '%s';\n" "$role" "$pw" | psql_su
if [ "$mode" = rotate ]; then
  printf '%s' "$pw" > "$file"
fi
# Prove it: log in over TCP with the new password.
printf '%s' "$pw" | $COMPOSE exec -T db sh -c 'PGPASSWORD=$(cat) psql -h 127.0.0.1 -U "$1" -d "$2" -tAc "SELECT 1" > /dev/null' sh "$role" "$db"
if [ "$mode" = rotate ]; then
  echo "$role: password rotated, written to $file and verified"
else
  echo "$role: password set from $file and verified"
fi
