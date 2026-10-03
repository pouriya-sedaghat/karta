#!/bin/sh
# Restores a backup taken by scripts/backup.sh into the Compose project of
# this checkout: a fresh host, or another project (COMPOSE="docker compose
# -p NAME ..."). Restore order:
#
#   1. verify the backup (SHA256SUMS, pg_verifybackup)
#   2. compare ./secrets, restored first from offline storage, with the
#      backup's fingerprints (a mismatch only warns: step 5 applies ./secrets)
#   3. preflight, before anything is changed: the database volume and the
#      fetcher's outbox must both be empty, or --replace must be given (it
#      deletes them); an outbox the backup cannot fill must be mountable
#   4. database: a new pgdata volume, the base backup and its WAL; PostgreSQL
#      replays to the backup's consistent end when it starts
#   5. set the role passwords to ./secrets (scripts/rotate-db-password.sh
#      --current): a physical backup carries the backed-up cluster's roles
#   6. the fetcher's outbox (state and deliveries) from the backup; with
#      --replace and a backup without one, the outbox is emptied so that no
#      stale fetcher state or delivery survives
#   7. karta restore-check --finalize: the registry and every retained release
#      are verified against the backup; automatic online activation is paused
#      and the restored active pointer is audited
#   8. the API and the publisher start; the script waits until the API is
#      ready and reports the restore time
#
# The fetcher is not started: start it (make up-online) once the source is
# confirmed, and resume online activation after checking the producer's
# current serial (docs/operations.md, "Backup and restore").
#
#   scripts/restore.sh BACKUP_DIR [--replace]
# The restore-check report is kept in KARTA_RESTORE_REPORTS (default
# backups/restore-reports).
set -eu
cd "$(dirname "$0")/.."
COMPOSE=${COMPOSE:-docker compose}
export COMPOSE
backup=${1:-}
replace=${2:-}
[ -n "$backup" ] && [ -d "$backup" ] || { echo "usage: $0 BACKUP_DIR [--replace]" >&2; exit 2; }
[ -z "$replace" ] || [ "$replace" = --replace ] || { echo "usage: $0 BACKUP_DIR [--replace]" >&2; exit 2; }
backup=$(cd "$backup" && pwd)
fail() { echo "restore: $*" >&2; exit 1; }
started=$(date +%s)
id=$(basename "$backup")

