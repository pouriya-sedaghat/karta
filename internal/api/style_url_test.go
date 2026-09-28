package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/pouriya-sedaghat/karta/internal/region"
	"github.com/pouriya-sedaghat/karta/internal/style"
)

// upgradedRenderer stands in for a later API version whose renderer adds an
// observable field for the same stored template and release.
func upgradedRenderer(stored json.RawMessage, p style.Params) ([]byte, error) {
	b, err := style.Render(stored, p)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	doc["metadata"].(map[string]any)["karta:renderer"] = "2"
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err = enc.Encode(doc)
	return buf.Bytes(), err
}

var styleURLPattern = regexp.MustCompile(`^(.+)/v1/releases/(r[0-9a-f]{24})/styles/([0-9a-f]{32})\.json$`)

// fetchStyle checks an issued style URL on h: it serves bytes whose digest is
// the id in the URL, as immutable, revalidates to 304, and is where the
// release-pinned style path redirects. It returns the bytes.
func fetchStyle(t *testing.T, h http.Handler, styleURL string) []byte {
	t.Helper()
	m := styleURLPattern.FindStringSubmatch(styleURL)
	if m == nil {
		t.Fatalf("style URL %q is not content-addressed", styleURL)
	}
	rec := getURL(t, h, styleURL, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", styleURL, rec.Code, rec.Body)
	}
	etag := `"` + m[3] + `"`
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("style Cache-Control %q", cc)
	}
	if rec.Header().Get("ETag") != etag || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("style headers %v", rec.Header())
	}
	body := rec.Body.Bytes()
	if sum := sha256.Sum256(body); hex.EncodeToString(sum[:16]) != m[3] {
		t.Fatalf("URL %s does not name its bytes", styleURL)
	}
	nm := getURL(t, h, styleURL, map[string]string{"If-None-Match": etag})
	if nm.Code != http.StatusNotModified || nm.Header().Get("ETag") != etag || nm.Header().Get("Cache-Control") != rec.Header().Get("Cache-Control") || nm.Body.Len() != 0 {
		t.Errorf("revalidation: %d %v %q", nm.Code, nm.Header(), nm.Body)
	}
	red := call(t, h, http.MethodGet, "/v1/releases/"+m[2]+"/style.json", nil, nil)
	if red.Code != http.StatusTemporaryRedirect || red.Header().Get("Location") != styleURL ||
		red.Header().Get("Cache-Control") != "no-cache" || red.Body.Len() != 0 {
		t.Errorf("style.json: %d %v %q", red.Code, red.Header(), red.Body)
	}
	return body
}

// expectNotServed checks that h no longer serves a previously issued style URL.
func expectNotServed(t *testing.T, h http.Handler, styleURL string) {
	t.Helper()
	expectErr(t, getURL(t, h, styleURL, nil), http.StatusNotFound, CodeUnknownStyle, "style_id")
}

