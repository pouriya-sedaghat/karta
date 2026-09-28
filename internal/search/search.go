// Package search runs named-place and POI search against a release database.
//
// Scope: features with a name tag and a place/POI tag (see the flex config);
// streets, addresses and house numbers are not searchable.
//
// Matching uses karta.normalize() inside the release database, the same
// function that built the name index, so query and index normalization can
// never diverge. Results are ordered by, in turn:
//
//  1. match tier: exact (0), prefix (1), word prefix (2), substring (3);
//     queries shorter than 3 normalized characters use exact/prefix only
//  2. importance rank of the feature (lower first)
//  3. name class: name/name:<lang> before official/short/int names before
//     alt_name/old_name
//  4. larger area first (points have area 0)
//  5. shorter matched name first
//  6. OSM type (node, relation, way as N < R < W) and OSM id, ascending
//
// The last key is unique, so the order is total and deterministic.
package search

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// Request is a validated search request.
type Request struct {
	Query string
	Limit int
	// Lang restricts matches to name tags with this language suffix
	// (name:fa, alt_name:fa, ...) and prefers name:<lang> as display name.
	Lang string
	// BBox restricts results to label points inside west,south,east,north.
	BBox *[4]float64
}

// Match describes which name tag matched.
type Match struct {
	Key      string  `json:"key"`
	Value    string  `json:"value"`
	Language *string `json:"language"`
	Type     string  `json:"type"`
}

// Result is one search hit.
type Result struct {
	ID          string            `json:"id"`
	OSMType     string            `json:"osm_type"`
	OSMID       int64             `json:"osm_id"`
	DisplayName string            `json:"display_name"`
	Names       map[string]string `json:"names"`
	Category    string            `json:"category"`
	Subcategory string            `json:"subcategory"`
	Lon         float64           `json:"lon"`
	Lat         float64           `json:"lat"`
	BBox        [4]float64        `json:"bbox"`
	Rank        int               `json:"importance_rank"`
	Match       Match             `json:"match"`
}

// Querier is satisfied by pgx pools, connections and transactions.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var tierNames = []string{"exact", "prefix", "word_prefix", "substring"}

// Normalize applies the release's search normalization to s.
func Normalize(ctx context.Context, q Querier, s string) (string, error) {
	var out string
	err := q.QueryRow(ctx, `SELECT karta.normalize($1::text)`, s).Scan(&out)
	return out, err
}

// The candidate set is the union of a prefix range scan on the C-collated
// B-tree (any length) and a trigram substring scan (3+ characters); a
// feature matching several names keeps its best match.
const querySQL = `
WITH p AS MATERIALIZED (
    SELECT nq, char_length(nq) AS qlen,
           replace(replace(replace(nq, '\', '\\'), '%', '\%'), '_', '\_') AS esc
    FROM (SELECT karta.normalize($1::text) COLLATE "C" AS nq) s
), candidates AS (
    SELECT n.* FROM p JOIN karta.place_names n
        ON n.norm >= p.nq AND n.norm < p.nq || U&'\+10FFFF'
    WHERE p.qlen > 0
    UNION ALL
    SELECT n.* FROM p JOIN karta.place_names n ON n.norm LIKE '%' || p.esc || '%'
    WHERE p.qlen >= 3
), matched AS (
    SELECT c.*,
        CASE WHEN c.norm = p.nq THEN 0
             WHEN left(c.norm, p.qlen) = p.nq THEN 1
             WHEN strpos(c.norm, ' ' || p.nq) > 0 THEN 2
             ELSE 3 END AS tier
    FROM candidates c, p
    WHERE $2::text IS NULL OR c.lang = $2::text
), best AS (
    SELECT DISTINCT ON (osm_type, osm_id) *
    FROM matched
    ORDER BY osm_type, osm_id, tier, name_class, key_order, char_length(norm), key COLLATE "C", value COLLATE "C"
)
SELECT karta.osm_type_name(f.osm_type), f.osm_id,
       CASE WHEN $2::text IS NULL THEN f.display_name
            ELSE COALESCE(f.names->>('name:' || $2::text), f.display_name) END,
       jsonb_strip_nulls(jsonb_build_object(
           'name', f.names->'name', 'name:fa', f.names->'name:fa', 'name:en', f.names->'name:en',
           'name:' || COALESCE($2::text, 'en'), f.names->('name:' || COALESCE($2::text, 'en')))),
       f.category, f.subcategory, f.lon, f.lat, f.bbox, f.rank,
       b.key, b.lang, b.value, b.tier
FROM best b JOIN karta.features f USING (osm_type, osm_id)
WHERE $3::float8 IS NULL
   OR f.label_point && public.ST_Transform(public.ST_MakeEnvelope($3, $4, $5, $6, 4326), 3857)
ORDER BY b.tier, f.rank, b.name_class, f.area_m2 DESC, char_length(b.norm), f.osm_type, f.osm_id
LIMIT $7`

// Run executes a search. The caller validates the request.
func Run(ctx context.Context, q Querier, req Request) ([]Result, error) {
	var lang, w, s, e, n any
	if req.Lang != "" {
		lang = req.Lang
	}
	if req.BBox != nil {
		w, s, e, n = req.BBox[0], req.BBox[1], req.BBox[2], req.BBox[3]
	}
	rows, err := q.Query(ctx, querySQL, req.Query, lang, w, s, e, n, req.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := []Result{}
	for rows.Next() {
		var r Result
		var names []byte
		var bbox []float64
		var tier int
		if err := rows.Scan(&r.OSMType, &r.OSMID, &r.DisplayName, &names, &r.Category, &r.Subcategory,
			&r.Lon, &r.Lat, &bbox, &r.Rank, &r.Match.Key, &r.Match.Language, &r.Match.Value, &tier); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(names, &r.Names); err != nil {
			return nil, err
		}
		copy(r.BBox[:], bbox)
		r.ID = r.OSMType + "/" + strconv.FormatInt(r.OSMID, 10)
		r.Match.Type = tierNames[tier]
		results = append(results, r)
	}
	return results, rows.Err()
}
