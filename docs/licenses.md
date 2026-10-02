# Licenses, attribution and provenance (Stage 4 audit)

This records what Karta ships or runs, under which license, how
attribution is shown, and where each data snapshot came from. It is an
engineering audit, not legal advice. Questions that need the owner or
counsel are marked **Owner question**.

## OpenStreetMap data (ODbL 1.0)

Every snapshot Karta publishes is OpenStreetMap data, © OpenStreetMap
contributors, available under the Open Database License 1.0. The data scope
of the production deployment is Iran, from raw OSM data.

**Attribution shown.**

* The manifest's `attribution` object: `© OpenStreetMap contributors`,
  license `ODbL-1.0`, link to `https://www.openstreetmap.org/copyright`
  (checked in `TestStack` "manifest").
* The style's vector source carries the same attribution, so every MapLibre
  client shows it in its attribution control (checked in `TestStack`
  "style").
* The demo page's footer: "Map data © OpenStreetMap contributors, available
  under the Open Database License (ODbL)", with links to the copyright page
  and the license. `TestDemoRendersOffline` "OpenStreetMap attribution is
  visible" checks that the footer is on screen with both links and that the
  map's attribution control carries the source attribution. It runs for the
  fixture and the Chitgar views and through the path-prefix proxy.
* `docs/api.md` asks API clients to display the attribution with any map
  or search result.

**What Karta makes from the data.**

| Artifact | ODbL term (engineering reading) | Leaves the operator's control? |
| --- | --- | --- |
| release databases (PostGIS) | Derivative Database | only if backups or databases are given to others |
| backups (`scripts/backup.sh`) | contain the Derivative Databases | yes, if stored with a third party |
| vector tiles and search answers served by the API | Produced Work from a Derivative Database (generally treated so for rendered output; vector tiles are a borderline case) | yes, when the API is public |

ODbL 4.3 requires the attribution notice on Publicly Used Produced Works;
Karta shows it as above. ODbL 4.6 requires that whoever Publicly Uses a
Produced Work of a Derivative Database also offers that Derivative Database,
or the method of making it, under the ODbL. Karta's derivative is
reproducible: an unmodified snapshot, identified by the manifest's
`source_sha256` and `osm_data_timestamp`, plus this repository's osm2pgsql
flex script and SQL (`internal/schema/`) at the recorded schema revision.

**Owner question.** Whether the public deployment offers the derivative (or
the method: the snapshot reference plus the import code) and how, and
whether release databases or backups will ever leave the operator's
control (stored with a third party, shared with partners). If either
applies, the derivative-database obligations need a decision, with counsel
if necessary. The implementation does not decide this.

## Fonts

| Font | License | How it is shipped |
| --- | --- | --- |
| Vazirmatn Regular and Bold (`internal/glyphs/fonts/`) | SIL Open Font License 1.1, © 2015 The Vazirmatn Project Authors; no Reserved Font Name is declared | embedded in the `karta` binary, which generates the glyph ranges served to browsers; `OFL.txt` is in the repository and, since Stage 4, in both images at `/usr/share/licenses/karta/Vazirmatn-OFL.txt` |

The OFL permits embedding and redistribution provided each copy carries the
copyright notice and the license; the images now do. The demo footer names
the font and its license.

## Map renderer

| Component | License | How it is shipped |
| --- | --- | --- |
| MapLibre GL JS 6.11.2 (npm, integrity-pinned in `web/package-lock.json`) | BSD-3-Clause | copied into the API image with its `LICENSE.txt`, served at `/demo/vendor/maplibre-gl/LICENSE.txt`; named in the demo footer |

## Go modules compiled into the binaries

`karta` and `karta-load` link only these modules (`go list -deps`); test
tools (chromedp, kin-openapi, orb, …) are not shipped.

| Module | License |
| --- | --- |
| Go standard library (go1.27.1) | BSD-3-Clause |
| github.com/jackc/pgx/v5 v5.11.0, pgpassfile v1.0.0, pgservicefile, puddle/v2 v2.2.2 | MIT |
| golang.org/x/image v0.46.0, x/sync v0.23.0, x/text v0.42.0 | BSD-3-Clause |
| google.golang.org/protobuf v1.36.12 | BSD-3-Clause |

