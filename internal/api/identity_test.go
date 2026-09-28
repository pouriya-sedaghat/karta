package api

import (
	"bytes"
	"io"
	"log/slog"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/glyphs"
	"github.com/pouriya-sedaghat/karta/internal/region"
	"github.com/pouriya-sedaghat/karta/internal/release"
	"github.com/pouriya-sedaghat/karta/internal/releaseid"
	"github.com/pouriya-sedaghat/karta/internal/schema"
	"github.com/pouriya-sedaghat/karta/internal/style"
)

// oneRelease serves a single loaded release without a database: the
// manifest and style handlers read only its metadata, catalog and style.
type oneRelease struct{ rel *release.Release }

func (o oneRelease) Active() *release.Release { return o.rel }
func (o oneRelease) Get(id string) (*release.Release, bool) {
	if id != o.rel.Info.ReleaseID {
		return nil, false
	}
	return o.rel, true
}
func (o oneRelease) Status() release.Status {
	return release.Status{Ready: true, Reason: release.ReasonReady, ReleaseID: o.rel.Info.ReleaseID}
}

const (
	testAttribution = "© OpenStreetMap contributors"
	testSource      = "7d0e69a2d5e1ad184ee48637626882bb8bcb7acd8e95123e05d0697ce216191e"
)

var testDataTimestamp = time.Date(2026, 9, 27, 20, 23, 36, 0, time.UTC)

// releaseIDFor derives the id the importer would give cfg, all other inputs
// fixed. cfg.Identity is the mapping the importer uses.
func releaseIDFor(t *testing.T, cfg region.Config) string {
	t.Helper()
	id, _, err := releaseid.Derive(releaseid.Inputs{
		SourceSHA256: testSource, DataTimestamp: testDataTimestamp, DataTimestampSource: "pbf_header",
		Region: cfg.Identity(), SchemaRevision: schema.Revision(), StyleRevision: style.Revision(),
		Attribution: testAttribution, License: "ODbL-1.0", LicenseURL: "https://www.openstreetmap.org/copyright",
		Toolchain: map[string]string{"osm2pgsql": "1.11.0", "postgresql": "18.6", "postgis": "3.6.4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// served returns the manifest and style the API serves for cfg imported as
// release id. The region values reach the handlers as the importer stores
// them (double precision, read back unchanged).
func served(t *testing.T, cfg region.Config, id string) (manifest, styleJSON []byte) {
	t.Helper()
	catalog := schema.Layers()
	rel := &release.Release{
		Info: release.Info{
			ReleaseID: id, SchemaMajor: schema.Major, SchemaRevision: schema.Revision(), StyleRevision: style.Revision(),
			RegionID: cfg.ID, RegionName: cfg.Name, BBox: cfg.BBox, Center: cfg.View.Center, Zoom: cfg.View.Zoom,
			MinZoom: catalog.MinZoom, MaxZoom: catalog.MaxZoom, SourceSHA256: testSource,
			DataTimestamp: testDataTimestamp, DataTimestampSource: "pbf_header", ImportedAt: testDataTimestamp,
			Attribution: testAttribution, License: "ODbL-1.0", LicenseURL: "https://www.openstreetmap.org/copyright",
		},
		Catalog: catalog,
		Style:   style.Template(),
	}
	g, err := glyphs.New()
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{PublicBaseURL: "http://localhost:8080", RequestTimeout: 5 * time.Second, WebDir: t.TempDir()},
		oneRelease{rel}, g, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	m := call(t, h, http.MethodGet, "/v1/manifest", nil, nil)
	s := call(t, h, http.MethodGet, "/v1/releases/"+id+"/style.json", nil, nil)
	if m.Code != http.StatusOK || s.Code != http.StatusOK {
		t.Fatalf("manifest %d, style %d: %s %s", m.Code, s.Code, m.Body, s.Body)
	}
	return m.Body.Bytes(), s.Body.Bytes()
}

// If an accepted region change alters the bytes served at a release-pinned
// URL, it must alter the release id. Each case changes one value by less than
// 1e-7 (the rounding of the previous encoding), stays a valid region that the
// same snapshot's header box accepts, and is checked against the bytes the
// real manifest and style handlers produce.
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
				t.Fatalf("different bytes at the same URL /v1/releases/%s/style.json", id)
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
