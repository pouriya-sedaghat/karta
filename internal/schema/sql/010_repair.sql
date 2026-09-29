-- Geometry validity. osm2pgsql already rejects polygons it cannot assemble
-- (recorded in karta.import_skipped). Anything invalid that still arrives is
-- repaired with GEOS' structure method, keeping only parts of the layer's own
-- dimension; geometries that become empty are dropped. Both counts are kept.

CREATE FUNCTION pg_temp.repair_table(tbl regclass, dim integer) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    repaired bigint;
    dropped bigint;
    label text := replace(tbl::text, 'karta.', '');
BEGIN
    EXECUTE format(
        'UPDATE %s SET geom = public.ST_CollectionExtract(public.ST_MakeValid(geom, %L), %s) '
        'WHERE NOT public.ST_IsValid(geom)',
        tbl, CASE WHEN dim = 3 THEN 'method=structure' ELSE 'method=linework' END, dim);
    GET DIAGNOSTICS repaired = ROW_COUNT;
    EXECUTE format('DELETE FROM %s WHERE geom IS NULL OR public.ST_IsEmpty(geom)', tbl);
    GET DIAGNOSTICS dropped = ROW_COUNT;
    INSERT INTO karta.import_stats VALUES ('repaired_' || label, repaired), ('dropped_empty_' || label, dropped);
END $$;

SELECT pg_temp.repair_table('karta.roads', 2);
SELECT pg_temp.repair_table('karta.waterways', 2);
SELECT pg_temp.repair_table('karta.water', 3);
SELECT pg_temp.repair_table('karta.landcover', 3);
SELECT pg_temp.repair_table('karta.buildings', 3);

-- Named features mix points, lines and areas; repair each within its dimension.
WITH repaired AS (
    UPDATE karta.features
    SET geom = public.ST_CollectionExtract(
        public.ST_MakeValid(geom, CASE WHEN public.ST_Dimension(geom) = 2 THEN 'method=structure' ELSE 'method=linework' END),
        public.ST_Dimension(geom) + 1)
    WHERE NOT public.ST_IsValid(geom)
    RETURNING 1
)
INSERT INTO karta.import_stats SELECT 'repaired_features', count(*) FROM repaired;

WITH dropped AS (
    DELETE FROM karta.features WHERE geom IS NULL OR public.ST_IsEmpty(geom) RETURNING 1
)
INSERT INTO karta.import_stats SELECT 'dropped_empty_features', count(*) FROM dropped;
