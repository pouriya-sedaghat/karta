-- Derived columns used by tiles and search: true (spheroidal) area, the
-- lowest zoom at which a feature is emitted, and for named features a label
-- point, display name and importance rank. All values are computed once at
-- import from the clipped geometry, so every tile and search is reproducible.

CREATE FUNCTION karta.true_area(geom public.geometry) RETURNS double precision
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
AS $$
SELECT CASE WHEN public.ST_Dimension(geom) = 2
    THEN public.ST_Area(public.ST_Transform(geom, 4326)::public.geography)
    ELSE 0 END
$$;

CREATE FUNCTION karta.area_minzoom(area_m2 double precision, smallest smallint) RETURNS smallint
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
AS $$
SELECT CASE
    WHEN area_m2 >= 1000000 THEN 8
    WHEN area_m2 >= 100000 THEN 10
    WHEN area_m2 >= 10000 THEN 12
    WHEN area_m2 >= 1000 THEN 13
    ELSE smallest END::smallint
$$;

-- Lower rank = more important. Used as the label priority (symbol-sort-key)
-- and as the second search ordering key after match quality.
CREATE FUNCTION karta.feature_rank(category text, subcategory text) RETURNS smallint
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
AS $$
SELECT CASE
    WHEN category = 'place' THEN CASE subcategory
        WHEN 'city' THEN 1 WHEN 'town' THEN 2 WHEN 'suburb' THEN 3 WHEN 'borough' THEN 3
        WHEN 'quarter' THEN 4 WHEN 'village' THEN 4 WHEN 'neighbourhood' THEN 5
        WHEN 'hamlet' THEN 6 WHEN 'island' THEN 6 ELSE 7 END
    WHEN category = 'aeroway' AND subcategory = 'aerodrome' THEN 3
    WHEN category = 'tourism' AND subcategory IN ('attraction', 'museum', 'zoo', 'theme_park') THEN 5
    WHEN category IN ('natural', 'water') AND subcategory IN ('water', 'lake', 'reservoir', 'pond', 'peak') THEN 5
    WHEN category = 'waterway' AND subcategory IN ('river', 'canal') THEN 5
    WHEN category = 'leisure' AND subcategory IN ('park', 'nature_reserve', 'stadium', 'golf_course') THEN 6
    WHEN category = 'shop' AND subcategory = 'mall' THEN 6
    WHEN category = 'amenity' AND subcategory IN ('university', 'hospital', 'college', 'bus_station') THEN 6
    WHEN category IN ('railway', 'public_transport') AND subcategory = 'station' THEN 6
    WHEN category = 'historic' THEN 7
    WHEN category = 'tourism' THEN 8
    WHEN category IN ('amenity', 'shop', 'leisure', 'healthcare', 'office', 'craft', 'emergency',
                      'club', 'sport', 'man_made', 'natural', 'waterway', 'water', 'aeroway') THEN 9
    WHEN category = 'landuse' THEN 10
    WHEN category IN ('railway', 'highway', 'public_transport') THEN 11
    WHEN category = 'building' THEN 12
    ELSE 13 END::smallint
$$;

-- Line and area layers ---------------------------------------------------

ALTER TABLE karta.roads ADD COLUMN minzoom smallint;
UPDATE karta.roads SET minzoom = CASE class
    WHEN 'motorway' THEN 5 WHEN 'trunk' THEN 6 WHEN 'primary' THEN 8 WHEN 'secondary' THEN 9
    WHEN 'tertiary' THEN 11 WHEN 'rail' THEN 10 WHEN 'minor' THEN 13 ELSE 14 END;
ALTER TABLE karta.roads ALTER COLUMN minzoom SET NOT NULL;

ALTER TABLE karta.waterways ADD COLUMN minzoom smallint;
UPDATE karta.waterways SET minzoom = CASE WHEN kind IN ('river', 'canal') THEN 10 ELSE 13 END;
ALTER TABLE karta.waterways ALTER COLUMN minzoom SET NOT NULL;

