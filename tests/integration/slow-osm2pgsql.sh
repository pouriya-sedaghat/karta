#!/bin/sh
# Test-only osm2pgsql wrapper (compose.test.yaml): delays every import by 15 s
# so a test can observe serving during a running import, or interrupt it.
# `--version` is not delayed.
if [ "${1:-}" != "--version" ]; then
  sleep 15
fi
exec osm2pgsql "$@"
