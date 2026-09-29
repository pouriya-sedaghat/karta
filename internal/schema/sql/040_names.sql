-- Search index source: one row per (feature, name tag, name value).
-- alt_name and old_name may hold several ';'-separated values.
--
-- lang       language suffix of the tag (name:fa -> fa); NULL for untagged
--            names such as `name`, whose language is not declared.
-- name_class 0 = name / name:<lang>, 1 = official/short/int/local/regional/
--            national names, 2 = alternative and old names. A primary-name
--            match outranks an alternative-name match of equal importance.
-- key_order  which tag to report when several names of one feature match.
CREATE TABLE karta.place_names AS
WITH exploded AS (
    SELECT f.osm_type, f.osm_id, n.key, btrim(v.value) AS value
    FROM karta.features f
    CROSS JOIN LATERAL jsonb_each_text(f.names) AS n(key, value)
    CROSS JOIN LATERAL unnest(
        CASE WHEN n.key ~ '^(alt|old)_name(:|$)' THEN string_to_array(n.value, ';')
             ELSE ARRAY[n.value] END) AS v(value)
)
SELECT DISTINCT
    osm_type,
    osm_id,
    key,
    substring(key FROM ':([a-z]{2,3})$') AS lang,
    (CASE WHEN key ~ '^name(:|$)' THEN 0
          WHEN key ~ '^(alt|old)_name(:|$)' THEN 2
          ELSE 1 END)::smallint AS name_class,
    (CASE WHEN key = 'name' THEN 0 WHEN key = 'name:fa' THEN 1 WHEN key = 'name:en' THEN 2
          WHEN key ~ '^name:' THEN 3 WHEN key ~ '^(alt|old)_name' THEN 5 ELSE 4 END)::smallint AS key_order,
    value,
    karta.normalize(value) COLLATE "C" AS norm
FROM exploded
WHERE value <> '';

DELETE FROM karta.place_names WHERE norm = '';

ALTER TABLE karta.place_names
    ALTER COLUMN osm_type SET NOT NULL, ALTER COLUMN osm_id SET NOT NULL,
    ALTER COLUMN key SET NOT NULL, ALTER COLUMN value SET NOT NULL, ALTER COLUMN norm SET NOT NULL;
