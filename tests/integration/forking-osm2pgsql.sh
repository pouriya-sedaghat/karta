#!/bin/sh
# Test-only osm2pgsql wrapper (compose.test.yaml) for the bounded-publication
# tests. Every import starts a background child that inherits and keeps the
# output pipe open, as a stray helper process would. Then:
#   /tmp/karta-fork-hang exists  the wrapper never finishes (a hung import):
#                                only the publication deadline stops it
#   otherwise                    osm2pgsql runs normally and the wrapper
#                                exits, leaving the child behind
# Each child records its PID in /tmp/karta-fork-children for the test.
# `--version` is passed through.
if [ "${1:-}" = "--version" ]; then
  exec osm2pgsql "$@"
fi
sleep 3600 &
echo $! >> /tmp/karta-fork-children
echo "forking-osm2pgsql: child $! keeps the output pipe open"
if [ -e /tmp/karta-fork-hang ]; then
  echo "forking-osm2pgsql: hanging"
  while :; do sleep 1; done
fi
osm2pgsql "$@"
