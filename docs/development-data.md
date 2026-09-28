# Tehran development data

Use a real OSM snapshot of the **Chitgar Lake area** in western Tehran to exercise the map and place search. The provisional WGS84 bounding box is `51.175,35.705,51.285,35.785` (west,south,east,north). It spans the lake, park, roads and surrounding named places; this is a development sample, **not** an administrative boundary or a promise that a particular OSM tag will exist in every snapshot. A small committed synthetic OSM fixture is still required for deterministic CI; the Tehran extract is the real-data acceptance sample.

## Verified development snapshot (2026-09-27)

An actual Iran-derived extract and provenance sidecar have been supplied for local development. They are available in `data/local/tehran-chitgar.osm.pbf` and `data/local/tehran-chitgar.osm.pbf.provenance.json` in the working copy; neither is tracked by Git. A separate checkout or Claude Code environment needs these two files copied into its own `data/local/` directory before real-data checks can run. Do not substitute an online download silently: use the exact digest below when reproducing these observations.

| Observation | Value |
| --- | --- |
| Extract SHA-256 and byte size | `7d0e69a2d5e1ad184ee48637626882bb8bcb7acd8e95123e05d0697ce216191e`, 1,149,950 bytes |
| Source Iran SHA-256 (sidecar claim; original unavailable here) | `fbb1b010efaa16b01ba24baaa40109f90cda4d32cf38bf4be1b5b67bac2e4529` |
| Source snapshot timestamp (sidecar) | 2026-09-27T20:23:36Z; extraction used `osmium` 1.19.0 with `smart` strategy |
| Selection box | `51.175,35.705,51.285,35.785` (W,S,E,N) |
| Objects | 125,907 nodes; 18,619 ways; 267 relations |
| Relevant raw tags | 7,530 ways with `highway` (plus 768 nodes); 76 ways/relations with `natural=water`; 1,020 ways/relations with `landuse` (including 638 `grass`; plus 12 nodes); 129 ways/relations with `leisure=park` (plus 1 node); 976 objects of any type with `amenity` (535 nodes, 439 ways, 2 relations) |
| Names | 6,051 objects with `name`; 241 with an explicit `name:fa`; 5,844 `name` values containing at least one Persian/Arabic-script character |
| Search reference | way `1259635603`, `name=دریاچه چیتگر`, `name:fa=دریاچه چیتگر`, `name:en=Chitgar Lake`, `tourism=attraction` |

The supplied validation report records 3,473 “PERSIAN NAME VALUES” without its counting command or tag definition. This is **not** the number of explicit `name:fa` tags. The counts in the table come from an independent scan of the supplied PBF's OSM entities and tags; the file's SHA-256 matches the sidecar and report. All 124,175 distinct way-node references resolve within the extract; the decoded node-coordinate extent `51.1248135,35.6214497,51.3609568,35.8379681` matches the supplied osmium report. This extent exceeds the selection box because intersecting geometry is retained. The original Iran file was not available to check the claimed source digest or rerun extraction; the supplied report shows an empty `osmium check-refs` output but does not record its exit status.

These observations validate the **raw development input**, not the Stage 1 importer, search ranking, styled tiles, Persian glyph rendering, or disconnected operation. Stage 1 must still import this exact file and record its resulting layer counts, query result, rendered map, runtime and resources. Compare the raw tag counts to the import behavior, accounting explicitly for filtering and geometry repair.

## Stage 1 import of this snapshot (2026-09-28)

`make import-tehran` imported the file above (digest verified against the region pin and the sidecar; data timestamp 2026-09-27T20:23:36Z taken from the sidecar's source header, since the extract's own header has none) as release `rc7684f3108af1233d275559b` (schema `s1-f04d8b82ea6a669f`, style `st1-9d59ea57b8a34b6a`; toolchain osm2pgsql 1.11.0, PostgreSQL 18.6, PostGIS 3.6.4, GEOS 3.14.1, PROJ 9.8.1, ICU 153.128, all part of the release identity). All 24 acceptance checks in `config/regions/tehran-chitgar.json` passed.

| Imported (after filtering, repair and clipping to the box) | Rows |
| --- | --- |
| `roads` (highway classes + railways) | 7,511 (raw: 7,530 ways with `highway`; construction, proposed, platform and area highways are not roads, railways are added) |
| `water` / `waterways` | 80 / 33 (raw: 76 ways/relations with `natural=water`, plus reservoirs and basins) |
| `landcover` / `buildings` | 1,990 / 8,087 |
| searchable `features` (named place/POI objects) | 2,673: 71 places, 2,602 POIs; 3,329 indexed name values |

