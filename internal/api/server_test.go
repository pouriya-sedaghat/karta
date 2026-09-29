package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/glyphs"
	"github.com/pouriya-sedaghat/karta/internal/release"
)

// noReleases is a release source with nothing loaded; every handler path
// that validates input before touching a database can be tested with it.
type noReleases struct{}

func (noReleases) Active() *release.Release            { return nil }
func (noReleases) Get(string) (*release.Release, bool) { return nil, false }
func (noReleases) Status() release.Status {
	return release.Status{Reason: release.ReasonNoRelease, Detail: "none", CheckedAt: time.Unix(0, 0)}
}

func newServer(t *testing.T) http.Handler {
	t.Helper()
	g, err := glyphs.New()
	if err != nil {
		t.Fatal(err)
	}
	web := t.TempDir()
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<!doctype html><title>x</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(web, "vendor"), 0o700); err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{
		PublicBaseURL: "http://localhost:8080", RequestTimeout: 5 * time.Second,
		CORSAllowedOrigins: []string{"https://app.example"}, WebDir: web,
	}, noReleases{}, g, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type errBody struct {
	Error struct {
		Code, Message, Parameter, RequestID string
	} `json:"error"`
}

func call(t *testing.T, h http.Handler, method, target string, body io.Reader, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func expectErr(t *testing.T, rec *httptest.ResponseRecorder, status int, code, param string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
	}
	var e errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body is not JSON: %s", rec.Body)
	}
	if e.Error.Code != code || e.Error.Parameter != param || e.Error.Message == "" {
		t.Fatalf("error %+v, want %s/%s", e.Error, code, param)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("headers %v", rec.Header())
	}
	if rec.Header().Get("X-Request-ID") == "" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing standard headers %v", rec.Header())
	}
}

func TestSearchValidation(t *testing.T) {
	h := newServer(t)
	cases := []struct {
		query, code, param string
		status             int
	}{
		{"", CodeInvalidParameter, "q", 400},
		{"q=%20", CodeInvalidParameter, "q", 400},
		{"q=" + strings.Repeat("x", 101), CodeInvalidParameter, "q", 400},
		{"q=" + strings.Repeat("%D8%A7", 101), CodeInvalidParameter, "q", 400}, // 101 Arabic letters
		{"q=%FF%FE", CodeInvalidParameter, "q", 400},                           // invalid UTF-8
		{"q=a%00b", CodeInvalidParameter, "q", 400},
		{"q=a&limit=0", CodeInvalidParameter, "limit", 400},
		{"q=a&limit=-1", CodeInvalidParameter, "limit", 400},
		{"q=a&limit=51", CodeInvalidParameter, "limit", 400},
		{"q=a&lang=f", CodeInvalidParameter, "lang", 400},
		{"q=a&lang=fa-IR", CodeInvalidParameter, "lang", 400},
		{"q=a&bbox=0,0,1", CodeInvalidParameter, "bbox", 400},
		{"q=a&bbox=0,0,1,Inf", CodeInvalidParameter, "bbox", 400},
		{"q=a&bbox=10,0,1,1", CodeInvalidParameter, "bbox", 400},
		{"q=a&bbox=0,-90,1,1", CodeInvalidParameter, "bbox", 400},
		{"q=a&q=b", CodeInvalidParameter, "q", 400},
		{"q=a&sort=name", CodeUnknownParameter, "sort", 400},
		{"q=a&release_id=r1", CodeInvalidParameter, "release_id", 400},
		{"q=a&release_id=r0123456789abcdef01234567", CodeUnknownRelease, "release_id", 404},
		{"q=a", CodeNoActiveRelease, "", 503},
		{"q=%zz", CodeInvalidParameter, "", 400},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			expectErr(t, call(t, h, "GET", "/v1/search?"+c.query, nil, nil), c.status, c.code, c.param)
		})
	}
}

