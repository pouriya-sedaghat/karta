#!/bin/sh
# Test-only osm2pgsql wrapper (compose.test.yaml): delays every import by 15 s
# so a test can observe serving during a running import, or interrupt it.
# A test holds the import longer by creating /tmp/karta-hold-import in the
# publisher container: it then waits until the file is removed.
# `--version` is not delayed.
if [ "${1:-}" != "--version" ]; then
  sleep 15
  while [ -e /tmp/karta-hold-import ]; do
    sleep 0.2
  done
fi
exec osm2pgsql "$@"
