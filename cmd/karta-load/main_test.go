package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTileXY(t *testing.T) {
	// The tiles the integration and browser tests use over the fixture and
	// Chitgar (docs/development-data.md).
	for _, c := range []struct {
		lon, lat float64
		z, x, y  int
	}{
		{0.01, 0.0075, 14, 8192, 8191},
		{51.21494, 35.74558, 14, 10522, 6448},
		{51.2054, 35.7215, 15, 21044, 12898},
		{-180, 85.05, 2, 0, 0},
		{180, -85.05, 2, 3, 3}, // clamped to the last tile
	} {
		if x, y := tileXY(c.lon, c.lat, c.z); x != c.x || y != c.y {
			t.Errorf("%g,%g z%d: %d/%d, want %d/%d", c.lon, c.lat, c.z, x, y, c.x, c.y)
		}
	}
}

func TestParseMix(t *testing.T) {
	m, err := parseMix("tile=7, search=2,manifest=1")
	if err != nil || m["tile"] != 7 || m["search"] != 2 || m["manifest"] != 1 {
		t.Fatalf("%v %v", m, err)
	}
	for _, bad := range []string{"tile", "tile=x", "tiles=1", "tile=0", "tile=-1"} {
		if _, err := parseMix(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A run against a fake API: requests are pinned, spread over the mix and
// the box, and errors are counted.
func TestRun(t *testing.T) {
	var tiles, searches, pinned, bad atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/manifest", func(w http.ResponseWriter, r *http.Request) {
		// Absolute URLs under a public base the load tool does not use.
		_, _ = w.Write([]byte(`{"release":{"release_id":"r0123456789abcdef01234567"},
			"style_url":"https://maps.example/v1/releases/r0123456789abcdef01234567/style.json",
			"tiles":{"url_template":"https://maps.example/v1/releases/r0123456789abcdef01234567/tiles/{z}/{x}/{y}.pbf",
			"minzoom":0,"maxzoom":16,"bounds":[51.175,35.705,51.285,35.785]},
			"search":{"url":"https://maps.example/v1/search"}}`))
	})
	mux.HandleFunc("GET /v1/releases/r0123456789abcdef01234567/tiles/{z}/{x}/{y}", func(w http.ResponseWriter, r *http.Request) {
		tiles.Add(1)
		if r.PathValue("z") == "16" {
			bad.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/search", func(w http.ResponseWriter, r *http.Request) {
		searches.Add(1)
		if r.URL.Query().Get("release_id") == "r0123456789abcdef01234567" {
			pinned.Add(1)
		}
		_, _ = w.Write([]byte(`{"release_id":"r0123456789abcdef01234567","results":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s, err := run(context.Background(), options{base: srv.URL, duration: 500 * time.Millisecond, concurrency: 4,
		mix: map[string]int{"tile": 3, "search": 1}, minZoom: 15, maxZoom: 16, queries: []string{"دریاچه"}, timeout: time.Second, seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	if s.Requests == 0 || s.Kinds["tile"].Requests == 0 || s.Kinds["search"].Requests == 0 {
		t.Fatalf("summary %+v", s)
	}
	if int64(s.Kinds["search"].Requests) > searches.Load() || pinned.Load() != searches.Load() {
		t.Errorf("searches %d, pinned %d", searches.Load(), pinned.Load())
	}
	if s.Errors == 0 || s.Kinds["tile"].Statuses["503"] == 0 || s.Kinds["tile"].Statuses["204"] == 0 || len(s.ErrorSamples) == 0 {
		t.Errorf("errors not counted: %+v", s.Kinds["tile"])
	}
	if s.ReleaseID != "r0123456789abcdef01234567" || len(s.ReleasesAnswer) != 1 || s.MinZoom != 15 || s.MaxZoom != 16 {
		t.Errorf("%+v", s)
	}
	b, _ := json.Marshal(s)
	if !strings.Contains(string(b), `"p99_ms"`) || s.Mode != "closed_loop" {
		t.Errorf("%s", b)
	}

	// Open loop: a rate above what four slow workers can serve leaves
	// requests late.
	slow := http.NewServeMux()
	slow.HandleFunc("GET /v1/manifest", mux.ServeHTTP)
	slow.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) { time.Sleep(50 * time.Millisecond) })
	srv2 := httptest.NewServer(slow)
	defer srv2.Close()
	s, err = run(context.Background(), options{base: srv2.URL, duration: 500 * time.Millisecond, concurrency: 4, rate: 400,
		mix: map[string]int{"tile": 1}, minZoom: 10, maxZoom: 12, timeout: time.Second, seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if s.Mode != "open_loop" || s.Late == 0 {
		t.Errorf("saturation not detected: %+v", s)
	}
}
