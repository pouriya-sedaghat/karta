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
package releaseid

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
func Canonical(in Inputs) string {
	var b strings.Builder
	line := func(k, v string) { fmt.Fprintf(&b, "%s=%s\n", k, strconv.Quote(v)) }
	b.WriteString("karta-release-id/2\n")
	line("source_sha256", in.SourceSHA256)
	line("provenance_sha256", in.ProvenanceSHA256)
	line("data_timestamp", in.DataTimestamp.UTC().Format(time.RFC3339Nano))
	line("data_timestamp_source", in.DataTimestampSource)
	line("region.id", in.Region.ID)
	line("region.name", in.Region.Name)
	line("region.bbox", fmt.Sprintf("%s,%s,%s,%s", num(in.Region.BBox[0]), num(in.Region.BBox[1]), num(in.Region.BBox[2]), num(in.Region.BBox[3])))
	line("region.view.center", fmt.Sprintf("%s,%s", num(in.Region.Center[0]), num(in.Region.Center[1])))
	line("region.view.zoom", num(in.Region.Zoom))
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
	return b.String()
}

// Derive returns "r" followed by 24 hex characters (96 bits) of a SHA-256
// over the canonical encoding of the inputs.
func Derive(in Inputs) string {
	sum := sha256.Sum256([]byte(Canonical(in)))
	return "r" + hex.EncodeToString(sum[:])[:24]
}

// DatabaseName is the PostgreSQL database holding a release.
func DatabaseName(id string) string {
	return "karta_" + id
}

// Valid reports whether s is a well-formed release identifier.
func Valid(s string) bool {
	return Pattern.MatchString(s)
}

// num formats a coordinate or zoom with a fixed precision of 7 decimals
// (about 1 cm), so equal values always encode identically.
func num(f float64) string {
	return strconv.FormatFloat(f, 'f', 7, 64)
}
