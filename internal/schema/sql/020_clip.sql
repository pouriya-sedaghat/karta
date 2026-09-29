-- Clip every layer to the configured region box. An extract keeps whole ways
-- and completed multipolygons that cross its selection box, so data outside
-- the box is partial; clipping gives the map a clean edge instead of ragged
-- fragments. Objects entirely outside are dropped and objects crossing the
-- edge are cut. Both counts are recorded per layer.

CREATE TEMPORARY TABLE region ON COMMIT DROP AS
SELECT public.ST_Transform(public.ST_MakeEnvelope(west, south, east, north, 4326), 3857) AS geom
FROM karta.release_params;

CREATE FUNCTION pg_temp.clip_table(tbl regclass, dim integer) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    dropped bigint;
    cut bigint;
    emptied bigint;
    label text := replace(tbl::text, 'karta.', '');
BEGIN
    EXECUTE format(
        'DELETE FROM %s t USING region r WHERE NOT public.ST_Intersects(t.geom, r.geom)', tbl);
    GET DIAGNOSTICS dropped = ROW_COUNT;
    IF dim = 0 THEN
        -- Mixed-dimension layer: keep each object's own dimension.
        EXECUTE format(
            'UPDATE %s t SET geom = public.ST_CollectionExtract(public.ST_Intersection(t.geom, r.geom), '
            'public.ST_Dimension(t.geom) + 1) FROM region r WHERE NOT public.ST_CoveredBy(t.geom, r.geom)', tbl);
    ELSE
        EXECUTE format(
            'UPDATE %s t SET geom = public.ST_CollectionExtract(public.ST_Intersection(t.geom, r.geom), %s) '
            'FROM region r WHERE NOT public.ST_CoveredBy(t.geom, r.geom)', tbl, dim);
    END IF;
    GET DIAGNOSTICS cut = ROW_COUNT;
    -- A line that only touches the edge intersects the box in a point.
    EXECUTE format('DELETE FROM %s WHERE public.ST_IsEmpty(geom)', tbl);
    GET DIAGNOSTICS emptied = ROW_COUNT;
    INSERT INTO karta.import_stats VALUES
        ('clip_dropped_' || label, dropped + emptied), ('clip_cut_' || label, cut);
END $$;

SELECT pg_temp.clip_table('karta.roads', 2);
SELECT pg_temp.clip_table('karta.waterways', 2);
SELECT pg_temp.clip_table('karta.water', 3);
SELECT pg_temp.clip_table('karta.landcover', 3);
SELECT pg_temp.clip_table('karta.buildings', 3);
SELECT pg_temp.clip_table('karta.features', 0);
