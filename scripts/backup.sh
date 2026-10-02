#!/bin/sh
# Takes a backup of a running Karta deployment into a new directory
# DEST/karta-<UTC time> (DEST defaults to ./backups):
#
#   db/                     physical base backup of the whole PostgreSQL cluster
#                           (pg_basebackup: tar, gzip, WAL streamed, SHA-256
#                           manifest; verified with pg_verifybackup): the
#                           registry (active pointer, releases, submissions,
#                           authorizations, audit log, online anti-replay state)
#                           and every release database, consistent with each
#                           other, and the database roles
#   online.tar              the fetcher's outbox without partial downloads: its
#                           state (the highest verified serial) and the complete
#                           deliveries; only when the deployment has an outbox
#   registry.before.json    what the registry held before and after the base
#   registry.after.json     backup (karta registry-summary), for restore-check
#   config/                 config/regions, config/sources, .env, Compose files
#   MANIFEST                backup id, time, versions, images, secret
#                           fingerprints (SHA-256 of each secret file, never the
#                           secret), duration
#   SHA256SUMS              the digest of every other file
#
# Secrets are not in the backup: keep ./secrets offline, separately
# (docs/operations.md, "Backup and restore"). The backup holds the roles'
# password verifiers, the audit log and the data: store it encrypted, with
# access limited like the secrets. Files are 0600 in a 0700 directory.
#
#   scripts/backup.sh [DEST]          COMPOSE overrides `docker compose`
set -eu
cd "$(dirname "$0")/.."
COMPOSE=${COMPOSE:-docker compose}
dest=${1:-backups}
umask 077
started=$(date +%s)
id=karta-$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p "$dest"
chmod 700 "$dest"
dest=$(cd "$dest" && pwd)
work="$dest/.$id.partial"
final="$dest/$id"
[ ! -e "$final" ] || { echo "backup: $final exists" >&2; exit 1; }
mkdir "$work"
trap 'rm -rf "$work"' EXIT
fail() { echo "backup: $*" >&2; exit 1; }

db=$($COMPOSE ps -q db)
[ -n "$db" ] || fail "the db service is not running"
db_image=$(docker inspect -f '{{.Config.Image}}' "$db")
project=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$db")
me="$(id -u):$(id -g)"

summary() {
  $COMPOSE run --rm -T --no-deps --entrypoint /usr/local/bin/karta importer registry-summary > "$1" ||
    fail "registry summary failed"
}

echo "backup $id of project $project"
summary "$work/registry.before.json"

# The whole cluster, from a container sharing the database's network
# namespace (replication is allowed from 127.0.0.1 only), as the superuser.
mkdir "$work/db"
docker run --rm --network "container:$db" --user "$me" \
  -v "$work/db:/backup" -v "$(pwd)/secrets/db_superuser_password:/run/secrets/pw:ro" \
  --entrypoint sh "$db_image" -c '
    set -eu
    umask 077
    printf "127.0.0.1:5432:*:postgres:%s\n" "$(cat /run/secrets/pw)" > /tmp/pgpass
    PGPASSFILE=/tmp/pgpass exec pg_basebackup -h 127.0.0.1 -U postgres -D /backup \
      -Ft -z -X stream -c fast --manifest-checksums=SHA256 --label="$1"' sh "$id" ||
  fail "pg_basebackup failed"
docker run --rm --user "$me" -v "$work/db:/backup:ro" --entrypoint pg_verifybackup "$db_image" -n -q /backup ||
  fail "pg_verifybackup refused the base backup"
for f in "$work"/db/*; do
  case "$(basename "$f")" in
    base.tar.gz|pg_wal.tar.gz|backup_manifest) ;;
    *) echo "backup: note: $(basename "$f") is a tablespace archive; scripts/restore.sh does not restore tablespaces (docs/operations.md)" >&2 ;;
  esac
done

summary "$work/registry.after.json"
changed=false
if [ "$(grep -v '"taken_at"' "$work/registry.before.json")" != "$(grep -v '"taken_at"' "$work/registry.after.json")" ]; then
  changed=true
  echo "backup: note: the registry changed while the backup ran (a publication, switch or cleanup); restore-check reports differences as notes" >&2
fi

# The fetcher's outbox (a volume, or a host directory), wherever the
# publisher reads it. The fetcher pauses while it is copied.
pub=$($COMPOSE ps -a -q publisher)
online=""
if [ -n "$pub" ]; then
  online=$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/data/online"}}{{if eq .Type "volume"}}{{.Name}}{{else}}{{.Source}}{{end}}{{end}}{{end}}' "$pub")
fi
if [ -n "$online" ]; then
  fetcher=$($COMPOSE --profile online ps -q --status running fetcher 2>/dev/null || true)
  [ -z "$fetcher" ] || $COMPOSE --profile online stop fetcher > /dev/null
  docker run --rm -v "$online:/src:ro" -v "$work:/dst" --entrypoint sh "$db_image" -c '
    set -eu
    cd /src
    tar --numeric-owner --exclude=./.partial --exclude="./.tmp-*" -cf /dst/online.tar .
    chown "$1" /dst/online.tar' sh "$me" || fail "copying the fetcher outbox failed"
  [ -z "$fetcher" ] || $COMPOSE --profile online start fetcher > /dev/null
fi

mkdir -p "$work/config"
cp -R config/regions config/sources "$work/config/"
for f in .env compose*.yaml deploy/monitoring/thresholds.yml; do
  [ ! -f "$f" ] || cp "$f" "$work/config/$(echo "$f" | tr / _)"
done

fingerprint() { [ ! -s "secrets/$1" ] || printf '    "%s": "%s",\n' "$1" "$(sha256sum < "secrets/$1" | cut -d' ' -f1)"; }
image_id() { docker image inspect -f '{{.Id}}' "$1" 2>/dev/null || echo unknown; }
{
  echo "{"
  echo "  \"backup_id\": \"$id\","
  echo "  \"created_at\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\","
  echo "  \"compose_project\": \"$project\","
  echo "  \"karta_version\": \"$(git describe --always --dirty 2>/dev/null || echo unknown)\","
  echo "  \"karta_commit\": \"$(git rev-parse HEAD 2>/dev/null || echo unknown)\","
  echo "  \"db_image\": \"$db_image\","
  echo "  \"api_image_id\": \"$(image_id "karta-api:${KARTA_IMAGE_TAG:-local}")\","
  echo "  \"importer_image_id\": \"$(image_id "karta-importer:${KARTA_IMAGE_TAG:-local}")\","
  echo "  \"postgres_version\": \"$($COMPOSE exec -T db postgres --version | tr -d '\r')\","
  echo "  \"registry_changed_during_backup\": $changed,"
  echo "  \"online_outbox\": $( [ -n "$online" ] && echo true || echo false ),"
  echo "  \"secret_fingerprints\": {"
  for s in db_superuser_password db_importer_password db_api_password db_monitor_password operator_tokens metrics_tokens; do
    fingerprint "$s"
  done | sed '$ s/,$//'
  echo "  },"
  echo "  \"duration_seconds\": $(( $(date +%s) - started ))"
  echo "}"
} > "$work/MANIFEST"
(cd "$work" && find . -type f ! -name SHA256SUMS | sort | xargs sha256sum) > "$work/SHA256SUMS"
chmod -R go-rwx "$work"
mv "$work" "$final"
trap - EXIT
echo "backup complete: $final ($(du -sh "$final" | cut -f1), $(( $(date +%s) - started )) s)"