ALTER TABLE karta.water ADD COLUMN area_m2 double precision, ADD COLUMN minzoom smallint;
UPDATE karta.water SET area_m2 = karta.true_area(geom);
UPDATE karta.water SET minzoom = karta.area_minzoom(area_m2, 13::smallint);
ALTER TABLE karta.water ALTER COLUMN area_m2 SET NOT NULL, ALTER COLUMN minzoom SET NOT NULL;

ALTER TABLE karta.landcover ADD COLUMN area_m2 double precision, ADD COLUMN minzoom smallint;
UPDATE karta.landcover SET area_m2 = karta.true_area(geom);
UPDATE karta.landcover SET minzoom = karta.area_minzoom(area_m2, 14::smallint);
ALTER TABLE karta.landcover ALTER COLUMN area_m2 SET NOT NULL, ALTER COLUMN minzoom SET NOT NULL;

ALTER TABLE karta.buildings ADD COLUMN area_m2 double precision, ADD COLUMN minzoom smallint;
UPDATE karta.buildings SET area_m2 = karta.true_area(geom);
UPDATE karta.buildings SET minzoom = CASE WHEN area_m2 >= 5000 THEN 13 ELSE 14 END;
ALTER TABLE karta.buildings ALTER COLUMN area_m2 SET NOT NULL, ALTER COLUMN minzoom SET NOT NULL;

-- Named features ---------------------------------------------------------

ALTER TABLE karta.features
    ADD COLUMN display_name text,
    ADD COLUMN rank smallint,
    ADD COLUMN area_m2 double precision,
    ADD COLUMN minzoom smallint,
    ADD COLUMN label_point public.geometry(Point, 3857),
    ADD COLUMN lon double precision,
    ADD COLUMN lat double precision,
    ADD COLUMN bbox double precision[];

-- The display name prefers the local `name`; otherwise the first of name:fa,
-- name:en, or the lexically smallest remaining name tag.
UPDATE karta.features SET
    display_name = COALESCE(names->>'name', names->>'name:fa', names->>'name:en',
        (SELECT value FROM jsonb_each_text(names) ORDER BY key COLLATE "C" LIMIT 1)),
    rank = karta.feature_rank(category, subcategory),
    area_m2 = karta.true_area(geom),
    -- The inscribed-circle centre keeps area labels well inside concave shapes.
    label_point = CASE WHEN public.ST_Dimension(geom) = 2
        THEN (public.ST_MaximumInscribedCircle(geom)).center
        ELSE public.ST_PointOnSurface(geom) END;

-- WGS84 output is rounded to 7 decimals (about 1 cm), OSM's own precision,
-- which also removes noise from the Web Mercator round trip.
UPDATE karta.features SET
    lon = round(public.ST_X(public.ST_Transform(label_point, 4326))::numeric, 7),
    lat = round(public.ST_Y(public.ST_Transform(label_point, 4326))::numeric, 7),
    bbox = ARRAY[
        round(public.ST_XMin(public.ST_Transform(geom, 4326)::public.box2d)::numeric, 7),
        round(public.ST_YMin(public.ST_Transform(geom, 4326)::public.box2d)::numeric, 7),
        round(public.ST_XMax(public.ST_Transform(geom, 4326)::public.box2d)::numeric, 7),
        round(public.ST_YMax(public.ST_Transform(geom, 4326)::public.box2d)::numeric, 7)]::double precision[],
    minzoom = CASE
        WHEN category = 'place' THEN CASE
            WHEN rank <= 1 THEN 8 WHEN rank = 2 THEN 10 WHEN rank = 3 THEN 11
            WHEN rank = 4 THEN 12 WHEN rank = 5 THEN 13 ELSE 14 END
        WHEN area_m2 >= 200000 THEN 12
        WHEN area_m2 >= 20000 THEN 13
        WHEN rank <= 6 THEN 14
        WHEN rank <= 9 THEN 15
        ELSE 16 END;

ALTER TABLE karta.features
    ALTER COLUMN display_name SET NOT NULL, ALTER COLUMN rank SET NOT NULL,
    ALTER COLUMN area_m2 SET NOT NULL, ALTER COLUMN minzoom SET NOT NULL,
    ALTER COLUMN label_point SET NOT NULL, ALTER COLUMN lon SET NOT NULL,
    ALTER COLUMN lat SET NOT NULL, ALTER COLUMN bbox SET NOT NULL;
