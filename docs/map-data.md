# Map data, tiles and style (schema major version 1)

The machine-readable contract is `internal/schema/layers.json`; this page
explains it. The import pipeline is osm2pgsql 1.11 (flex,
`internal/schema/flex/karta.lua`) followed by the SQL in
`internal/schema/sql/`, run in one transaction inside the release database.

## Tiles

* Web Mercator XYZ, `GET /v1/releases/{release_id}/tiles/{z}/{x}/{y}.pbf`,
  Mapbox Vector Tile v2, extent 4096, buffer 64, uncompressed.
* Source zoom range **0–16**. Requests above 16 return `404
  tile_zoom_out_of_range`; MapLibre overzooms z16 tiles for closer views.
* Generated on request by `karta.tile(z, x, y)` in the release database (a
  Martin-compatible function signature). Tiles outside the region box return
  `204` without touching the database; tiles with no features also return
  `204`. Features are ordered, so a tile is byte-identical on every request.

| Layer | Geometry | From zoom | Fields |
| --- | --- | --- | --- |
| `landcover` | polygon | 8 (by area: ≥1 km² z8, ≥0.1 km² z10, ≥1 ha z12, ≥1000 m² z13, all z14) | `class` (park, grass, wood, farmland, cemetery, sport, residential, commercial, industrial, construction, education, hospital, parking, bare, wetland), `kind` (`key=value`), `osm_type`, `osm_id` |
| `water` | polygon | 8 (same area rule; all from z13) | `kind`, `name`, `name_fa`, `name_en`, `osm_type`, `osm_id` |
| `waterways` | line | river/canal z10, others z13 | `kind`, `name`, `name_fa`, `name_en`, `tunnel`, `osm_id` |
| `buildings` | polygon | ≥5000 m² z13, all z14 | `kind`, `osm_type`, `osm_id` |
| `roads` | line | motorway z5, trunk z6, primary z8, secondary z9, rail z10, tertiary z11, minor z13, service/track/path z14 | `class`, `subclass`, `name`, `name_fa`, `name_en`, `ref`, `oneway`, `bridge`, `tunnel`, `layer`, `osm_id` |
| `places` | point | city z8, town z10, suburb z11, quarter/village z12, neighbourhood z13, others z14 | `kind`, `name`, `name_fa`, `name_en`, `rank`, `osm_type`, `osm_id` |
| `pois` | point | area ≥0.2 km² z12, ≥2 ha z13, rank ≤6 z14, rank ≤9 z15, others z16 | `category`, `subcategory`, `name`, `name_fa`, `name_en`, `rank`, `osm_type`, `osm_id` |

`name` in `places`/`pois` is the display name: `name`, else `name:fa`, else
`name:en`. `rank` is an importance order (1 city … 13 other; see
`karta.feature_rank` in `030_derive.sql`) used as label priority. Absent
values are omitted from features, not sent as empty strings.

## Geometry treatment

* **Invalid geometry.** osm2pgsql's area assembler rejects polygons it cannot
  build (self-intersections, unclosed or incomplete multipolygon rings);
  these and relations whose members are missing from the extract are
  recorded in `karta.import_skipped` with a reason and reported per layer.
  Anything invalid that still arrives is repaired with GEOS
  `ST_MakeValid(…, 'method=structure')` (lines: `linework`), keeping only
  the layer's own dimension; empty results are dropped. Both counts are in the
  import report (`geometry_stats`).
* **Extract boundary.** An extract keeps whole ways and completed
  multipolygons that cross its selection box, so the data outside it is
  partial. Every layer is clipped to the region box at import; objects
  entirely outside are dropped and crossing ones are cut
  (`clip_dropped_*`, `clip_cut_*`). Search results therefore always lie inside
  the region.
* **Tile boundaries.** Geometry is clipped to the tile plus a 64/4096 buffer,
  so lines and polygon edges continue across neighbouring tiles; MapLibre
  clips fills to the tile, so the buffer is never visible. The style draws no
  outlines on clipped layers except building outlines (`fill-outline-color`),
  which coincide with the clip only outside the visible tile.
* **Label points.** Areas use the centre of the maximum inscribed circle
  (stays inside concave shapes and lakes); lines and points use
  `ST_PointOnSurface`. Coordinates in search results are rounded to 7
  decimals (OSM precision).

## Style compatibility

The style (`internal/style/basemap.json`) uses only the `karta` vector source,
no sprite and two fontstacks, `Vazirmatn Regular` and `Vazirmatn Bold`.
`style.Validate` rejects a style that uses another source, a tile layer that
the catalog does not list, a field (`["get", …]`, `["has", …]`) that the layer
does not have, or a fontstack that is not served. It runs:

1. at import, before any data work (a failing style aborts the import), and
2. whenever the API loads a release (a mismatch makes the release
   `release_incompatible` and readiness 503).

The importer also decodes tiles at every second zoom over the region centre
plus the configured tiles, and fails if a tile contains a layer or field that
is not in the catalog. The style stored with a release is the one validated at
import, so a release's style URL is stable.

## Persian and right-to-left labels

MapLibre GL JS 6.11 shapes Arabic-script text and reorders bidirectional text
itself (the RTL plugin is deprecated and not used) and requests glyphs for
the resulting Arabic Presentation Forms (U+FB50–U+FDFF, U+FE70–U+FEFF).
Glyph ranges are generated by the API from the bundled Vazirmatn 33.003
fonts (SIL OFL 1.1, `internal/glyphs/fonts/OFL.txt`), which map every Persian
letter's presentation forms, ZWNJ and Persian digits. The generator computes
exact signed distance fields from the TrueType outlines (24 px em, 3 px border,
radius 8, cutoff 0.25 — MapLibre's glyph format); ranges the font does not
cover are served as valid empty ranges. The browser tests assert that the
presentation-form ranges are requested and that Persian labels are placed,
and save screenshots for review.

## Search index

`karta.place_names` holds one row per feature, name tag and value (`alt_name`
and `old_name` are split on `;`). Tags: `name`, `name:<lang>`, `alt_name`,
`official_name`, `short_name`, `old_name`, `int_name`, `loc_name`,
`reg_name`, `nat_name`, each optionally with a `:<lang>` suffix. Only
features with a name and a place/POI tag are indexed (see `feature_keys` in
the flex config); streets, boundaries, addresses and house numbers are not.
The normalized value (`karta.normalize`) is indexed twice: a `"C"`-collated
B-tree for prefix range scans and a trigram GIN index for substring matches.
