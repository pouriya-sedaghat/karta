# Fixture license

`karta-fixture.osm` is a hand-written, synthetic OSM XML file created for
Karta's tests. Its coordinates (near 0°, 0°), names and tags are invented and
are **not** derived from OpenStreetMap or any other dataset.

To the extent possible under law, the Karta authors dedicate this fixture to
the public domain under the [Creative Commons CC0 1.0 Universal
dedication](https://creativecommons.org/publicdomain/zero/1.0/).

## What it contains and why

| Objects | Purpose |
| --- | --- |
| way 100 lake (`name`, `name:fa`, `name:en`), way 302 attraction with the same Persian name | water layer, exact Persian and English search, equal-rank tie broken by area |
| relation 900 pond multipolygon with an inner ring, `name=برکه ۷` | multipolygon assembly, Persian digits (`برکه 7` matches) |
| relation 901 multipolygon whose member way is missing | incomplete geometry is skipped and reported |
| way 104 self-intersecting "bowtie" grass | invalid polygon is rejected and reported |
| way 200 primary road crossing the east edge of the region | clipping to the region box |
| ways 201-204 residential, footway, subway tunnel, construction | road classes, tunnels, excluded highway values |
| way 205 stream, ways 300-301 buildings | waterways and buildings layers |
| node 500 `كافه آزمون` (Arabic kaf) | Arabic/Persian letter normalization |
| node 501 `Café Délice` | Latin diacritic folding |
| nodes 502, 503 two `Twin Kiosk` | deterministic tie-break by OSM id |
| node 504 name with ZWNJ | ZWNJ treated as a space |
| node 505 `ایستگاه ۵` / `Station 5` | Persian digits, rank of stations |
| node 506 only `name:en` | display name fallback |
| node 507 unnamed bench | unnamed objects are not searchable |
| node 508 outside the region | dropped by clipping |
| node 509 name with harakat, node 512 with tatweel, node 513 with Arabic yeh | diacritic, tatweel and yeh normalization |
| nodes 510, 511 neighbourhood and suburb | places layer |

## Publication snapshots (Stage 2)

`karta-fixture-b.osm` is snapshot **B**, a later hand edit of the same
synthetic data (also CC0): its header timestamp is 2026-02-01 instead of
2026-01-01, node 506 (`English Only Cafe`) is removed and node 520
(`کافه نسخه دوم` / `Second Edition Cafe`) is added, so a search tells the two
releases apart in both directions while the region's validation queries pass
for both.

`snapshots/karta-fixture-a.osm.pbf` (from `karta-fixture.osm`) and
`snapshots/karta-fixture-b.osm.pbf` (from `karta-fixture-b.osm`) are the
deterministic PBF conversions the publication tests submit through the inbox
(`go run ./cmd/karta-fixture` regenerates them; `-check` verifies them; a unit
test does the same). They use uncompressed blobs so their bytes do not depend
on a zlib implementation, and their SHA-256 digests are pinned in
`config/regions/fixture.json`:

| Snapshot | SHA-256 | Bytes | Data timestamp |
| --- | --- | --- | --- |
| A | `6a0672e252f62b03971106770d539e2ea506b85db535dfa28354c1bc66a0a94f` | 2,157 | 2026-01-01T00:00:00Z |
| B | `493f7292f7f6bebaef9e8af21d3c3da053ad18938381764a61fad2b4a980bc38` | 2,189 | 2026-02-01T00:00:00Z |
