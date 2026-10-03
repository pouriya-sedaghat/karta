package api

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/glyphs"
	"github.com/pouriya-sedaghat/karta/internal/region"
	"github.com/pouriya-sedaghat/karta/internal/style"
)

func TestRequestMetrics(t *testing.T) {
	cfg := region.Config{ID: "fixture", Name: "Fixture", BBox: [4]float64{0, 0, 0.02, 0.015},
		View: region.View{Center: [2]float64{0.01, 0.0075}, Zoom: 15}}
	id := releaseIDFor(t, cfg)
	rel := testRelease(cfg, id)
	g, err := glyphs.New()
	if err != nil {
		t.Fatal(err)
	}
	m := NewMetrics()
	h, err := newHandler(Config{PublicBaseURL: "http://localhost:8080", RequestTimeout: 5 * time.Second, Metrics: m},
		oneRelease{rel}, g, slog.New(slog.NewTextHandler(io.Discard, nil)), style.Render)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if rec := call(t, h, http.MethodGet, "/v1/manifest", nil, nil); rec.Code != http.StatusOK {
			t.Fatalf("manifest %d", rec.Code)
		}
	}
	call(t, h, http.MethodGet, "/v1/releases/"+id+"/style.json", nil, nil)                // 307
	call(t, h, http.MethodGet, "/v1/no-such-thing/secret-path", nil, nil)                 // 404
	call(t, h, http.MethodPost, "/v1/manifest", strings.NewReader("x"), nil)              // 405, answered by the middleware
	call(t, h, http.MethodGet, "/v1/search?q=very-private-search-term&limit=1", nil, nil) // 400 or 503 here: no database
	call(t, h, http.MethodGet, "/health/ready", nil, nil)

	rec := call(t, m.Handler(oneRelease{rel}), http.MethodGet, "/metrics", nil, nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("%d %s", rec.Code, rec.Header())
	}
	for _, want := range []string{
		`karta_api_ready 1`,
		`karta_api_readiness_info{reason="ready"} 1`,
		`karta_api_release_info{release_id="` + id + `"} 1`,
		`karta_api_requests_total{code="2xx",route="manifest"} 3`,
		`karta_api_requests_total{code="3xx",route="style_redirect"} 1`,
		`karta_api_requests_total{code="4xx",route="not_found"} 1`,
		`karta_api_requests_total{code="4xx",route="refused"} 1`,
		`karta_api_requests_total{code="2xx",route="health_ready"} 1`,
		`karta_api_request_duration_seconds_count{route="manifest"} 3`,
		`karta_api_request_duration_seconds_bucket{route="manifest",le="+Inf"} 3`,
		`karta_api_requests_in_flight 0`,
		"# TYPE karta_api_requests_total counter",
		"# TYPE karta_api_request_duration_seconds histogram",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if !strings.Contains(body, `route="search"`) {
		t.Error("the search request is not counted")
	}
	// Labels carry routes, never paths, queries or search terms.
	for _, leak := range []string{"very-private-search-term", "secret-path", "/v1/"} {
		if strings.Contains(body, leak) {
			t.Errorf("metrics expose %q", leak)
		}
	}
}

func TestStatusClass(t *testing.T) {
	for code, want := range map[int]string{200: "2xx", 204: "2xx", 304: "3xx", 410: "4xx", 503: "5xx"} {
		if got := statusClass(code); got != want {
			t.Errorf("%d: %s", code, got)
		}
	}
}
