-- Vector tile function. Signature and behaviour are compatible with a Martin
-- function source: (z, x, y) -> MVT bytes, empty bytea when the tile has no
-- features. Layers, fields and zoom ranges are the contract listed in
-- internal/schema/layers.json; the style may reference only those.
--
-- Geometry is clipped to the tile plus a 64-unit buffer (of 4096) so lines
-- and polygon edges continue seamlessly across neighbouring tiles; MapLibre
-- clips fills to the tile boundary, so buffered edges are not visible.
-- Features are ordered inside each layer so that identical input always
-- produces byte-identical tiles.
CREATE FUNCTION karta.tile(z integer, x integer, y integer)
RETURNS bytea
LANGUAGE sql STABLE STRICT PARALLEL SAFE
AS $$
WITH
bounds AS (
    SELECT public.ST_TileEnvelope(z, x, y) AS env,
           public.ST_TileEnvelope(z, x, y, margin => 64.0 / 4096) AS query_env
),
landcover AS (
    SELECT public.ST_AsMVTGeom(t.geom, b.env, 4096, 64, true) AS geom,
           t.class, t.kind, karta.osm_type_name(t.osm_type) AS osm_type, t.osm_id
    FROM karta.landcover t, bounds b
    WHERE t.geom && b.query_env AND t.minzoom <= z
),
water AS (
    SELECT public.ST_AsMVTGeom(t.geom, b.env, 4096, 64, true) AS geom,
           t.kind, t.name, t.name_fa, t.name_en, karta.osm_type_name(t.osm_type) AS osm_type, t.osm_id
    FROM karta.water t, bounds b
    WHERE t.geom && b.query_env AND t.minzoom <= z
),
waterways AS (
    SELECT public.ST_AsMVTGeom(t.geom, b.env, 4096, 64, true) AS geom,
           t.kind, t.name, t.name_fa, t.name_en, t.tunnel, t.osm_id
    FROM karta.waterways t, bounds b
    WHERE t.geom && b.query_env AND t.minzoom <= z
),
buildings AS (
    SELECT public.ST_AsMVTGeom(t.geom, b.env, 4096, 64, true) AS geom,
           t.kind, karta.osm_type_name(t.osm_type) AS osm_type, t.osm_id
    FROM karta.buildings t, bounds b
    WHERE t.geom && b.query_env AND t.minzoom <= z
),
roads AS (
    SELECT public.ST_AsMVTGeom(t.geom, b.env, 4096, 64, true) AS geom,
           t.class, t.subclass, t.name, t.name_fa, t.name_en, t.ref,
           t.oneway, t.bridge, t.tunnel, t.layer, t.osm_id
    FROM karta.roads t, bounds b
    WHERE t.geom && b.query_env AND t.minzoom <= z
),
places AS (
    SELECT public.ST_AsMVTGeom(t.label_point, b.env, 4096, 64, true) AS geom,
           t.subcategory AS kind, t.display_name AS name,
           t.names->>'name:fa' AS name_fa, t.names->>'name:en' AS name_en,
           t.rank, karta.osm_type_name(t.osm_type) AS osm_type, t.osm_id
    FROM karta.features t, bounds b
    WHERE t.category = 'place' AND t.label_point && b.query_env AND t.minzoom <= z
),
pois AS (
    SELECT public.ST_AsMVTGeom(t.label_point, b.env, 4096, 64, true) AS geom,
           t.category, t.subcategory, t.display_name AS name,
           t.names->>'name:fa' AS name_fa, t.names->>'name:en' AS name_en,
           t.rank, karta.osm_type_name(t.osm_type) AS osm_type, t.osm_id
    FROM karta.features t, bounds b
    WHERE t.category <> 'place' AND t.label_point && b.query_env AND t.minzoom <= z
)
SELECT
    COALESCE((SELECT public.ST_AsMVT(l, 'landcover', 4096, 'geom' ORDER BY l.osm_type, l.osm_id) FROM landcover l WHERE l.geom IS NOT NULL), ''::bytea)
 || COALESCE((SELECT public.ST_AsMVT(l, 'water', 4096, 'geom' ORDER BY l.osm_type, l.osm_id) FROM water l WHERE l.geom IS NOT NULL), ''::bytea)
 || COALESCE((SELECT public.ST_AsMVT(l, 'waterways', 4096, 'geom' ORDER BY l.osm_id) FROM waterways l WHERE l.geom IS NOT NULL), ''::bytea)
 || COALESCE((SELECT public.ST_AsMVT(l, 'buildings', 4096, 'geom' ORDER BY l.osm_type, l.osm_id) FROM buildings l WHERE l.geom IS NOT NULL), ''::bytea)
 || COALESCE((SELECT public.ST_AsMVT(l, 'roads', 4096, 'geom' ORDER BY l.layer, l.osm_id) FROM roads l WHERE l.geom IS NOT NULL), ''::bytea)
 || COALESCE((SELECT public.ST_AsMVT(l, 'places', 4096, 'geom' ORDER BY l.rank, l.osm_type, l.osm_id) FROM places l WHERE l.geom IS NOT NULL), ''::bytea)
 || COALESCE((SELECT public.ST_AsMVT(l, 'pois', 4096, 'geom' ORDER BY l.rank, l.osm_type, l.osm_id) FROM pois l WHERE l.geom IS NOT NULL), ''::bytea)
$$;
