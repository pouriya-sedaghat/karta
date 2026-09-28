// Package releaseid derives the stable identifier of a release from its
// validated inputs: the source snapshot digest, the region definition and
// the schema and style revisions. Identical inputs always yield the same
// identifier; changing any of them yields a different one.
package releaseid

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
)

// Pattern matches identifiers produced by Derive.
var Pattern = regexp.MustCompile(`^r[0-9a-f]{24}$`)

// Inputs are the values an identifier is derived from.
type Inputs struct {
	SourceSHA256   string
	RegionID       string
	RegionBBox     [4]float64
	SchemaRevision string
	StyleRevision  string
}

// Derive returns "r" followed by 24 hex characters (96 bits) of a SHA-256
// over a canonical, versioned encoding of the inputs.
func Derive(in Inputs) string {
	canonical := fmt.Sprintf("karta-release-id/1\nsource_sha256=%s\nregion=%s\nbbox=%s,%s,%s,%s\nschema=%s\nstyle=%s\n",
		in.SourceSHA256, in.RegionID,
		num(in.RegionBBox[0]), num(in.RegionBBox[1]), num(in.RegionBBox[2]), num(in.RegionBBox[3]),
		in.SchemaRevision, in.StyleRevision)
	sum := sha256.Sum256([]byte(canonical))
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

func num(f float64) string {
	return strconv.FormatFloat(f, 'f', 7, 64)
}
