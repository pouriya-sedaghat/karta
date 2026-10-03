#!/bin/sh
# Delivers an OSM snapshot to a Karta intake landing area with the
# producer completion contract (docs/runbook.md, "Local intake"):
#
#   1. compute the SHA-256 and size of the SOURCE copy, before the transfer;
#   2. transfer the snapshot (and its provenance sidecar, if present) under
#      temporary names the watcher ignores, then rename each into place;
#   3. write NAME.osm.pbf.complete (format karta-delivery/1, with that digest
#      and size) under a temporary name and rename it into place LAST.
#
# The watcher hands off nothing without the marker, and only bytes with
# exactly its digest and size, so a truncated or changed copy is refused
# (a .sha256 or .md5 file is never a completion signal).
#
# usage: scripts/deliver.sh SNAPSHOT.osm.pbf DEST [NAME]
#   DEST   a local landing directory, or sftp://[USER@]HOST[:PORT]/PATH (an
#          OpenSSH sftp batch as the landing account; keys from ssh-agent or
#          ~/.ssh as usual, SFTP_OPTS for more ssh options)
#   NAME   the delivery name (default: the snapshot's base name)
#   EXPECTED_SHA256=hex refuses to deliver a file with another digest.
set -eu
snap=${1:?usage: deliver.sh SNAPSHOT.osm.pbf DEST [NAME]}
dest=${2:?usage: deliver.sh SNAPSHOT.osm.pbf DEST [NAME]}
case "$snap" in
  *.osm.pbf) ;;
  *) echo "deliver.sh: $snap must end in .osm.pbf" >&2; exit 2 ;;
esac
name=${3:-$(basename "$snap" .osm.pbf)}
if ! printf '%s' "$name" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'; then
  echo "deliver.sh: name $name must start with a letter or digit and use only letters, digits, . _ -" >&2
  exit 2
fi
if [ ! -f "$snap" ] || [ -L "$snap" ]; then
  echo "deliver.sh: $snap is not a regular file" >&2
  exit 2
fi
sidecar=
if [ -f "$snap.provenance.json" ] && [ ! -L "$snap.provenance.json" ]; then
  sidecar="$snap.provenance.json"
fi

# The digest and size of the source copy, computed before anything moves.
if command -v sha256sum > /dev/null 2>&1; then
  digest=$(sha256sum < "$snap" | cut -d' ' -f1)
else
  digest=$(shasum -a 256 < "$snap" | cut -d' ' -f1)
fi
size=$(wc -c < "$snap" | tr -d ' ')
if [ -n "${EXPECTED_SHA256:-}" ] && [ "$digest" != "$(printf '%s' "$EXPECTED_SHA256" | tr 'A-F' 'a-f')" ]; then
  echo "deliver.sh: $snap has SHA-256 $digest, not the expected $EXPECTED_SHA256; not delivered" >&2
  exit 3
fi
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
printf '{"format": "karta-delivery/1", "file": "%s", "sha256": "%s", "size_bytes": %s}\n' "$name.osm.pbf" "$digest" "$size" > "$work/marker"

case "$dest" in
sftp://*)
  rest=${dest#sftp://}
  hostpart=${rest%%/*}
  dir=/${rest#*/}
  [ "$hostpart" != "$rest" ] || { echo "deliver.sh: $dest needs a path (sftp://HOST/PATH)" >&2; exit 2; }
  port=
  case "$hostpart" in
    *:*) port=${hostpart##*:}; hostpart=${hostpart%:*} ;;
  esac
  {
    echo "cd \"$dir\""
    echo "put \"$snap\" \".$name.osm.pbf.part\""
    echo "rename \".$name.osm.pbf.part\" \"$name.osm.pbf\""
    if [ -n "$sidecar" ]; then
      echo "put \"$sidecar\" \".$name.osm.pbf.provenance.json.part\""
      echo "rename \".$name.osm.pbf.provenance.json.part\" \"$name.osm.pbf.provenance.json\""
    fi
    echo "put \"$work/marker\" \".$name.osm.pbf.complete.part\""
    echo "rename \".$name.osm.pbf.complete.part\" \"$name.osm.pbf.complete\""
  } > "$work/batch"
  # -b: any failing command aborts the batch, so the marker is never
  # renamed into place after a failed transfer.
  # shellcheck disable=SC2086
  sftp -b "$work/batch" ${port:+-P "$port"} ${SFTP_OPTS:-} "$hostpart"
  ;;
*)
  [ -d "$dest" ] || { echo "deliver.sh: landing directory $dest does not exist" >&2; exit 2; }
  # The watcher runs as another user and reads the landing area read-only.
  umask 022
  for f in "$name.osm.pbf" "$name.osm.pbf.provenance.json" "$name.osm.pbf.complete"; do
    if [ -e "$dest/$f" ]; then
      echo "deliver.sh: $dest/$f exists; use another NAME (the watcher never reprocesses a consumed delivery)" >&2
      exit 2
    fi
  done
  cp "$snap" "$dest/.$name.osm.pbf.part"
  mv "$dest/.$name.osm.pbf.part" "$dest/$name.osm.pbf"
  if [ -n "$sidecar" ]; then
    cp "$sidecar" "$dest/.$name.osm.pbf.provenance.json.part"
    mv "$dest/.$name.osm.pbf.provenance.json.part" "$dest/$name.osm.pbf.provenance.json"
  fi
  cp "$work/marker" "$dest/.$name.osm.pbf.complete.part"
  mv "$dest/.$name.osm.pbf.complete.part" "$dest/$name.osm.pbf.complete"
  ;;
esac
echo "delivered $name.osm.pbf ($size bytes, SHA-256 $digest) to $dest"
