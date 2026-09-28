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
