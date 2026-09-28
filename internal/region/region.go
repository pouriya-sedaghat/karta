// Package region loads the configuration that describes one importable
// geographic region: its box, the source snapshot it accepts and the
// acceptance checks an import must pass before the release can be served.
package region

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
)

var (
	idPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Config is one region file, e.g. config/regions/tehran-chitgar.json.
type Config struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// BBox is west, south, east, north in WGS84. Imported data is clipped to it.
	BBox   [4]float64 `json:"bbox"`
	Source Source     `json:"source"`
	View   View       `json:"view"`
	// Validation is run against the imported candidate; any failure aborts
	// the import and leaves no release behind.
	Validation Validation `json:"validation"`
}

// Source constrains which snapshot may be imported for the region.
type Source struct {
	// ExpectedSHA256 pins one exact snapshot. Empty accepts any snapshot.
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	// RequireProvenance demands a <file>.provenance.json sidecar whose digest
	// and box match the file and this region.
	RequireProvenance bool `json:"require_provenance"`
}

// View is the default map position for clients.
type View struct {
	Center [2]float64 `json:"center"`
	Zoom   float64    `json:"zoom"`
}

// Validation lists acceptance checks.
type Validation struct {
	// MinCounts are lower bounds on imported rows per table (sanity gates).
	MinCounts map[string]int64 `json:"min_counts"`
	Search    []SearchCheck    `json:"search"`
	Tiles     []TileCheck      `json:"tiles"`
}

// SearchCheck requires a query to return an element within the first
// MaxPosition results.
type SearchCheck struct {
	Query       string `json:"q"`
	Lang        string `json:"lang,omitempty"`
	OSMType     string `json:"osm_type"`
	OSMID       int64  `json:"osm_id"`
	MaxPosition int    `json:"max_position"`
}

// TileCheck requires the tile containing a point at a zoom to contain layers.
type TileCheck struct {
	Lon    float64  `json:"lon"`
	Lat    float64  `json:"lat"`
	Zoom   int      `json:"z"`
	Layers []string `json:"layers"`
}

// Tables whose rows may be bounded by MinCounts.
var countableTables = map[string]bool{
	"roads": true, "water": true, "waterways": true, "landcover": true,
	"buildings": true, "features": true, "place_names": true,
}

// Load reads and validates a region file. Unknown fields are rejected so a
// typo cannot silently disable a check.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- operator-supplied configuration path
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Validate checks the configuration for consistency.
func (c Config) Validate() error {
	var errs []error
	if !idPattern.MatchString(c.ID) {
		errs = append(errs, fmt.Errorf("id %q must match %s", c.ID, idPattern))
	}
	if c.Name == "" {
		errs = append(errs, errors.New("name is required"))
	}
	if err := ValidBBox(c.BBox); err != nil {
		errs = append(errs, err)
	}
	if s := c.Source.ExpectedSHA256; s != "" && !sha256Pattern.MatchString(s) {
		errs = append(errs, errors.New("source.expected_sha256 must be 64 lowercase hex characters"))
	}
	lon, lat := c.View.Center[0], c.View.Center[1]
	if lon < c.BBox[0] || lon > c.BBox[2] || lat < c.BBox[1] || lat > c.BBox[3] {
		errs = append(errs, errors.New("view.center must lie inside bbox"))
	}
	if c.View.Zoom < 0 || c.View.Zoom > 22 {
		errs = append(errs, errors.New("view.zoom must be 0..22"))
	}
	for table, n := range c.Validation.MinCounts {
		if !countableTables[table] {
			errs = append(errs, fmt.Errorf("validation.min_counts: unknown table %q", table))
		}
		if n < 0 {
			errs = append(errs, fmt.Errorf("validation.min_counts.%s must be >= 0", table))
		}
	}
	for i, s := range c.Validation.Search {
		if s.Query == "" || (s.OSMType != "node" && s.OSMType != "way" && s.OSMType != "relation") || s.OSMID <= 0 || s.MaxPosition < 1 {
			errs = append(errs, fmt.Errorf("validation.search[%d] needs q, osm_type (node|way|relation), osm_id and max_position >= 1", i))
		}
	}
	for i, t := range c.Validation.Tiles {
		if t.Zoom < 0 || t.Zoom > 16 || len(t.Layers) == 0 ||
			t.Lon < c.BBox[0] || t.Lon > c.BBox[2] || t.Lat < c.BBox[1] || t.Lat > c.BBox[3] {
			errs = append(errs, fmt.Errorf("validation.tiles[%d] needs a point inside bbox, z 0..16 and layers", i))
		}
	}
	return errors.Join(errs...)
}

// ValidBBox checks a west,south,east,north WGS84 box within Web Mercator's range.
func ValidBBox(b [4]float64) error {
	for _, v := range b {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("bbox values must be finite")
		}
	}
	if !(b[0] >= -180 && b[0] < b[2] && b[2] <= 180 && b[1] >= -85.05112878 && b[1] < b[3] && b[3] <= 85.05112878) {
		return errors.New("bbox must be west,south,east,north with west<east, south<north inside the Web Mercator range")
	}
	return nil
}

// SameBBox compares boxes with a tolerance of 1e-7 degrees (about 1 cm),
// the precision of OSM coordinates.
func SameBBox(a, b [4]float64) bool {
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-7 {
			return false
		}
	}
	return true
}