The image build copies each linked module's license file, and the Go
license, to `/usr/share/licenses/karta/go-modules/`. It fails if a linked
module has no license file, so a new dependency cannot be shipped without
its notice.

## Container images

| Image | Main components and licenses | Notes |
| --- | --- | --- |
| `postgis/postgis:18-3.6` (pinned by digest) | PostgreSQL (PostgreSQL License), PostGIS (GPL-2.0-or-later), GEOS (LGPL-2.1), PROJ (MIT), Debian packages | run unmodified, pulled from Docker Hub |
| `karta-importer` (built here on `ubuntu:24.04`) | osm2pgsql 1.11.0 from Ubuntu (GPL-2.0-or-later), Ubuntu packages, Karta binaries | osm2pgsql runs as a separate program; package copyright files are in `/usr/share/doc/`. If this image is ever distributed outside the operator, the GPL's source offer applies to osm2pgsql (the Ubuntu source package is its corresponding source) |
| `karta-api` (built here on `distroless/static-debian12`) | Karta binary and demo assets; distroless base: Debian `base-files`, `netbase`, `tzdata`, `ca-certificates` | copyright files of the base packages are in the image |
| `prom/prometheus:v3.15.0`, `prometheuscommunity/postgres-exporter:v0.20.1` (pinned, opt-in) | Apache-2.0; both upstream images are built on a BusyBox base (GPL-2.0) | run unmodified, pulled from upstream |
| build-only: `golang:1.27`, `node:22` | not shipped | – |

## Test data

* `testdata/fixture/`: synthetic, hand-written data (invented coordinates
  and names), CC0 1.0 (`testdata/fixture/LICENSE.md`). Not derived from
  OpenStreetMap.
* The Chitgar extract is OSM data (ODbL 1.0). It is supplied out of band
  and never committed (`.gitignore`, `docs/development-data.md`).

## Karta's own license

The repository has no top-level license file. `web/package.json` declares
`BSD-3-Clause` for the demo package only. **Owner question:** the license
under which Karta itself is distributed. It matters for ODbL 4.6 if the
import code is the offered "method of making" the derivative.

## Provenance chains

Each claim is marked **verified** (checked here) or **claimed** (recorded by
the producer, not checkable without the original file).

### Chitgar development extract (tiers B)

| Item | Value | Status |
| --- | --- | --- |
| file | `tehran-chitgar.osm.pbf`, 1,149,950 bytes | **verified** (`make verify-tehran`, 2026-10-02) |
| SHA-256 | `7d0e69a2d5e1ad184ee48637626882bb8bcb7acd8e95123e05d0697ce216191e` | **verified**; equal to the sidecar's `output_sha256` and output size, and pinned in `config/regions/tehran-chitgar.json` |
| header box | 51.175, 35.705, 51.285, 35.785 | **verified** (the publisher compares it with the region file to 1e-7°) |
| data timestamp | 2026-09-27T20:23:36Z (from the sidecar's source header; the extract's own header has none) | **claimed**, consistent with the newest object (2026-09-27T14:34:58Z, checked by the publisher) |
| extraction | osmium 1.19.0 (libosmium 2.23.0), strategy `smart`, the box above | **claimed** (sidecar) |
| source file | `iran-260927.osm.pbf`, 229,580,914 bytes, SHA-256 `fbb1b010efaa16b01ba24baaa40109f90cda4d32cf38bf4be1b5b67bac2e4529` | **claimed**: the Iran file was not available, so neither its digest nor its size has been checked |
| source origin | Geofabrik Iran extract: replication base `https://download.geofabrik.de/asia/iran-updates`, sequence 4920, timestamp 2026-09-27T20:23:36Z, generator osmium/1.16.0, header box 44.023033, 24.039475, 63.35413, 39.790447 | **claimed** (sidecar) |
| license | ODbL 1.0, © OpenStreetMap contributors | stated by the sidecar; inherent to OSM data |

### Iran (tier D): to be recorded

When the owner chooses the Iran snapshot source, record the same chain:
distributor or extraction process, file name, size, SHA-256 (verified on
the target host), replication sequence and timestamp, the header or sidecar
box (which becomes the region box; `karta region-draft`), any extraction
step with its tool versions, and which claims could not be verified.
