package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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
func (o oneRelease) Lookup(id string) (*release.Release, release.Lookup) {
	if id != o.rel.Info.ReleaseID {
		return nil, release.Unknown
	}
	return o.rel, release.Served
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

// testRelease is cfg imported as release id, as release.Manager loads it:
// the region values as stored (double precision, read back unchanged), the
// layer catalog and the stored style template.
func testRelease(cfg region.Config, id string) *release.Release {
	catalog := schema.Layers()
	return &release.Release{
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
}

// testHandler is an API server for rel with the given public base URL and
// style renderer; each call is a fresh process as far as the API is concerned.
func testHandler(t *testing.T, rel *release.Release, baseURL string, render styleRenderer) http.Handler {
	t.Helper()
	g, err := glyphs.New()
	if err != nil {
		t.Fatal(err)
	}
	h, err := newHandler(Config{PublicBaseURL: baseURL, RequestTimeout: 5 * time.Second, WebDir: t.TempDir()},
		oneRelease{rel}, g, slog.New(slog.NewTextHandler(io.Discard, nil)), render)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// getURL requests an absolute URL the API issued (or a path) from h.
func getURL(t *testing.T, h http.Handler, target string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	return call(t, h, http.MethodGet, u.RequestURI(), nil, header)
}

// testManifest is the part of the manifest the style tests read.
type testManifest struct {
	Release struct {
		ReleaseID string `json:"release_id"`
	} `json:"release"`
	StyleURL string `json:"style_url"`
	Tiles    struct {
		URLTemplate string `json:"url_template"`
	} `json:"tiles"`
}

func manifestOf(t *testing.T, h http.Handler) (testManifest, *httptest.ResponseRecorder) {
	t.Helper()
	rec := call(t, h, http.MethodGet, "/v1/manifest", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest %d: %s", rec.Code, rec.Body)
	}
	var m testManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m, rec
}

// served returns the manifest and style the API serves for cfg imported as
// release id: the style is fetched at the URL the manifest names.
func served(t *testing.T, cfg region.Config, id string) (manifest, styleJSON []byte) {
	t.Helper()
	h := testHandler(t, testRelease(cfg, id), "http://localhost:8080", style.Render)
	m, rec := manifestOf(t, h)
	s := getURL(t, h, m.StyleURL, nil)
	if s.Code != http.StatusOK {
		t.Fatalf("style %d: %s", s.Code, s.Body)
	}
	return rec.Body.Bytes(), s.Body.Bytes()
}
