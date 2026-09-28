# Tehran development data

Use a real OSM snapshot of the **Chitgar Lake area** in western Tehran to exercise the map and place search. The provisional WGS84 bounding box is `51.175,35.705,51.285,35.785` (west,south,east,north). It spans the lake, park, roads and surrounding named places; this is a development sample, **not** an administrative boundary or a promise that a particular OSM tag will exist in every snapshot. A small committed synthetic OSM fixture is still required for deterministic CI; the Tehran extract is the real-data acceptance sample.

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
