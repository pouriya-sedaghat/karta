// Package releaseid derives the stable identifier of a release from every
// input that can change what the release serves: the source snapshot and its
// provenance, the data timestamp, the region definition (including the name
// and default view the manifest and style publish), the schema and style
// revisions, the attribution text, and the versions of the tools whose output
// ends up in tiles and search (osm2pgsql, PostgreSQL, PostGIS, GEOS, PROJ,
// ICU). Identical inputs always yield the same identifier; changing any of
// them yields a different one, so release-pinned URLs can be cached as
// immutable. Acceptance thresholds are deliberately excluded: they decide
// whether an import is accepted, not what it serves.
//
// The invariant is one-directional and exact: if an accepted input changes
// release-pinned output, it changes the identifier. The encoding therefore
// never rounds: the manifest and style publish the stored float64 values of
// the box, center and zoom at full precision, so the identifier hashes the
// same values losslessly.
package releaseid

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Pattern matches identifiers produced by Derive.
var Pattern = regexp.MustCompile(`^r[0-9a-f]{24}$`)

// Region is the output-affecting part of a region definition.
type Region struct {
	ID     string
	Name   string
	BBox   [4]float64
	Center [2]float64
	Zoom   float64
}

// Inputs are the values an identifier is derived from.
type Inputs struct {
	SourceSHA256 string
	// ProvenanceSHA256 is the digest of the provenance sidecar bytes, or ""
	// when the snapshot has none.
	ProvenanceSHA256    string
	DataTimestamp       time.Time
	DataTimestampSource string
	Region              Region
	SchemaRevision      string
	StyleRevision       string
	Attribution         string
	License             string
	LicenseURL          string
	// Toolchain maps a component to the version that produced the release,
	// e.g. "osm2pgsql" -> "1.11.0", "geos" -> "3.14.1-CAPI-1.20.5".
	Toolchain map[string]string
}

// Canonical is the versioned, unambiguous text the identifier hashes. It is
// stored with each release so the identifier can be recomputed and audited.
//
// Every value is encoded losslessly: strings are quoted, and floats are
// written as the shortest decimal that parses back to exactly the same
// float64, so two inputs share a canonical text only if all their values are
// identical. Non-finite floats are rejected: they cannot be served (JSON has
// no NaN or infinity) and would all encode alike.
func Canonical(in Inputs) (string, error) {
	r := in.Region
	floats := []struct {
		key  string
		vals []float64
	}{{"region.bbox", r.BBox[:]}, {"region.view.center", r.Center[:]}, {"region.view.zoom", []float64{r.Zoom}}}
	for _, fl := range floats {
		for _, f := range fl.vals {
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return "", fmt.Errorf("%w: %s contains %v", ErrNotFinite, fl.key, f)
			}
		}
	}
	var b strings.Builder
	line := func(k, v string) { fmt.Fprintf(&b, "%s=%s\n", k, strconv.Quote(v)) }
	b.WriteString("karta-release-id/3\n")
	line("source_sha256", in.SourceSHA256)
	line("provenance_sha256", in.ProvenanceSHA256)
	line("data_timestamp", in.DataTimestamp.UTC().Format(time.RFC3339Nano))
	line("data_timestamp_source", in.DataTimestampSource)
	line("region.id", r.ID)
	line("region.name", r.Name)
	for _, fl := range floats {
		line(fl.key, nums(fl.vals))
	}
	line("schema_revision", in.SchemaRevision)
	line("style_revision", in.StyleRevision)
	line("attribution", in.Attribution)
	line("license", in.License)
	line("license_url", in.LicenseURL)
	keys := make([]string, 0, len(in.Toolchain))
	for k := range in.Toolchain {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		line("toolchain."+k, in.Toolchain[k])
	}
	return b.String(), nil
}

// ErrNotFinite reports a NaN or infinite coordinate or zoom.
var ErrNotFinite = errors.New("release identity: value is not finite")

// Derive returns the identifier, "r" followed by 24 hex characters (96 bits)
// of a SHA-256 over the canonical encoding of the inputs, together with that
// encoding.
func Derive(in Inputs) (id, canonical string, err error) {
	canonical, err = Canonical(in)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return "r" + hex.EncodeToString(sum[:])[:24], canonical, nil
}

// DatabaseName is the PostgreSQL database holding a release.
func DatabaseName(id string) string {
	return "karta_" + id
}

// Valid reports whether s is a well-formed release identifier.
func Valid(s string) bool {
	return Pattern.MatchString(s)
}

// nums encodes floats losslessly: FormatFloat with precision -1 writes the
// fewest digits that parse back to exactly f, so distinct float64 values
// (including -0 and 0, which JSON also writes differently) never share an
// encoding.
func nums(fs []float64) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = strconv.FormatFloat(f, 'f', -1, 64)
	}
	return strings.Join(parts, ",")
}
