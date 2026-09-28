-- Indexes for tile and search queries. osm2pgsql already created a GiST index
-- on each geometry column; repair, clipping and the derived columns rewrote
-- many rows, so those indexes are rebuilt compactly.

REINDEX TABLE karta.roads;
REINDEX TABLE karta.waterways;
REINDEX TABLE karta.water;
REINDEX TABLE karta.landcover;
REINDEX TABLE karta.buildings;
REINDEX TABLE karta.features;

ALTER TABLE karta.features ADD PRIMARY KEY (osm_type, osm_id);
CREATE INDEX features_label_point_idx ON karta.features USING gist (label_point);

-- Prefix matches are a range scan on the "C"-collated B-tree (works for any
-- query length); word-prefix and substring matches (3+ characters) use trigrams.
CREATE INDEX place_names_norm_idx ON karta.place_names (norm);
CREATE INDEX place_names_norm_trgm_idx ON karta.place_names USING gin (norm public.gin_trgm_ops);
CREATE INDEX place_names_feature_idx ON karta.place_names (osm_type, osm_id);