func TestRequestRestrictions(t *testing.T) {
	h := newServer(t)
	rec := call(t, h, "POST", "/v1/search?q=a", strings.NewReader("{}"), nil)
	expectErr(t, rec, 405, CodeMethodNotAllowed, "")
	if rec.Header().Get("Allow") != "GET, HEAD, OPTIONS" {
		t.Errorf("Allow %q", rec.Header().Get("Allow"))
	}
	expectErr(t, call(t, h, "GET", "/v1/search?q=a", strings.NewReader("x"), nil), 413, CodeBodyNotAllowed, "")
	expectErr(t, call(t, h, "GET", "/nope", nil, nil), 404, CodeNotFound, "")
	expectErr(t, call(t, h, "GET", "/v1/manifest", nil, nil), 503, CodeNoActiveRelease, "")
	expectErr(t, call(t, h, "GET", "/v1/manifest?x=1", nil, nil), 400, CodeUnknownParameter, "")
	if rec := call(t, h, "GET", "/health/ready", nil, nil); rec.Code != 503 || !strings.Contains(rec.Body.String(), `"reason":"no_active_release"`) {
		t.Errorf("ready %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, "GET", "/health/live", nil, nil); rec.Code != 200 {
		t.Errorf("live %d", rec.Code)
	}
	// A client-supplied request id is echoed only when it is well-formed.
	if rec := call(t, h, "GET", "/health/live", nil, map[string]string{"X-Request-ID": "abc-123"}); rec.Header().Get("X-Request-ID") != "abc-123" {
		t.Errorf("request id %q", rec.Header().Get("X-Request-ID"))
	}
	if rec := call(t, h, "GET", "/health/live", nil, map[string]string{"X-Request-ID": "a b\r\nc"}); rec.Header().Get("X-Request-ID") == "a b\r\nc" {
		t.Error("malformed request id echoed")
	}
}

func TestTileAndStyleValidation(t *testing.T) {
	h := newServer(t)
	id := "r0123456789abcdef01234567"
	expectErr(t, call(t, h, "GET", "/v1/releases/latest/tiles/0/0/0.pbf", nil, nil), 400, CodeInvalidParameter, "release_id")
	expectErr(t, call(t, h, "GET", "/v1/releases/"+id+"/tiles/0/0/0.pbf", nil, nil), 404, CodeUnknownRelease, "release_id")
	expectErr(t, call(t, h, "GET", "/v1/releases/"+id+"/tiles/0/0/0.png", nil, nil), 404, CodeNotFound, "")
	expectErr(t, call(t, h, "GET", "/v1/releases/"+id+"/style.json", nil, nil), 404, CodeUnknownRelease, "release_id")
	expectErr(t, call(t, h, "GET", "/v1/releases/"+id+"/style.json?v=2", nil, nil), 400, CodeUnknownParameter, "")
}

func TestGlyphEndpoint(t *testing.T) {
	h := newServer(t)
	rec := call(t, h, "GET", "/v1/fonts/Vazirmatn%20Regular/65024-65279.pbf", nil, nil)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/x-protobuf" || rec.Body.Len() < 1000 {
		t.Fatalf("glyphs %d %v %d bytes", rec.Code, rec.Header(), rec.Body.Len())
	}
	etag := rec.Header().Get("ETag")
	if rec := call(t, h, "GET", "/v1/fonts/Vazirmatn%20Regular/65024-65279.pbf", nil, map[string]string{"If-None-Match": etag}); rec.Code != 304 {
		t.Errorf("If-None-Match: %d", rec.Code)
	}
	expectErr(t, call(t, h, "GET", "/v1/fonts/Comic%20Sans/0-255.pbf", nil, nil), 404, CodeUnknownFontstack, "fontstack")
	expectErr(t, call(t, h, "GET", "/v1/fonts/Vazirmatn%20Regular/0-300.pbf", nil, nil), 400, CodeInvalidParameter, "range")
	expectErr(t, call(t, h, "GET", "/v1/fonts/Vazirmatn%20Regular/65536-65791.pbf", nil, nil), 400, CodeInvalidParameter, "range")
}

func TestCORS(t *testing.T) {
	h := newServer(t)
	rec := call(t, h, "GET", "/health/live", nil, map[string]string{"Origin": "https://app.example"})
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Errorf("allowed origin: %v", rec.Header())
	}
	rec = call(t, h, "GET", "/health/live", nil, map[string]string{"Origin": "https://other.example"})
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("other origin allowed: %v", rec.Header())
	}
	rec = call(t, h, "OPTIONS", "/v1/search", nil, map[string]string{"Origin": "https://app.example", "Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": "if-none-match"})
	if rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Methods") != "GET, HEAD" || rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("preflight %d %v", rec.Code, rec.Header())
	}
}

