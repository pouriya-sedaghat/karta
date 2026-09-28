package api

import (
	"bytes"
	"math"
	"testing"

	"github.com/pouriya-sedaghat/karta/internal/region"
)

// If an accepted region change alters what a release serves, it must alter
// the release id, which keys the tile URLs and pinned searches (the style URL
// is additionally keyed by its own bytes). Each case changes one value by
// less than 1e-7 (the rounding of the previous encoding), stays a valid
// region that the same snapshot's header box accepts, and is checked against
// the bytes the real manifest and style handlers produce.
func TestSubE7RegionChangesNeverReuseAReleaseURL(t *testing.T) {
	base := region.Config{
		ID: "tehran-chitgar", Name: "Chitgar Lake area, Tehran (development sample)",
		BBox: [4]float64{51.175, 35.705, 51.285, 35.785},
		View: region.View{Center: [2]float64{51.215, 35.745}, Zoom: 13.00000001},
	}
	up := func(f *float64) { *f = math.Nextafter(*f, math.Inf(1)) }
	down := func(f *float64) { *f = math.Nextafter(*f, math.Inf(-1)) }
	cases := map[string]func(*region.Config){
		"zoom 13.00000001 to 13.00000002": func(c *region.Config) { c.View.Zoom = 13.00000002 },
		"zoom next float":                 func(c *region.Config) { up(&c.View.Zoom) },
		"center lon +1e-8":                func(c *region.Config) { c.View.Center[0] += 1e-8 },
		"center lat next float":           func(c *region.Config) { down(&c.View.Center[1]) },
		"bbox west +1e-8":                 func(c *region.Config) { c.BBox[0] += 1e-8 },
		"bbox south next float":           func(c *region.Config) { up(&c.BBox[1]) },
		"bbox east -1e-8":                 func(c *region.Config) { c.BBox[2] -= 1e-8 },
		"bbox north next float":           func(c *region.Config) { down(&c.BBox[3]) },
	}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	baseID := releaseIDFor(t, base)
	baseManifest, baseStyle := served(t, base, baseID)
	seen := map[string]string{baseID: "base"}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := base
			mutate(&c)
			if err := c.Validate(); err != nil {
				t.Fatalf("changed region is not accepted: %v", err)
			}
			if !region.SameBBox(base.BBox, c.BBox) {
				t.Fatal("changed box would not match the same snapshot header")
			}
			// Served under the id the old encoding gave both, the bytes differ.
			manifest, styleJSON := served(t, c, baseID)
			if bytes.Equal(manifest, baseManifest) || bytes.Equal(styleJSON, baseStyle) {
				t.Fatalf("change is not observable:\n%s\n%s", manifest, styleJSON)
			}
			id := releaseIDFor(t, c)
			if id == baseID {
				t.Fatalf("release %s names different output (tile URLs /v1/releases/%s/tiles/...)", id, id)
			}
			if other, dup := seen[id]; dup {
				t.Errorf("same id as %s", other)
			}
			seen[id] = name
			// Under its own id the style names only its own URLs.
			_, own := served(t, c, id)
			if bytes.Contains(own, []byte(baseID)) || !bytes.Contains(own, []byte("/v1/releases/"+id+"/tiles/")) {
				t.Errorf("style is not pinned to %s: %s", id, own)
			}
		})
	}
}