# 1. The backup is complete and unchanged.
[ -f "$backup/MANIFEST" ] && [ -f "$backup/SHA256SUMS" ] && [ -f "$backup/db/base.tar.gz" ] || fail "$backup is not a Karta backup"
(cd "$backup" && sha256sum --quiet -c SHA256SUMS) || fail "the backup's files do not match SHA256SUMS"
for f in "$backup"/db/*; do
  case "$(basename "$f")" in
    base.tar.gz|pg_wal.tar.gz|backup_manifest) ;;
    *) fail "$(basename "$f"): tablespace archives are restored by hand (docs/operations.md)" ;;
  esac
done
db_image=$(sed -n 's/^  "db_image": "\(.*\)",$/\1/p' "$backup/MANIFEST")
[ -n "$db_image" ] || fail "MANIFEST names no db_image"
docker run --rm -v "$backup/db:/backup:ro" --entrypoint pg_verifybackup "$db_image" -n -q /backup ||
  fail "pg_verifybackup refused the base backup"

# 2. The secrets held offline.
[ -s secrets/db_superuser_password ] && [ -s secrets/db_importer_password ] && [ -s secrets/db_api_password ] &&
  [ -s secrets/operator_tokens ] || fail "restore ./secrets from offline storage first (or create new ones with make secrets)"
for s in db_superuser_password db_importer_password db_api_password db_monitor_password operator_tokens metrics_tokens; do
  want=$(sed -n "s/^    \"$s\": \"\\([0-9a-f]*\\)\",\\{0,1\\}\$/\\1/p" "$backup/MANIFEST")
  [ -z "$want" ] || [ ! -s "secrets/$s" ] || [ "$(sha256sum < "secrets/$s" | cut -d' ' -f1)" = "$want" ] ||
    echo "restore: note: secrets/$s differs from the backup's; it is applied as the current secret" >&2
done

# 3. Preflight: where the data goes. Nothing has been changed so far, and
# nothing is changed unless both destinations may be written.
$COMPOSE create db publisher > /dev/null 2>&1 || fail "cannot create the db and publisher containers (images built? .env?)"
dbc=$($COMPOSE ps -a -q db)
pub=$($COMPOSE ps -a -q publisher)
pgdata=$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/var/lib/postgresql"}}{{.Name}}{{end}}{{end}}' "$dbc")
online=$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/data/online"}}{{if eq .Type "volume"}}{{.Name}}{{else}}{{.Source}}{{end}}{{end}}{{end}}' "$pub")
[ -n "$pgdata" ] || fail "the db service has no pgdata volume"
[ ! -f "$backup/online.tar" ] || [ -n "$online" ] || fail "the backup has a fetcher outbox but the publisher mounts none"
has_db() { docker run --rm -v "$1:/var/lib/postgresql:ro" --entrypoint sh "$db_image" -c '[ -f "$PGDATA/PG_VERSION" ]'; }
has_files() { docker run --rm -v "$1:/v:ro" --entrypoint sh "$db_image" -c '[ -n "$(ls -A /v 2>/dev/null)" ]'; }
db_in_use=""
outbox_in_use=""
! has_db "$pgdata" || db_in_use=1
[ -z "$online" ] || ! has_files "$online" || outbox_in_use=1
if [ "$replace" != --replace ]; then
  refuse=""
  [ -z "$db_in_use" ] || refuse="volume $pgdata holds a database"
  [ -z "$outbox_in_use" ] || refuse="${refuse:+$refuse; }the outbox $online holds files$( [ -f "$backup/online.tar" ] || echo " (fetcher state or deliveries the backup would not replace)")"
  [ -z "$refuse" ] || fail "$refuse; nothing was changed. Add --replace to delete them and restore"
fi
if [ -n "$db_in_use" ]; then
  echo "restore: --replace: deleting the current database volume $pgdata"
  $COMPOSE --profile online --profile monitoring --profile tools down > /dev/null 2>&1 || true
  docker volume rm "$pgdata" > /dev/null
  $COMPOSE create db publisher > /dev/null 2>&1 || fail "cannot recreate the db container"
  dbc=$($COMPOSE ps -a -q db)
fi

# 4. The database.
echo "restore: database from $id"
docker run --rm -v "$pgdata:/var/lib/postgresql" -v "$backup/db:/backup:ro" --entrypoint sh "$db_image" -c '
  set -eu
  mkdir -p "$PGDATA"
  tar -xzf /backup/base.tar.gz -C "$PGDATA"
  [ ! -f /backup/pg_wal.tar.gz ] || tar -xzf /backup/pg_wal.tar.gz -C "$PGDATA/pg_wal"
  chown -R postgres:postgres "$(dirname "$PGDATA")"
  chmod 700 "$PGDATA"' || fail "extracting the base backup failed"
$COMPOSE up -d --wait db > /dev/null || fail "PostgreSQL did not start on the restored data"

# 5. The role passwords from ./secrets.
for r in superuser importer api monitor; do
  out=$(./scripts/rotate-db-password.sh --current $r 2>&1) && echo "restore: $out" || {
    [ $r = monitor ] && echo "restore: no monitor role in this backup (make monitoring-role creates it)" || fail "$out"
  }
done

# 6. The fetcher's outbox.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
chmod 755 "$tmp"
cp "$backup/registry.before.json" "$backup/registry.after.json" "$tmp/"
outbox_flag=""
if [ -f "$backup/online.tar" ]; then
  docker run --rm -v "$online:/dst" -v "$backup/online.tar:/online.tar:ro" --entrypoint sh "$db_image" -c '
    set -eu
    find /dst -mindepth 1 -delete
    tar --numeric-owner --same-owner -xpf /online.tar -C /dst' || fail "restoring the outbox failed"
  mkdir "$tmp/online"
  tar -xf "$backup/online.tar" -C "$tmp/online" ./.fetcher 2>/dev/null || true
  outbox_flag="--fetcher-outbox /restore/online"
elif [ -n "$outbox_in_use" ]; then
  # --replace (checked in step 3): the backup has no outbox, so the
  # current fetcher state and deliveries are removed rather than kept
  # beside a registry they do not belong to.
  echo "restore: --replace: the backup has no fetcher outbox; emptying $online"
  docker run --rm -v "$online:/dst" --entrypoint sh "$db_image" -c 'find /dst -mindepth 1 -delete' ||
    fail "emptying the outbox failed"
fi
chmod -R a+rX "$tmp"

# 7. Verify and record.
report="$tmp/report.json"
if ! $COMPOSE run --rm -T --no-deps -v "$tmp:/restore:ro" --entrypoint /usr/local/bin/karta importer restore-check \
    --expect-before /restore/registry.before.json --expect-after /restore/registry.after.json $outbox_flag \
    --finalize --backup-id "$id" --reason "restore of $id" > "$report"; then
  cat "$report"
  fail "restore-check failed: the restored data is NOT recorded as restored; services are not started"
fi
cat "$report"
reports=${KARTA_RESTORE_REPORTS:-backups/restore-reports}
mkdir -p "$reports"
cp "$report" "$reports/$id-$(date -u +%Y%m%dT%H%M%SZ).json"

# 8. Serving.
$COMPOSE up -d api publisher > /dev/null
i=0
until $COMPOSE exec -T api /usr/local/bin/karta healthcheck > /dev/null 2>&1; do
  i=$((i + 1))
  [ $i -lt 120 ] || fail "the API did not become ready"
  sleep 1
done
active=$(sed -n 's/^    "active_release_id": "\(.*\)",$/\1/p' "$report" | head -1)
echo "restore complete in $(( $(date +%s) - started )) s: active release ${active:-none}; automatic online activation is paused"
echo "next: check make op-status; start the fetcher only for a confirmed source, then resume (docs/operations.md)"