// An existing release database is served by successive API processes. A
// style URL, once issued, must never serve different bytes: after a renderer
// change or a new KARTA_PUBLIC_BASE_URL the manifest issues a new URL, and the
// old one is no longer served, while the release keeps its id and tile URLs.
func TestIssuedStyleURLNeverServesDifferentBytes(t *testing.T) {
	cfg := region.Config{
		ID: "tehran-chitgar", Name: "Chitgar Lake area, Tehran (development sample)",
		BBox: [4]float64{51.175, 35.705, 51.285, 35.785},
		View: region.View{Center: [2]float64{51.215, 35.745}, Zoom: 13},
	}
	id := releaseIDFor(t, cfg)
	rel := testRelease(cfg, id)

	// API as released.
	v1 := testHandler(t, rel, "http://localhost:8080", style.Render)
	m1, rec1 := manifestOf(t, v1)
	if rec1.Header().Get("Cache-Control") != "no-cache" || rec1.Header().Get("ETag") == "" {
		t.Errorf("manifest headers %v", rec1.Header())
	}
	if u := styleURLPattern.FindStringSubmatch(m1.StyleURL); u == nil || u[1] != "http://localhost:8080" || u[2] != id {
		t.Fatalf("style_url %q", m1.StyleURL)
	}
	s1 := fetchStyle(t, v1, m1.StyleURL)
	styleID := styleURLPattern.FindStringSubmatch(m1.StyleURL)[3]
	prefix := "/v1/releases/" + id + "/styles/"
	expectErr(t, call(t, v1, http.MethodGet, prefix+strings.Repeat("0", 32)+".json", nil, nil), http.StatusNotFound, CodeUnknownStyle, "style_id")
	expectErr(t, call(t, v1, http.MethodGet, prefix+"latest.json", nil, nil), http.StatusBadRequest, CodeInvalidParameter, "style_id")
	expectErr(t, call(t, v1, http.MethodGet, prefix+styleID, nil, nil), http.StatusNotFound, CodeNotFound, "")
	expectErr(t, call(t, v1, http.MethodGet, prefix+styleID+".json?v=1", nil, nil), http.StatusBadRequest, CodeUnknownParameter, "")
	expectErr(t, call(t, v1, http.MethodGet, "/v1/releases/r000000000000000000000000/styles/"+styleID+".json", nil, nil),
		http.StatusNotFound, CodeUnknownRelease, "release_id")

	// A restart with the same version and configuration issues the same URL
	// for the same bytes.
	restarted := testHandler(t, rel, "http://localhost:8080", style.Render)
	if m, _ := manifestOf(t, restarted); m.StyleURL != m1.StyleURL {
		t.Fatalf("restart changed the style URL: %s -> %s", m1.StyleURL, m.StyleURL)
	}
	if !bytes.Equal(fetchStyle(t, restarted, m1.StyleURL), s1) {
		t.Fatal("restart changed the style bytes")
	}

	// A later API version renders the same, unchanged release differently.
	v2 := testHandler(t, rel, "http://localhost:8080", upgradedRenderer)
	m2, rec2 := manifestOf(t, v2)
	if m2.Release.ReleaseID != id || m2.Tiles.URLTemplate != m1.Tiles.URLTemplate {
		t.Errorf("the existing release must keep its id and tile URLs: %+v", m2)
	}
	if m2.StyleURL == m1.StyleURL {
		t.Fatalf("the renderer change reuses %s for different bytes", m1.StyleURL)
	}
	if rec2.Header().Get("ETag") == rec1.Header().Get("ETag") {
		t.Error("manifest ETag did not change with the style URL; revalidating clients would keep the old URL")
	}
	expectNotServed(t, v2, m1.StyleURL)
	s2 := fetchStyle(t, v2, m2.StyleURL)
	if bytes.Equal(s2, s1) || !bytes.Contains(s2, []byte(`"karta:renderer":"2"`)) {
		t.Fatal("the upgraded renderer's output is not what its URL serves")
	}
	// Rolling back does not serve the newer URL with older bytes either.
	expectNotServed(t, v1, m2.StyleURL)

	// A new KARTA_PUBLIC_BASE_URL changes the URLs inside the style.
	moved := testHandler(t, rel, "https://maps.example.org", style.Render)
	m3, _ := manifestOf(t, moved)
	u3 := styleURLPattern.FindStringSubmatch(m3.StyleURL)
	if u3 == nil || u3[1] != "https://maps.example.org" || u3[3] == styleID {
		t.Fatalf("style_url after the base URL change: %q (was %q)", m3.StyleURL, m1.StyleURL)
	}
	if m3.Release.ReleaseID != id || m3.Tiles.URLTemplate != "https://maps.example.org/v1/releases/"+id+"/tiles/{z}/{x}/{y}.pbf" {
		t.Errorf("manifest after the base URL change: %+v", m3)
	}
	expectNotServed(t, moved, m1.StyleURL)
	s3 := fetchStyle(t, moved, m3.StyleURL)
	if !bytes.Contains(s3, []byte(`"https://maps.example.org/v1/releases/`+id+`/tiles/{z}/{x}/{y}.pbf"`)) ||
		!bytes.Contains(s3, []byte(`"https://maps.example.org/v1/fonts/{fontstack}/{range}.pbf"`)) || bytes.Contains(s3, []byte("localhost")) {
		t.Errorf("style under the new base URL: %s", s3)
	}
}
