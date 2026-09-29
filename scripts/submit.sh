#!/bin/sh
# Submits an OSM snapshot to a Karta publication inbox using the completion
# protocol (docs/runbook.md, "Publish a snapshot"):
#
#   1. copy the snapshot (and its provenance sidecar, if present) into the
#      inbox under temporary hidden names, then rename them into place;
#   2. write NAME.osm.pbf.ready, holding the SHA-256 of the source file,
#      under a temporary name and rename it into place last.
#
# The publisher reads nothing before the marker exists and imports only if
# its private copy has exactly the marker's digest, so a partial or
# corrupted copy is never imported.
#
# usage: scripts/submit.sh SNAPSHOT.osm.pbf [INBOX_DIR] [NAME]
#   INBOX_DIR defaults to data/inbox, NAME to the snapshot's base name.
#   EXPECTED_SHA256=hex refuses to submit a file with another digest.
set -eu
snap=${1:?usage: submit.sh SNAPSHOT.osm.pbf [INBOX_DIR] [NAME]}
inbox=${2:-data/inbox}
case "$snap" in
  *.osm.pbf) ;;
  *) echo "submit.sh: $snap must end in .osm.pbf" >&2; exit 2 ;;
esac
name=${3:-$(basename "$snap" .osm.pbf)}
case "$name" in
  [A-Za-z0-9]*) ;;
  *) echo "submit.sh: name $name must start with a letter or digit" >&2; exit 2 ;;
esac
if [ ! -f "$snap" ] || [ -L "$snap" ]; then
  echo "submit.sh: $snap is not a regular file" >&2
  exit 2
fi
[ -d "$inbox" ] || { echo "submit.sh: inbox $inbox does not exist" >&2; exit 2; }
digest=$(sha256sum < "$snap" | cut -d' ' -f1)
if [ -n "${EXPECTED_SHA256:-}" ] && [ "$digest" != "$EXPECTED_SHA256" ]; then
  echo "submit.sh: $snap has SHA-256 $digest, not the expected $EXPECTED_SHA256; not submitted" >&2
  exit 3
fi
# The publisher runs as another user and reads the inbox read-only.
umask 022
tmp="$inbox/.$name.$$"
trap 'rm -f "$tmp.pbf" "$tmp.json" "$tmp.ready"' EXIT
cp "$snap" "$tmp.pbf"
mv -f "$tmp.pbf" "$inbox/$name.osm.pbf"
if [ -f "$snap.provenance.json" ] && [ ! -L "$snap.provenance.json" ]; then
  cp "$snap.provenance.json" "$tmp.json"
  mv -f "$tmp.json" "$inbox/$name.osm.pbf.provenance.json"
fi
printf '%s  %s\n' "$digest" "$name.osm.pbf" > "$tmp.ready"
mv -f "$tmp.ready" "$inbox/$name.osm.pbf.ready"
echo "submitted $name.osm.pbf ($digest) to $inbox"
