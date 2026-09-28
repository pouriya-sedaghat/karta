# ADR 0002: Stage 1 implementation choices

Status: proposed with the Stage 1 PR, 2026-09-28. Builds on
[ADR 0001](../architecture.md).

## Decisions

**One language: Go (1.27).** The API, importer orchestration, glyph generator
and all tests are Go. A single static binary (`karta serve | import |
healthcheck`) runs in a 25 MB distroless image; `net/http` gives the
timeouts, header limits and routing we need without a framework; pgx is a
mature PostgreSQL driver; `go vet`, staticcheck, gosec and govulncheck cover
static and security analysis. Python (the existing extraction script) would
have needed a separate web stack and ASGI timeout handling; Go keeps one
toolchain for service, tools and tests (including the headless-browser test via
chromedp).

**Tiles from PostGIS functions, served by the API, instead of Martin.** Each
release database contains `karta.tile(z, x, y) → bytea`, built from the same
SQL that defines the layers. The API calls it on the release's read-only
pool. This gives per-request release selection (Stage 2 serves a pinned old
release next to a new one) without reconfiguring or proxying a second server,
keeps one port and one set of timeouts, and lets the importer validate the
very function that will serve tiles. The signature is Martin's function-source
signature, so Martin can serve a release database later if tile throughput
needs it. Measured on the Chitgar sample: distinct uncached tiles p50 6.9 ms,
p95 62.5 ms.

**A database per release, plus a registry database.** A release is an
immutable database `karta_<release_id>` holding data, search index, tile
function, style and metadata (`karta.release_info`), set read-only after
import. The registry holds the release list, states and the single active
pointer. Stage 2 imports into a new candidate database while serving, then
flips the pointer in one transaction; the API already resolves the release
once per request and keeps one pool per release, so no consumer changes.
Stage 1 only bootstraps: an import activates itself when nothing is active and
refuses (exit 4) otherwise. **Stage 2 design gate:** `release.Manager.swap`
closes the previous release's pool immediately, which is correct only because
Stage 1 changes the pointer solely after a reset. Switching while serving must
first keep the previous release resolvable for a grace period covering
in-flight and pinned requests.

**Release id = hash of every output-affecting input.** `r` + 24 hex of
SHA-256 over a versioned canonical text (`releaseid.Canonical`, stored in
`karta.release_info.identity`): snapshot digest, provenance-sidecar digest,
data timestamp and its source, region id, name, box and default view, schema
revision (digest of the flex config, SQL and layer catalog), style revision
(digest of the style template), attribution and license, and the toolchain
that shapes tiles and search (osm2pgsql, PostgreSQL, PostGIS, GEOS, PROJ,
pg_trgm, ICU collation version). The toolchain is read from the candidate
database, so an import builds in a randomly named `karta_c…` database and
renames it to `karta_<release_id>` only after validation. Identical inputs
always give the same id and any change gives a new one, so release-pinned
style and tile URLs can be cached as immutable. Acceptance thresholds are
excluded: they decide acceptance, not output. The schema also has a *major*
version: the API serves releases whose major version it supports. Resources
that are not release-pinned (glyphs, demo assets, the served style's
rendering) carry validators derived from their bytes.

**Search normalization inside the release database.** `karta.normalize()`
builds the index at import and normalizes queries at request time, so the two
cannot diverge and each release carries the normalization its index was built
with. Normalization is NFKD, ICU case folding, removal of marks/tatweel/format
characters, Arabic→Persian letter variants, digit folding and punctuation →
space. Ranking is deterministic and documented in the OpenAPI description.

**Persian labels: MapLibre 6.11 built-in shaping + generated SDF glyphs.**
MapLibre 6 shapes Arabic script and handles bidi itself (the RTL plugin is
deprecated). It needs glyphs for Arabic Presentation Forms; Vazirmatn 33.003
(SIL OFL 1.1) covers them and Latin in one family. Glyph ranges are generated
in Go from the bundled TTF (exact outline distance fields) instead of
committing binary glyph files or depending on node-fontnik, which needs
prebuilt native binaries downloaded at install time.

**PostgreSQL 18.6 / PostGIS 3.6.4 (GEOS 3.14).** Needed for
`ST_MakeValid(…, 'method=structure')`, `casefold()` and current GEOS. The
`postgis/postgis:17-3.5` image ships GEOS 3.9.

**osm2pgsql 1.11.0 from Ubuntu 24.04.** The flex output with
`geom:is_null()` checks lets the Lua config record every object that could
not be turned into geometry. Non-slim mode suits region-sized imports;
`KARTA_OSM2PGSQL_SLIM=true` switches to a disk-backed middle for large ones.

## Pinning

Base images are pinned by manifest-list digest (Dockerfile, compose.yaml);
osm2pgsql by Debian package version; Go modules by `go.sum`; MapLibre by
`package-lock.json` integrity hashes; the fonts are committed with their
license. GitHub Actions are pinned to major version tags, not commit SHAs.
