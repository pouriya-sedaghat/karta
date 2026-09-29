# Karta Stage 1 runbook

Stage 1 serves **one** imported release: vector tiles, a MapLibre style with
local glyphs, named-place/POI search and a demo page, all from the local host.
Replacing the served data requires a reset (downtime); staged releases with
atomic switching are Stage 2 and are not available yet.

Requirements: Docker Engine with Compose v2.24+ (for `!reset`/`!override` in
the overlay files), GNU make, `curl`. Go 1.27+ only for tests and
development. Images are built locally; after `make build` nothing is fetched
from the network by any service.

## Bootstrap

```bash
make secrets        # random passwords in ./secrets (0700 dir), never overwritten
make build          # karta-api:local (distroless, 25 MB) and karta-importer:local
make up             # PostgreSQL (waits until healthy), then the API
curl -s http://localhost:8080/health/ready
# 503 {"status":"not_ready","reason":"no_active_release",...}   <- expected before an import
```

On a network that intercepts TLS, pass its CA bundle to the build:
`make build KARTA_BUILD_CA_FILE=/path/ca.pem` (used only as a build secret).

`db` initialisation (first start only) runs `deploy/postgres/initdb/10-karta.sh`:
roles `karta_reader` (NOLOGIN), `karta_api` (read-only sessions, 5 s statement
timeout), `karta_importer` (CREATEDB, not superuser), the PostGIS/pg_trgm
template `karta_template` and the registry database `karta_registry`.

## Import the fixture

```bash
make -s import-fixture > fixture-import.json   # JSON report on stdout, logs on stderr
make wait-ready                                 # the API picks the release up within 5 s
make smoke
```

The fixture (`testdata/fixture/karta-fixture.osm`, CC0) imports in under a
second. The report lists counts, skipped geometry, clipping, every validation
check and timings; a failed check aborts the import and leaves no release.

## Import the real Chitgar sample

The PBF and its sidecar are **not** in Git. Copy the two supplied files into
`data/local/` (see `docs/development-data.md`), then:

```bash
make verify-tehran    # both files present, SHA-256 7d0e69a2…191e, world-readable (importer is UID 10001)
make reset            # Stage 1 keeps one release: remove the fixture release first
make up
make import-tehran    # report also written to artifacts/tehran-import.json
make wait-ready
make test-browser-tehran   # renders lake, park/road and mall views; screenshots in artifacts/tehran/
```

`config/regions/tehran-chitgar.json` pins the snapshot digest and requires the
provenance sidecar: a different file (even a newer Geofabrik snapshot) is
refused with exit code 3. Do not substitute another snapshot silently.

## Import exit codes

| Code | Meaning | Active release |
| --- | --- | --- |
| 0 | imported and activated, or this exact release is already active | new / unchanged |
| 2 | usage or configuration error | unchanged |
| 3 | input verification failed: digest, provenance, header box, size, symlink, timestamp | unchanged |
| 4 | a different release is already active (Stage 1 serves one; `make reset` first) | unchanged |
| 5 | release validation failed (counts, tile contract, style, search or tile checks) | unchanged |
| 1 | other failure (database, osm2pgsql) | unchanged |
| 130 | interrupted | unchanged |

Each import builds in a working database `karta_c<random>`; the release id
is derived there (it includes the database toolchain versions) and the
database is renamed to `karta_<release_id>` only after every check passed. On
failure the candidate is dropped (`--keep-failed` keeps it for inspection
until the next import, which removes leftover candidates) and the registry
records the release as `failed` with the reason. Changing a region's name,
box or default view (by any amount), the provenance sidecar or the toolchain
gives a new release id; changing only its acceptance thresholds does not.

## Normal operation

* `make up` / `make down` start and stop without losing data; the API reloads
  the active release on start. Readiness (`/health/ready`) is 200 only with a
  loaded, compatible release and a reachable database; liveness
  (`/health/live`) reports the process only. The Compose health check uses
  readiness.
* Services restart `unless-stopped`; JSON logs are rotated (3 × 10 MB per
  container). Access logs contain method, path, status, bytes and duration,
  never query strings (search terms).
* If PostgreSQL goes away, readiness turns 503 `database_unavailable` and data
  requests return 503; the API recovers by itself when the database returns.
* Resource limits (override in `.env`): db 2 GiB / 2 CPUs, api 512 MiB / 1 CPU,
  importer 4 GiB. Measured use for the Chitgar sample is in the PR description.
