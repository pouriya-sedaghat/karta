-- Release metadata, written by the importer after validation, and read-only
-- access for the serving role. The release database is immutable after
-- import: the reader role can only SELECT and execute karta functions, and
-- the importer additionally sets the database default to read-only.
CREATE TABLE karta.release_info (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    release_id text NOT NULL,
    schema_major integer NOT NULL,
    schema_revision text NOT NULL,
    style_revision text NOT NULL,
    region_id text NOT NULL,
    region_name text NOT NULL,
    bbox double precision[] NOT NULL,
    center double precision[] NOT NULL,
    default_zoom double precision NOT NULL,
    minzoom smallint NOT NULL,
    maxzoom smallint NOT NULL,
    source_sha256 text NOT NULL,
    source_size bigint NOT NULL,
    data_timestamp timestamptz NOT NULL,
    data_timestamp_source text NOT NULL,
    provenance jsonb,
    imported_at timestamptz NOT NULL,
    importer_version text NOT NULL,
    osm2pgsql_version text NOT NULL,
    attribution text NOT NULL,
    license text NOT NULL,
    license_url text NOT NULL,
    layers jsonb NOT NULL,
    style jsonb NOT NULL,
    report jsonb NOT NULL,
    -- The canonical text the release id is the hash of (releaseid.Canonical),
    -- and the tool versions that produced the release.
    identity text NOT NULL,
    toolchain jsonb NOT NULL
);

GRANT USAGE ON SCHEMA karta TO karta_reader;
GRANT SELECT ON ALL TABLES IN SCHEMA karta TO karta_reader;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA karta TO karta_reader;
