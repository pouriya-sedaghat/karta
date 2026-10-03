package region

import (
	"errors"
	"math"
)

// Draft returns a region configuration for a snapshot whose box (from its
// header or its provenance sidecar) and SHA-256 are known: the box exactly
// as the snapshot states it (Verify compares the two within 1e-7 degrees),
// the digest pinned, a default view centred in the box at a zoom that shows
// it whole, and no validation checks: those must come from a measured
// import of this snapshot, never be guessed or scaled from another region
// (docs/operations.md, "The Iran region").
func Draft(id, name string, box [4]float64, sha256 string, requireProvenance bool) (Config, error) {
	if err := ValidBBox(box); err != nil {
		return Config{}, err
	}
	span := math.Max(box[2]-box[0], box[3]-box[1])
	zoom := 0.0
	if span > 0 {
		zoom = math.Max(0, math.Min(14, math.Floor(math.Log2(360/span))))
	}
	c := Config{
		ID: id, Name: name, BBox: box,
		Source:     Source{ExpectedSHA256: sha256, RequireProvenance: requireProvenance},
		View:       View{Center: [2]float64{(box[0] + box[2]) / 2, (box[1] + box[3]) / 2}, Zoom: zoom},
		Validation: Validation{MinCounts: map[string]int64{}, Search: []SearchCheck{}, Tiles: []TileCheck{}},
	}
	if sha256 == "" {
		return Config{}, errors.New("the snapshot digest is required")
	}
	return c, c.Validate()
}