* Upgrading the API image or changing `KARTA_PUBLIC_BASE_URL` needs no cache
  purge and no re-import: existing releases keep their ids and tile URLs, the
  style gets a new content-addressed URL (the manifest issues it), and style
  URLs issued before return `404 unknown_style` rather than different bytes.
  One exception applies only when upgrading from a build before `eab072b`
  (no external deployment is known, but none can be ruled out). Caches may
  keep that build's `style.json` responses for up to 24 hours, and those
  responses embed absolute tile and glyph URLs under the old
  `KARTA_PUBLIC_BASE_URL`. For those 24 hours:
  * keep the same release active, with no reset or re-import;
  * if the base URL changes, keep the old one reachable and serving tiles and
    glyphs as before, for example by routing it to the upgraded API;
  * purge `/v1/releases/*/style.json` in proxies or CDNs you operate.

  If the old base URL can't be kept, browser-cached copies fail to load tiles
  and glyphs until they expire. Purging a CDN does not clear browser caches.
  See `docs/api.md`, "Transition from builds before content-addressed styles".
* Upgrading the database image (PostgreSQL/PostGIS) is different: tiles are
  generated on the serving database, and the API does not yet compare its
  versions with the toolchain a release was built with. Upgrade it only
  together with `make reset` and a re-import, which gives a new release id.

## Behind a reverse proxy with a path prefix

Karta can be published under a path, e.g. `https://example.com/maps/`. Set
`KARTA_PUBLIC_BASE_URL` to the public URL including the prefix, and have the
proxy strip the prefix before forwarding:

```nginx
location /maps/ {
    proxy_pass http://127.0.0.1:8080/;   # trailing slash: /maps/v1/… is sent as /v1/…
}
```

```bash
KARTA_PUBLIC_BASE_URL=https://example.com/maps make up
```

The manifest, style, tile and glyph URLs are absolute under the base URL, so
they carry the prefix. The demo calls the API relative to its own URL, and `/`
and `/demo` redirect with a relative `Location: demo/`. So
`https://example.com/maps/` opens the demo at `https://example.com/maps/demo/`.
Expose the prefix with its trailing slash; nginx redirects a bare `/maps` to
`/maps/` for such a location. `make test-browser-prefix` runs all browser
tests through such a proxy (see Development).

## Reset and cleanup

```bash
make reset   # docker compose down -v: deletes ALL imported releases (the pgdata volume)
make clean   # reset + remove images, web/dist, web/node_modules and artifacts/
```

Secrets in `./secrets` survive both; delete the directory to rotate them
together with a `make reset` (the roles are created only at first initialisation).

## Simulated disconnected run

```bash
make test-offline
```

Starts a separate project (`karta-offline`) whose API and database are only on
the internal Docker network: no published port and no route to any external
host. `scripts/check-isolated.sh` verifies this from the running API
container's configuration:

* it is attached only to `karta-offline_backend`, and that network is `internal`;
* it publishes no port and has no port binding;
* its network namespace has no IPv4 or IPv6 default route.

As a negative control, the target first attaches the API to an extra plain
bridge network, like `frontend` in `compose.yaml`, and requires the check to
fail. It then detaches the API and requires the check to pass. The fixture is
imported. Headless Chromium, whose only network is the API container (every
other host goes to a dead proxy), loads the demo, renders two views and runs a
search. It asserts that every request went to the API origin and that the
Persian presentation-form glyph ranges were served.

## Development without Docker for the API

```bash
make web                       # npm ci + copy MapLibre into web/dist (pinned, integrity-checked)
KARTA_PUBLIC_BASE_URL=http://localhost:8080 KARTA_DB_HOST=… KARTA_DB_PASSWORD_FILE=… \
KARTA_WEB_DIR=web/dist go run ./cmd/karta serve
make lint test                 # gofmt, vet, staticcheck, govulncheck, gosec; unit tests
make test-integration          # isolated compose project on ports 18080/55433
make test-browser              # browser tests against the running stack (BASE_URL)
make test-browser-prefix       # the same through a proxy serving Karta under /maps, API restarted with that base URL
```

## Troubleshooting

| Symptom | Check |
| --- | --- |
| readiness `no_active_release` | no import yet, or the last import failed: read its report/stderr |
| readiness `release_incompatible` | the release's schema major, style layers or fonts do not match this API build; `detail` names the problem; re-import with the matching importer |
| import exit 3 "refusing to import a different snapshot" | the file is not the pinned snapshot; restore the documented file |
| import exit 3 "permission denied" | `chmod 0644` the snapshot and sidecar (importer runs as UID 10001) |
| demo shows nothing, console CSP errors | open the demo at `KARTA_PUBLIC_BASE_URL` + `/demo/` (the page only talks to that origin) |