* Names: 2,659 features have `name`, 2,512 of them in Persian script; 134 have an explicit `name:fa`, 446 `name:en`. Of the 241 raw objects with `name:fa`, 90 are streets and 10 are route/boundary relations without a place/POI tag (not searchable by design) and 141 are place/POI objects; 134 of those are imported. The other 7: four administrative boundary relations (Tehran Province, Tehran County, Tehran city, Central Tehran section) whose member ways are mostly outside the extract, and three nodes outside the box (for example `شهر زیبا` at 51.2947). The same filters explain 2,512 imported vs 5,844 raw Persian-script `name` values (3,055 raw values are street names). The report's 3,473 “PERSIAN NAME VALUES” matches neither count.
* Geometry: no polygon needed repair; osm2pgsql could not build 34 relations (incomplete boundaries) and 12 relations are route/stop-area types; all 46 are listed in `karta.import_skipped`. Clipping cut 280 roads, 107 landcover areas, 69 buildings, 48 features, 5 waterways and 3 water areas at the box edge, and dropped 3 features outside it.
* Search: `دریاچه چیتگر` and `Chitgar Lake` (with and without `lang=en`) both return way `1259635603` first (exact match on `name` / `name:en`) and relation `8128152` (“دریاچه شهدای خلیج فارس”, which carries the same strings as `alt_name`/`alt_name:en`) second. The lake's water polygon is relation `8128152`.
* Tiles: z14 `14/10522/6448` over the lake is 15,978 bytes with water 3, landcover 13, buildings 29, roads 166, places 2, pois 5; z15 `15/21044/12898` (freeway and Chitgar forest park) 9,561 bytes; z12 overview 45,834 bytes. The browser test renders the lake, park/road, overview and Iran Mall views with Persian labels placed and saves screenshots.
* Cost: import 3.5–4.1 s end to end (5.3 s including container start) (osm2pgsql 0.6–0.8 s, post-import SQL 2.4–2.6 s); osm2pgsql peak RSS 36 MB; PostgreSQL container peak about 139 MiB during import; release database 34 MB (PostgreSQL data directory 132 MB including template and registry).

## Obtain and extract

On a machine with sufficient RAM/disk (the osmium reference check can use substantial RAM even for a small extract), Python 3.9+, and [osmium-tool](https://osmcode.org/osmium-tool/) installed, download `iran-latest.osm.pbf` from [Geofabrik's Iran extract](https://download.geofabrik.de/asia/iran.html) using a browser or your normal trusted downloader. Record its download URL, date and published checksum if one is provided. Do not commit the input or the resulting PBF. Run:

```bash
mkdir -p data/local
python3 scripts/extract_tehran.py \
  --input /absolute/path/to/iran-latest.osm.pbf \
  --output data/local/tehran-chitgar.osm.pbf
```

To override the development box, pass `--bbox WEST,SOUTH,EAST,NORTH`. If the distributor provides an independently verified SHA-256, pass it as `--expected-sha256 HEX`. Use an **absolute** path to the input so that the command works from the repository root. The script fails rather than overwriting an existing extract; use a different output name for a newer source. It creates `tehran-chitgar.osm.pbf` and `tehran-chitgar.osm.pbf.provenance.json` beside it. The provenance contains both SHA-256 digests, the selection box, osmium version, full-file information and attribution. The script writes to temporary files, verifies the full extract can be parsed, is nonempty and that all way node references exist, then publishes the files. It uses `smart` extraction to preserve multipolygon geometry at the boundary; other relation types may be incomplete. An `osmium check-refs -r` failure alone does not imply the extract is invalid for this use.

Do not copy the PBF directly into the runtime update inbox: the extraction tool produces a **development data source**, not a release. A future import still has to validate its configured region, content, provenance and version before activation. Geofabrik may update the `latest` input at any time; the SHA-256 in the sidecar is the exact snapshot identity. If only the current file name is known, source data recency must be taken from recorded source metadata, never inferred from the output file's modification time.

## Verify before use

```bash
osmium fileinfo -e data/local/tehran-chitgar.osm.pbf
osmium check-refs data/local/tehran-chitgar.osm.pbf
sha256sum data/local/tehran-chitgar.osm.pbf
```

Compare the SHA-256 with `output_sha256` in the provenance JSON. When the real service is implemented, load this extract with the same import path used for the small fixture and verify a nonempty road tile, a nonempty water tile, a named Persian search result and locally served font/style assets. Log the actual names, OSM IDs, counts and date in the stage's PR; if a tag is missing, choose and document another area or adjust the explicit data check. Do not invent expected data or mark the real-data check passed without running it. The extracted objects can extend beyond the bounding box because ways and multipolygons are kept whole; clip tile output to tile bounds in the renderer.

OSM data is available under [ODbL](https://www.openstreetmap.org/copyright). Show `© OpenStreetMap contributors` with a link to the license in the map UI, and retain provenance when sharing any derived database. Review any extra font/style licenses independently.

### Why `smart` and these checks?

Osmium documents `--bbox` in longitude,latitude order, `smart` as a three-pass strategy that completes intersecting multipolygons, and `check-refs` as a way/node integrity check unless `-r` is supplied. Its `fileinfo -e -j` scans the full input and emits machine-readable metadata. See [extract](https://docs.osmcode.org/osmium/latest/osmium-extract.html), [check-refs](https://docs.osmcode.org/osmium/latest/osmium-check-refs.html), and [fileinfo](https://docs.osmcode.org/osmium/latest/osmium-fileinfo.html).
