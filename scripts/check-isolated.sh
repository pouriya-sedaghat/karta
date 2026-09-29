#!/usr/bin/env bash
# Checks that a running container has no way out of Docker, from its
# configuration rather than by trying to reach some external host:
#   * it is attached to exactly one network, the expected one;
#   * every network it is attached to is internal (Docker gives an internal
#     network no gateway and no route outside);
#   * it publishes no port and has no port binding configured;
#   * its network namespace has no IPv4 or IPv6 default route.
# Every violation is reported; the exit status is 1 if there is any.
#
# usage: scripts/check-isolated.sh CONTAINER NETWORK PROBE_IMAGE
#
# PROBE_IMAGE is a local image with /bin/sh and cat. It runs once, unprivileged
# and read-only, inside CONTAINER's network namespace only to read the routing
# tables (the API image is distroless and has no shell). Used by
# `make test-offline`.
set -euo pipefail

if [ $# -ne 3 ]; then
  echo "usage: $0 CONTAINER NETWORK PROBE_IMAGE" >&2
  exit 2
fi
container=$1 want=$2 probe=$3
fail=0
problem() {
  echo "not isolated: $*" >&2
  fail=1
}

nets=$(docker inspect -f '{{range $name, $_ := .NetworkSettings.Networks}}{{$name}} {{end}}' "$container")
nets=${nets% }
[ "$nets" = "$want" ] || problem "attached to networks [$nets], want only [$want]"
for n in $nets; do
  [ "$(docker network inspect -f '{{.Internal}}' "$n")" = true ] || problem "network $n is not internal"
done

published=$(docker inspect -f '{{range $port, $b := .NetworkSettings.Ports}}{{range $b}}{{.HostIp}}:{{.HostPort}}->{{$port}} {{end}}{{end}}' "$container")
[ -z "$published" ] || problem "publishes ports: $published"
bindings=$(docker inspect -f '{{len .HostConfig.PortBindings}}' "$container")
[ "$bindings" = 0 ] || problem "has $bindings port binding(s) configured"

tables=$(docker run --rm --network "container:$container" --read-only --cap-drop ALL \
  --security-opt no-new-privileges --user 65534:65534 --entrypoint /bin/sh "$probe" \
  -c 'cat /proc/net/route; echo ---; cat /proc/net/ipv6_route 2>/dev/null || true')
v4=$(printf '%s\n' "$tables" | sed '/^---$/,$d')
v6=$(printf '%s\n' "$tables" | sed '1,/^---$/d')
# /proc/net/route: Iface Destination Gateway ...; the default route has
# destination 00000000. /proc/net/ipv6_route: dest dest_len ... dev; the
# default route is ::/0 (the kernel's reject route on lo is not a way out).
default4=$(printf '%s\n' "$v4" | awk 'NR > 1 && $2 == "00000000" && $1 != "lo" {print $1}')
default6=$(printf '%s\n' "$v6" | awk '$1 == "00000000000000000000000000000000" && $2 == "00" && $10 != "lo" {print $10}')
[ -z "$default4" ] || problem "IPv4 default route via $default4"
[ -z "$default6" ] || problem "IPv6 default route via $default6"

if [ "$fail" -ne 0 ]; then
  exit 1
fi
routes4=$(printf '%s\n' "$v4" | awk 'NR > 1' | wc -l)
echo "isolated: $container is attached only to internal network $want, publishes no port, and none of its $routes4 IPv4 route(s) is a default route"
