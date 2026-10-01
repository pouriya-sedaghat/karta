package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/pouriya-sedaghat/karta/internal/glyphs"
	"github.com/pouriya-sedaghat/karta/internal/region"
	"github.com/pouriya-sedaghat/karta/internal/registry"
	"github.com/pouriya-sedaghat/karta/internal/releaseid"
	"github.com/pouriya-sedaghat/karta/internal/style"
	"github.com/pouriya-sedaghat/karta/openapi"
)

// freshRelease is oneRelease with a registry freshness setting, as
// release.Manager reports it.
type freshRelease struct {
	oneRelease
	f registry.Freshness
}

func (f freshRelease) Freshness() registry.Freshness { return f.f }

func TestManifestFreshness(t *testing.T) {
	cfg := region.Config{ID: "fixture", Name: "Fixture", BBox: [4]float64{0, 0, 0.02, 0.015},
		View: region.View{Center: [2]float64{0.01, 0.0075}, Zoom: 15}}
	rel := testRelease(cfg, releaseIDFor(t, cfg))
	if !releaseid.Valid(rel.Info.ReleaseID) {
		t.Fatal("bad test release id")
	}
	g, err := glyphs.New()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi3.NewLoader().LoadFromData(openapi.Spec)
	if err != nil {
		t.Fatal(err)
	}
	doc.Servers = openapi3.Servers{{URL: "http://localhost:8080"}}
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	long, short := 1000000*time.Hour, time.Hour
	type fresh struct {
		UpdateMode        string `json:"update_mode"`
		StaleAfterSeconds *int64 `json:"stale_after_seconds"`
		Stale             *bool  `json:"stale"`
	}
	for _, c := range []struct {
		name      string
		releases  Releases
		mode      string
		online    bool
		threshold *int64
		stale     *bool
	}{
		{"Stage 2 manager or no registry row", oneRelease{rel}, "manual", false, nil, nil},
		{"manual with a threshold", freshRelease{oneRelease{rel}, registry.Freshness{UpdateMode: "manual", StaleAfter: &long}}, "manual", false, ptrI(3600000000), ptrB(false)},
		{"online, no threshold", freshRelease{oneRelease{rel}, registry.Freshness{UpdateMode: "online"}}, "online", true, nil, nil},
		{"online and stale", freshRelease{oneRelease{rel}, registry.Freshness{UpdateMode: "online", StaleAfter: &short}}, "online", true, ptrI(3600), ptrB(true)},
		{"unknown mode reads as manual", freshRelease{oneRelease{rel}, registry.Freshness{UpdateMode: "other"}}, "manual", false, nil, nil},
	} {
		h, err := newHandler(Config{PublicBaseURL: "http://localhost:8080", RequestTimeout: 5 * time.Second}, c.releases, g,
			slog.New(slog.NewTextHandler(io.Discard, nil)), style.Render)
		if err != nil {
			t.Fatal(err)
		}
		rec := call(t, h, http.MethodGet, "/v1/manifest", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
		var m struct {
			Capabilities map[string]bool `json:"capabilities"`
			Freshness    fresh           `json:"freshness"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		f := m.Freshness
		if f.UpdateMode != c.mode || m.Capabilities["online_updates"] != c.online || !eqI(f.StaleAfterSeconds, c.threshold) || !eqB(f.Stale, c.stale) {
			t.Errorf("%s: %s", c.name, rec.Body)
		}
		// The response matches the published contract.
		req, _ := http.NewRequest(http.MethodGet, "http://localhost:8080/v1/manifest", nil)
		route, params, err := router.FindRoute(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
			Status:                 rec.Code, Header: rec.Header(), Body: io.NopCloser(rec.Body),
		}); err != nil {
			t.Errorf("%s: manifest does not match openapi.yaml: %v", c.name, err)
		}
	}
}

func ptrI(v int64) *int64 { return &v }
func ptrB(v bool) *bool   { return &v }

func eqI(a, b *int64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
func eqB(a, b *bool) bool  { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