func TestDemoFiles(t *testing.T) {
	h := newServer(t)
	rec := call(t, h, "GET", "/demo/", nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "connect-src 'self'") {
		t.Fatalf("demo %d %v", rec.Code, rec.Header())
	}
	if rec := call(t, h, "GET", "/", nil, nil); rec.Code != 302 || rec.Header().Get("Location") != "demo/" {
		t.Errorf("root %d %v", rec.Code, rec.Header())
	}
	for _, p := range []string{"/demo/vendor/", "/demo/missing.js", "/demo/secret.env", "/demo/%2e%2e/%2e%2e/etc/passwd"} {
		if rec := call(t, h, "GET", p, nil, nil); rec.Code != 404 && rec.Code != 301 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}

// Demo assets are validated by content: a changed file under the same URL
// and with the same modification time must not revalidate as unchanged.
func TestDemoValidatorsFollowContent(t *testing.T) {
	g, err := glyphs.New()
	if err != nil {
		t.Fatal(err)
	}
	web := t.TempDir()
	lib := filepath.Join(web, "vendor", "lib.js")
	if err := os.MkdirAll(filepath.Dir(lib), 0o700); err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(1985, 10, 26, 8, 15, 0, 0, time.UTC) // npm's fixed package mtime
	write := func(content string) {
		if err := os.WriteFile(lib, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(lib, fixed, fixed); err != nil {
			t.Fatal(err)
		}
	}
	h, err := New(Config{PublicBaseURL: "http://localhost:8080", RequestTimeout: 5 * time.Second, WebDir: web},
		noReleases{}, g, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	write("export const version = 1;")
	first := call(t, h, "GET", "/demo/vendor/lib.js", nil, nil)
	etag := first.Header().Get("ETag")
	if first.Code != 200 || etag == "" || first.Header().Get("Last-Modified") != "" {
		t.Fatalf("first: %d %v", first.Code, first.Header())
	}
	if rec := call(t, h, "GET", "/demo/vendor/lib.js", nil, map[string]string{"If-None-Match": etag}); rec.Code != 304 {
		t.Fatalf("unchanged file: %d, want 304", rec.Code)
	}
	write("export const version = 2;") // same size, same mtime, new content
	rec := call(t, h, "GET", "/demo/vendor/lib.js", nil, map[string]string{
		"If-None-Match": etag, "If-Modified-Since": fixed.Add(time.Hour).Format(http.TimeFormat),
	})
	if rec.Code != 200 || rec.Body.String() != "export const version = 2;" || rec.Header().Get("ETag") == etag {
		t.Fatalf("changed file: %d %q ETag %s (was %s)", rec.Code, rec.Body, rec.Header().Get("ETag"), etag)
	}
}

// The glyph validator changes with the generated bytes (see glyphs tests);
// here: it is not shared between ranges and a stale tag is not honoured.
func TestGlyphValidators(t *testing.T) {
	h := newServer(t)
	a := call(t, h, "GET", "/v1/fonts/Vazirmatn%20Regular/0-255.pbf", nil, nil)
	b := call(t, h, "GET", "/v1/fonts/Vazirmatn%20Regular/65024-65279.pbf", nil, nil)
	if a.Header().Get("ETag") == "" || a.Header().Get("ETag") == b.Header().Get("ETag") {
		t.Fatalf("ETags %q %q", a.Header().Get("ETag"), b.Header().Get("ETag"))
	}
	stale := call(t, h, "GET", "/v1/fonts/Vazirmatn%20Regular/65024-65279.pbf", nil, map[string]string{"If-None-Match": a.Header().Get("ETag")})
	if stale.Code != 200 {
		t.Fatalf("stale validator answered %d", stale.Code)
	}
}

func TestSpecServed(t *testing.T) {
	rec := call(t, newServer(t), "GET", "/v1/openapi.yaml", nil, nil)
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "openapi: 3.0.3") {
		t.Fatalf("spec %d", rec.Code)
	}
}
