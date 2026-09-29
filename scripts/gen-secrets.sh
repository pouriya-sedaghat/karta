#!/bin/sh
# Creates random database passwords in ./secrets (never overwrites).
# The directory is private (0700); files are 0644 so the non-root users in
# the containers (postgres, UID 65532 api, UID 10001 importer) can read the
# bind-mounted secrets. Use your orchestrator's secret store in production.
set -eu
cd "$(dirname "$0")/.."
umask 077
mkdir -p secrets
chmod 700 secrets
for name in db_superuser_password db_importer_password db_api_password; do
  f="secrets/$name"
  if [ -s "$f" ]; then
    continue
  fi
  od -An -N32 -tx1 /dev/urandom | tr -d ' \n' > "$f"
  chmod 644 "$f"
  echo "created $f"
done
