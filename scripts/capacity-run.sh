#!/bin/sh
# Measures a running deployment (docs/operations.md, "Capacity"). It records
# the host, versions, images and resource limits, the active release and the
# storage of every release, samples the CPU and memory of every container of
# the project while it runs karta-load (from the importer image), and writes
# everything into one report directory: the evidence of one run, for one
# evidence tier, host and data set. Nothing here is a service objective.
#
#   scripts/capacity-run.sh LABEL [karta-load flags...]
#   scripts/capacity-run.sh tier-b-chitgar -duration 2m -concurrency 16
#   scripts/capacity-run.sh tier-d-iran-rate200 -rate 200 -duration 10m -concurrency 64
#
# BASE: the API under test (default http://127.0.0.1:8080, reached from the
# host network); OUT: where reports go (default artifacts/capacity);
# COMPOSE overrides `docker compose`.
set -eu
cd "$(dirname "$0")/.."
COMPOSE=${COMPOSE:-docker compose}
BASE=${BASE:-http://127.0.0.1:8080}
label=${1:-}
[ -n "$label" ] || { echo "usage: $0 LABEL [karta-load flags...]" >&2; exit 2; }
shift
case "$label" in *[!A-Za-z0-9._-]*) echo "LABEL: letters, digits, . _ - only" >&2; exit 2 ;; esac
out="${OUT:-artifacts/capacity}/$label-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$out"
importer_image="karta-importer:${KARTA_IMAGE_TAG:-local}"

containers=$($COMPOSE ps -q)
[ -n "$containers" ] || { echo "no running containers in this project" >&2; exit 1; }

# The environment of this run.
{
  echo "label: $label"
  echo "started: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "host: $(uname -srm)"
  echo "cpus: $(nproc)"
  grep -E '^(MemTotal|SwapTotal)' /proc/meminfo
  root=$(docker info --format '{{.DockerRootDir}}')
  echo "docker_root: $root"
  df -h "$root" | tail -1
  echo "docker: $(docker version --format '{{.Server.Version}}')"
  echo "compose: $($COMPOSE version --short)"
  echo "karta: $(git describe --always --dirty 2>/dev/null || echo unknown)"
  echo "karta_load_flags: $*"
} > "$out/environment.txt"
for c in $containers; do
  docker inspect -f '{{.Name}} image={{.Config.Image}} image_id={{.Image}} mem_limit={{.HostConfig.Memory}} nano_cpus={{.HostConfig.NanoCpus}} pids_limit={{.HostConfig.PidsLimit}}' "$c"
done > "$out/containers.txt"
curl -fsS "$BASE/v1/manifest" > "$out/manifest.json" || { echo "the API at $BASE does not answer" >&2; exit 1; }
$COMPOSE exec -T db psql -q -U postgres -d postgres -At -F ' ' \
  -c "SELECT datname, pg_database_size(datname) FROM pg_database WHERE datname LIKE 'karta%' ORDER BY 1" > "$out/database-sizes.txt"
$COMPOSE exec -T db sh -c 'du -sb "$PGDATA"' > "$out/pgdata-bytes.txt"
$COMPOSE exec -T db postgres --version >> "$out/environment.txt"

# Sample every container every two seconds while the load runs.
sample() {
  while [ ! -e "$out/.done" ]; do
    docker stats --no-stream --format '{{.Name}},{{.CPUPerc}},{{.MemUsage}},{{.PIDs}}' $containers 2>/dev/null |
      sed "s/^/$(date -u +%H:%M:%S),/" >> "$out/stats.csv"
    sleep 2
  done
}
echo "time,container,cpu,memory,pids" > "$out/stats.csv"
sample &
sampler=$!
status=0
docker run --rm --network host --entrypoint /usr/local/bin/karta-load "$importer_image" -base "$BASE" "$@" > "$out/load.json" || status=$?
touch "$out/.done"
wait "$sampler" 2>/dev/null || true
rm -f "$out/.done"

# Peaks per container: CPU % of one core, memory in MiB.
awk -F, 'NR > 1 {
  cpu = $3; sub(/%/, "", cpu)
  split($4, m, " / "); v = m[1]; mib = v + 0
  if (v ~ /GiB/) mib *= 1024; else if (v ~ /KiB/) mib /= 1024; else if (v ~ /[0-9]B$/ && v !~ /iB/) mib /= 1048576
  if (cpu + 0 > maxcpu[$2]) maxcpu[$2] = cpu + 0
  if (mib > maxmem[$2]) maxmem[$2] = mib
  seen[$2] = 1
} END { for (c in seen) printf "%s peak_cpu_percent=%.1f peak_memory_mib=%.1f\n", c, maxcpu[c], maxmem[c] }' "$out/stats.csv" | sort > "$out/peaks.txt"

echo "report: $out"
cat "$out/peaks.txt"
grep -E '"(requests|errors|achieved_rate_per_second|error_ratio|late)"' "$out/load.json" | head -5 || true
exit $status
