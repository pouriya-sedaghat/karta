-- Runs before osm2pgsql. Creates the release schema, the parameters the
-- post-import steps read, and the table that collects import statistics.
CREATE SCHEMA karta;

CREATE TABLE karta.release_params (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    region_id text NOT NULL,
    west double precision NOT NULL,
    south double precision NOT NULL,
    east double precision NOT NULL,
    north double precision NOT NULL,
    CHECK (west < east AND south < north)
);

CREATE TABLE karta.import_stats (
    key text PRIMARY KEY,
    value bigint NOT NULL
);

-- Search normalization. The same function builds the name index at import
-- and normalizes queries at request time, so both always agree. Steps:
--   1. NFKD: folds compatibility forms (Arabic presentation forms, full-width
--      Latin, ligatures) and separates combining marks from base letters.
--   2. Unicode case folding with the ICU root collation.
--   3. Remove combining marks, tatweel and invisible format characters.
--   4. Map Arabic letter variants to their Persian forms, Arabic-Indic and
--      Persian digits to ASCII, and ZWNJ to a space.
--   5. Replace every run of non-alphanumeric characters with one space.
-- The indexed column and query comparisons use the "C" collation (code
-- point order) so a prefix search is a B-tree range scan.
CREATE FUNCTION karta.normalize(input text)
RETURNS text
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
AS $$
SELECT btrim(regexp_replace(
    translate(
        regexp_replace(
            casefold(normalize(input, NFKD) COLLATE "und-x-icu"),
            '[\u0300-\u036F\u0483-\u0489\u0591-\u05C7\u0610-\u061A\u0640\u064B-\u065F\u0670\u06D6-\u06DC\u06DF-\u06E4\u06E7\u06E8\u06EA-\u06ED\u08D3-\u08FF\u1AB0-\u1AFF\u1DC0-\u1DFF\u20D0-\u20FF\uFE20-\uFE2F\u00AD\u200B\u200D-\u200F\u202A-\u202E\u2060-\u2064\u2066-\u206F\uFEFF]',
            '', 'g'),
        U&'\064A\0649\0643\06D5\06C1\06BE\0629\0671\0660\0661\0662\0663\0664\0665\0666\0667\0668\0669\06F0\06F1\06F2\06F3\06F4\06F5\06F6\06F7\06F8\06F9\200C',
        U&'\06CC\06CC\06A9\0647\0647\0647\0647\0627' || '01234567890123456789 '),
    '[^[:alnum:]]+', ' ', 'g'))
$$;

-- Maps osm2pgsql's type column to the API's OSM element type.
CREATE FUNCTION karta.osm_type_name(osm_type char)
RETURNS text
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
AS $$
SELECT CASE osm_type WHEN 'N' THEN 'node' WHEN 'W' THEN 'way' WHEN 'R' THEN 'relation' END
$$;
