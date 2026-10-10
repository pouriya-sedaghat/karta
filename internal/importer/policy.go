package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/pouriya-sedaghat/karta/internal/region"
	"github.com/pouriya-sedaghat/karta/internal/schema"
)

// ValidationRevision versions what the content checks mean: change it
// whenever a check is added or its semantics change, so a release validated
// by the old checks must pass the new ones before it is activated.
const ValidationRevision = "karta-validation/1"

// Policy is the content policy a release is validated against: the region
// file's min_counts, searches and tiles, under ValidationRevision, bound to
// the region identity they were evaluated for (id, name, box and view, the
// region-file part of a release id). A pass therefore stands only for that
// exact identity: an edit of the name, box or view alone changes the policy,
// so no recorded pass is current any more, and a release built for another
// identity cannot be revalidated against the file (it is a new build).
// validation.max_drop_fraction is relative to the release active at a switch
// and is checked again at every forward switch, so it is not part of the
// policy.
type Policy struct {
	SHA256 string
	// Canonical is the JSON the digest is computed over, recorded with each
	// evaluation.
	Canonical json.RawMessage
}

// policyRegion is the region identity a policy is bound to. Its floats are
// encoded as the shortest text that parses back to the same float64, so any
// change of the box or view changes the policy.
type policyRegion struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	BBox   [4]float64 `json:"bbox"`
	Center [2]float64 `json:"center"`
	Zoom   float64    `json:"zoom"`
}

// PolicyOf returns the content policy of a region configuration. Absent and
// empty checks are the same policy; the order of searches and tiles is kept.
func PolicyOf(cfg region.Config) Policy {
	v := cfg.Validation
	id := cfg.Identity()
	doc := struct {
		Revision  string               `json:"revision"`
		Region    policyRegion         `json:"region"`
		MinCounts map[string]int64     `json:"min_counts"`
		Search    []region.SearchCheck `json:"search"`
		Tiles     []region.TileCheck   `json:"tiles"`
	}{Revision: ValidationRevision, Region: policyRegion{ID: id.ID, Name: id.Name, BBox: id.BBox, Center: id.Center, Zoom: id.Zoom},
		MinCounts: v.MinCounts, Search: v.Search, Tiles: v.Tiles}
	if doc.MinCounts == nil {
		doc.MinCounts = map[string]int64{}
	}
	if doc.Search == nil {
		doc.Search = []region.SearchCheck{}
	}
	if doc.Tiles == nil {
		doc.Tiles = []region.TileCheck{}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		// Only a non-finite float fails, which region.Load refuses: such a
		// configuration has no policy any release can have passed.
		b = []byte(`{"unencodable":true}`)
	}
	sum := sha256.Sum256(b)
	return Policy{SHA256: hex.EncodeToString(sum[:]), Canonical: b}
}

// Revalidate evaluates an existing release database against the content
// checks of cfg, without importing anything: min_counts on counts (the row
// counts measured when the release was built; the database is immutable),
// and the tile layer contract, configured tiles and searches run against
// the database, read only, exactly as a build validates. The caller makes
// sure the release was built with this build's schema and style revisions,
// so the layer catalog applies.
func Revalidate(ctx context.Context, conn *pgx.Conn, cfg region.Config, counts map[string]int64) *Report {
	r := &Report{Counts: counts}
	validate(ctx, conn, cfg, schema.Layers(), r)
	return r
}

// Failed lists the checks that did not pass, as "name: detail".
func (r *Report) Failed() []string {
	var out []string
	for _, c := range r.Checks {
		if !c.Passed {
			out = append(out, c.Name+": "+c.Detail)
		}
	}
	return out
}
