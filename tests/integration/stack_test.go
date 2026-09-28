//go:build integration

// Full-stack integration test. `make test-integration` starts PostgreSQL and
// the API in an isolated compose project (compose.test.yaml) with an empty
// database; this test then drives the importer and checks the API end to end.
// Subtests run in order and depend on each other.
//
// Environment:
//
//	KARTA_TEST_COMPOSE    compose command for the test project (required)
//	KARTA_TEST_BASE_URL   API base URL (default http://127.0.0.1:18080)
//	KARTA_TEST_DB_PORT    published PostgreSQL port (default 55433)
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/jackc/pgx/v5"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/mvt"
	"github.com/paulmach/orb/maptile"

	"github.com/pouriya-sedaghat/karta/internal/schema"
	"github.com/pouriya-sedaghat/karta/openapi"
)

var (
	base       = env("KARTA_TEST_BASE_URL", "http://127.0.0.1:18080")
	composeCmd = os.Getenv("KARTA_TEST_COMPOSE")
	dbPort     = env("KARTA_TEST_DB_PORT", "55433")
	repoRoot   = "../.."
	router     routers.Router
	client     = &http.Client{Timeout: 30 * time.Second}
	fixtureBox = [4]float64{0, 0, 0.02, 0.015}
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestMain(m *testing.M) {
	if composeCmd == "" {
		fmt.Fprintln(os.Stderr, "KARTA_TEST_COMPOSE is not set; run `make test-integration`")
		os.Exit(2)
	}
	doc, err := openapi3.NewLoader().LoadFromData(openapi.Spec)
	if err == nil {
		err = doc.Validate(context.Background())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapi:", err)
		os.Exit(2)
	}
	doc.Servers = openapi3.Servers{{URL: base}}
	router, err = legacy.NewRouter(doc)
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapi router:", err)
		os.Exit(2)
	}
	for _, ct := range []string{"application/vnd.mapbox-vector-tile", "application/x-protobuf", "application/yaml"} {
		openapi3filter.RegisterBodyDecoder(ct, openapi3filter.FileBodyDecoder)
	}
	os.Exit(m.Run())
}

// --- helpers ------------------------------------------------------------------

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("invalid JSON (%d): %v: %s", r.status, err, r.body)
	}
}

func (r response) errorCode(t *testing.T) (string, string) {
	t.Helper()
	var e struct {
		Error struct{ Code, Parameter string } `json:"error"`
	}
	r.json(t, &e)
	return e.Error.Code, e.Error.Parameter
}

// do sends a request and validates the response against the OpenAPI spec.
func do(t *testing.T, method, path string, header map[string]string, body io.Reader) response {
	t.Helper()
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Errorf("%s %s: no X-Request-ID", method, path)
	}
	if route, params, err := router.FindRoute(req); err == nil && method == http.MethodGet && resp.StatusCode != http.StatusNotModified {
		in := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
			Status:                 resp.StatusCode,
			Header:                 resp.Header,
			Body:                   io.NopCloser(bytes.NewReader(b)),
			Options:                &openapi3filter.Options{IncludeResponseStatus: true},
		}
		if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
			t.Errorf("%s %s: response does not match openapi.yaml: %v", method, path, err)
		}
	}
	return response{status: resp.StatusCode, header: resp.Header, body: b}
}

func get(t *testing.T, path string) response {
	t.Helper()
	return do(t, http.MethodGet, path, nil, nil)
}

func expectStatus(t *testing.T, r response, want int) {
	t.Helper()
	if r.status != want {
		t.Fatalf("status %d, want %d: %s", r.status, want, r.body)
	}
}

func expectError(t *testing.T, r response, status int, code, param string) {
	t.Helper()
	if r.status != status {
		t.Errorf("status %d, want %d: %s", r.status, status, r.body)
		return
	}
	gotCode, gotParam := r.errorCode(t)
	if gotCode != code || (param != "" && gotParam != param) {
		t.Errorf("error %s/%s, want %s/%s", gotCode, gotParam, code, param)
	}
	if cc := r.header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("error Cache-Control %q, want no-store", cc)
	}
}

func compose(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("sh", "-c", composeCmd+" "+strings.Join(args, " "))
	cmd.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("compose %v: %v", args, err)
	}
	return stdout.String(), stderr.String(), code
}

type importResult struct {
	ReleaseID     string `json:"release_id"`
	AlreadyActive bool   `json:"already_active"`
	Report        *struct {
		Checks []struct {
			Name   string `json:"name"`
			Passed bool   `json:"passed"`
		} `json:"checks"`
		Counts map[string]int64 `json:"counts"`
	} `json:"report"`
}

func runImport(t *testing.T, snapshot, region string) (importResult, int, string) {
	t.Helper()
	out, errOut, code := compose(t, "run", "--rm", "-T", "importer", "--snapshot", snapshot, "--region", region)
	var res importResult
	if strings.TrimSpace(out) != "" {
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("import output is not JSON: %v\n%s\n%s", err, out, errOut)
		}
	}
	return res, code, errOut
}

func waitReady(t *testing.T, want bool, reason string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/health/ready", nil)
		resp, err := client.Do(req)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			_ = json.Unmarshal(b, &last)
			if (resp.StatusCode == http.StatusOK) == want && (reason == "" || last["reason"] == reason) {
				return last
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("readiness did not become ready=%v reason=%q; last %v", want, reason, last)
	return nil
}

func superuser(t *testing.T, database string) *pgx.Conn {
	t.Helper()
	pw, err := os.ReadFile(filepath.Join(repoRoot, "secrets", "db_superuser_password"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=127.0.0.1 port=%s user=postgres dbname=%s sslmode=disable", dbPort, database))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Password = strings.TrimSpace(string(pw))
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

type searchResp struct {
	ReleaseID       string `json:"release_id"`
	NormalizedQuery string `json:"normalized_query"`
	Results         []struct {
		ID          string            `json:"id"`
		DisplayName string            `json:"display_name"`
		Names       map[string]string `json:"names"`
		Lon         float64           `json:"lon"`
		Lat         float64           `json:"lat"`
		Match       struct {
			Key      string  `json:"key"`
			Language *string `json:"language"`
			Type     string  `json:"type"`
		} `json:"match"`
	} `json:"results"`
}

func search(t *testing.T, params url.Values) (searchResp, response) {
	t.Helper()
	r := get(t, "/v1/search?"+params.Encode())
	var s searchResp
	if r.status == http.StatusOK {
		r.json(t, &s)
	}
	return s, r
}

func ids(s searchResp) []string {
	out := make([]string, len(s.Results))
	for i, r := range s.Results {
		out[i] = r.ID
	}
	return out
}

// --- the test ---------------------------------------------------------------------

// dropRelease is `make reset` for one release, done as the superuser: the
// registry forgets every release and the release database is dropped.
func dropRelease(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	reg := superuser(t, "karta_registry")
	for _, sql := range []string{`DELETE FROM registry.active_release`, `DELETE FROM registry.releases`} {
		if _, err := reg.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	reg.Close(ctx)
	pg := superuser(t, "postgres")
	if _, err := pg.Exec(ctx, "DROP DATABASE karta_"+id+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	pg.Close(ctx)
	waitReady(t, false, "no_active_release")
}

// releaseIdentity reads the canonical identity a release was derived from.
func releaseIdentity(t *testing.T, id string) string {
	t.Helper()
	ctx := context.Background()
	conn := superuser(t, "karta_"+id)
	defer conn.Close(ctx)
	var identity string
	if err := conn.QueryRow(ctx, `SELECT identity FROM karta.release_info`).Scan(&identity); err != nil {
		t.Fatal(err)
	}
	return identity
}

func noCandidatesLeft(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var leftovers int
	pg := superuser(t, "postgres")
	defer pg.Close(ctx)
	if err := pg.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE datname LIKE 'karta\_c%'`).Scan(&leftovers); err != nil || leftovers != 0 {
		t.Errorf("candidate databases left behind: %d %v", leftovers, err)
	}
}

func TestStack(t *testing.T) {
	var releaseID, otherViewID string
	// fixtureStyle is the style served at the fixture release's pinned URL.
	var fixtureStyle []byte

	t.Run("readiness without data", func(t *testing.T) {
		expectStatus(t, get(t, "/health/live"), http.StatusOK)
		st := waitReady(t, false, "no_active_release")
		if st["release_id"] != nil {
			t.Errorf("release_id %v, want null", st["release_id"])
		}
		expectError(t, get(t, "/v1/manifest"), http.StatusServiceUnavailable, "no_active_release", "")
		expectError(t, get(t, "/v1/search?q=lake"), http.StatusServiceUnavailable, "no_active_release", "")
		expectError(t, get(t, "/v1/releases/r000000000000000000000000/style.json"), http.StatusNotFound, "unknown_release", "")
	})

	t.Run("import rejects a snapshot that does not match the pinned digest", func(t *testing.T) {
		_, code, stderr := runImport(t, "/data/testdata/fixture/karta-fixture.osm", "/data/testdata/regions/fixture-wrong-digest.json")
		if code != 3 || !strings.Contains(stderr, "refusing to import a different snapshot") {
			t.Fatalf("exit %d, want 3 (input verification); stderr:\n%s", code, stderr)
		}
	})

	t.Run("import rejects a snapshot from another region", func(t *testing.T) {
		_, code, stderr := runImport(t, "/data/testdata/fixture/karta-fixture.osm", "/data/testdata/regions/fixture-other-bbox.json")
		if code != 3 || !strings.Contains(stderr, "belongs to another extract") {
			t.Fatalf("exit %d, want 3; stderr:\n%s", code, stderr)
		}
		waitReady(t, false, "no_active_release")
	})

	t.Run("import fixture", func(t *testing.T) {
		res, code, stderr := runImport(t, "/data/testdata/fixture/karta-fixture.osm", "/config/regions/fixture.json")
		if code != 0 || res.Report == nil {
			t.Fatalf("exit %d; stderr:\n%s", code, stderr)
		}
		if !regexp.MustCompile(`^r[0-9a-f]{24}$`).MatchString(res.ReleaseID) {
			t.Fatalf("release id %q", res.ReleaseID)
		}
		for _, c := range res.Report.Checks {
			if !c.Passed {
				t.Errorf("check %s failed", c.Name)
			}
		}
		releaseID = res.ReleaseID
		t.Logf("release %s counts %v", releaseID, res.Report.Counts)
	})
	if releaseID == "" {
		t.Fatal("no release imported; stopping")
	}

	t.Run("ready after import", func(t *testing.T) {
		st := waitReady(t, true, "ready")
		if st["release_id"] != releaseID {
			t.Fatalf("ready release %v, want %s", st["release_id"], releaseID)
		}
	})

	t.Run("re-importing the same snapshot is a no-op", func(t *testing.T) {
		res, code, stderr := runImport(t, "/data/testdata/fixture/karta-fixture.osm", "/config/regions/fixture.json")
		if code != 0 || !res.AlreadyActive || res.ReleaseID != releaseID {
			t.Fatalf("exit %d, result %+v; stderr:\n%s", code, res, stderr)
		}
	})

	t.Run("manifest", func(t *testing.T) {
		r := get(t, "/v1/manifest")
		expectStatus(t, r, http.StatusOK)
		var m struct {
			Release struct {
				ReleaseID     string `json:"release_id"`
				DataTimestamp string `json:"osm_data_timestamp"`
				SourceSHA256  string `json:"source_sha256"`
			} `json:"release"`
			StyleURL string `json:"style_url"`
			Tiles    struct {
				URLTemplate string   `json:"url_template"`
				Layers      []string `json:"layers"`
				MaxZoom     int      `json:"maxzoom"`
			} `json:"tiles"`
			Attribution struct {
				Text, LicenseURL string
			} `json:"attribution"`
			Capabilities map[string]bool `json:"capabilities"`
		}
		r.json(t, &m)
		if m.Release.ReleaseID != releaseID || m.Release.DataTimestamp != "2026-01-01T00:00:00Z" || len(m.Release.SourceSHA256) != 64 {
			t.Errorf("release %+v", m.Release)
		}
		if m.StyleURL != base+"/v1/releases/"+releaseID+"/style.json" ||
			m.Tiles.URLTemplate != base+"/v1/releases/"+releaseID+"/tiles/{z}/{x}/{y}.pbf" || m.Tiles.MaxZoom != 16 {
			t.Errorf("urls %q %q", m.StyleURL, m.Tiles.URLTemplate)
		}
		if m.Attribution.Text != "© OpenStreetMap contributors" {
			t.Errorf("attribution %q", m.Attribution.Text)
		}
		if m.Capabilities["address_geocoding"] || m.Capabilities["online_updates"] || m.Capabilities["manual_updates"] || !m.Capabilities["place_search"] {
			t.Errorf("capabilities %v", m.Capabilities)
		}
		if r.header.Get("Cache-Control") != "no-cache" || r.header.Get("ETag") == "" {
			t.Errorf("cache headers %q %q", r.header.Get("Cache-Control"), r.header.Get("ETag"))
		}
		nm := do(t, http.MethodGet, "/v1/manifest", map[string]string{"If-None-Match": r.header.Get("ETag")}, nil)
		expectStatus(t, nm, http.StatusNotModified)
		expectError(t, get(t, "/v1/manifest?x=1"), http.StatusBadRequest, "unknown_parameter", "")
	})

	t.Run("search", func(t *testing.T) {
		cases := []struct {
			name   string
			params url.Values
			want   []string // leading ids, in order
			exact  bool     // want is the complete result list
			first  string   // only the first id
			none   bool
		}{
			{name: "Persian exact, equal rank broken by area", params: url.Values{"q": {"دریاچه آزمون"}}, want: []string{"way/302", "way/100"}},
			{name: "English exact before prefix", params: url.Values{"q": {"Test Lake"}}, want: []string{"way/100", "way/302"}, exact: true},
			{name: "Persian kaf matches Arabic kaf", params: url.Values{"q": {"کافه"}}, first: "node/500"},
			{name: "Arabic kaf query", params: url.Values{"q": {"كافه آزمون"}}, first: "node/500"},
			{name: "Latin diacritics and case folded", params: url.Values{"q": {"CAFE delice"}}, first: "node/501"},
			{name: "identical names ordered by OSM id", params: url.Values{"q": {"twin kiosk"}}, want: []string{"node/502", "node/503"}, exact: true},
			{name: "space matches ZWNJ", params: url.Values{"q": {"کتاب فروشی"}}, first: "node/504"},
			{name: "ASCII digit matches Persian digit", params: url.Values{"q": {"ایستگاه 5"}}, first: "node/505"},
			{name: "Persian digit query", params: url.Values{"q": {"برکه ۷"}}, first: "relation/900"},
			{name: "English name of station", params: url.Values{"q": {"station 5"}}, first: "node/505"},
			{name: "harakat ignored", params: url.Values{"q": {"مسجد"}}, first: "node/509"},
			{name: "tatweel ignored", params: url.Values{"q": {"نانوایی"}}, first: "node/512"},
			{name: "Persian yeh matches Arabic yeh", params: url.Values{"q": {"داروخانه علی"}}, first: "node/513"},
			{name: "feature with only name:en", params: url.Values{"q": {"english only"}}, first: "node/506"},
			{name: "outside the region is not searchable", params: url.Values{"q": {"Outside Cafe"}}, none: true},
			{name: "unnamed features are not searchable", params: url.Values{"q": {"bench"}}, none: true},
			{name: "streets are not searchable", params: url.Values{"q": {"Test Expressway"}}, none: true},
			{name: "lang filter uses name:fa only", params: url.Values{"q": {"دریاچه"}, "lang": {"fa"}}, want: []string{"way/100"}, exact: true},
			{name: "bbox filter", params: url.Values{"q": {"twin kiosk"}, "bbox": {"0,0,0.0042,0.0092"}}, want: []string{"node/502"}, exact: true},
			{name: "limit", params: url.Values{"q": {"twin kiosk"}, "limit": {"1"}}, want: []string{"node/502"}, exact: true},
			{name: "one-character prefix", params: url.Values{"q": {"ک"}}, first: "node/500"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				s, r := search(t, c.params)
				expectStatus(t, r, http.StatusOK)
				got := ids(s)
				switch {
				case c.none && len(got) != 0:
					t.Errorf("got %v, want no results", got)
				case c.exact && strings.Join(got, ",") != strings.Join(c.want, ","):
					t.Errorf("got %v, want exactly %v", got, c.want)
				case c.want != nil && (len(got) < len(c.want) || strings.Join(got[:len(c.want)], ",") != strings.Join(c.want, ",")):
					t.Errorf("got %v, want leading %v", got, c.want)
				case c.first != "" && (len(got) == 0 || got[0] != c.first):
					t.Errorf("got %v, want first %s", got, c.first)
				}
				if s.ReleaseID != releaseID {
					t.Errorf("release_id %q", s.ReleaseID)
				}
				for _, res := range s.Results {
					if res.Lon < fixtureBox[0] || res.Lon > fixtureBox[2] || res.Lat < fixtureBox[1] || res.Lat > fixtureBox[3] {
						t.Errorf("%s at %v,%v outside the region", res.ID, res.Lon, res.Lat)
					}
				}
			})
		}

		s, r := search(t, url.Values{"q": {"Test Lake"}})
		if s.Results[0].Match.Key != "name:en" || s.Results[0].Match.Type != "exact" || s.Results[0].Match.Language == nil || *s.Results[0].Match.Language != "en" {
			t.Errorf("match %+v", s.Results[0].Match)
		}
		if s.Results[0].DisplayName != "دریاچه آزمون" || s.Results[0].Names["name:en"] != "Test Lake" {
			t.Errorf("display %q names %v", s.Results[0].DisplayName, s.Results[0].Names)
		}
		if r.header.Get("Cache-Control") != "no-cache" {
			t.Errorf("unpinned search Cache-Control %q", r.header.Get("Cache-Control"))
		}
		again := get(t, "/v1/search?"+url.Values{"q": {"Test Lake"}}.Encode())
		if !bytes.Equal(again.body, r.body) {
			t.Error("the same search returned different bytes")
		}
		_, pinned := search(t, url.Values{"q": {"Test Lake"}, "release_id": {releaseID}})
		expectStatus(t, pinned, http.StatusOK)
		if pinned.header.Get("Cache-Control") != "public, max-age=300" {
			t.Errorf("pinned search Cache-Control %q", pinned.header.Get("Cache-Control"))
		}
	})

	t.Run("search input validation", func(t *testing.T) {
		long := strings.Repeat("ا", 101)
		cases := []struct {
			query, code, param string
			status             int
		}{
			{"", "invalid_parameter", "q", 400},
			{"q=", "invalid_parameter", "q", 400},
			{"q=%20%20", "invalid_parameter", "q", 400},
			{"q=" + url.QueryEscape(long), "invalid_parameter", "q", 400},
			{"q=a%01b", "invalid_parameter", "q", 400},
			{"q=%25%25_", "invalid_query", "q", 400},
			{"q=%zz", "invalid_parameter", "", 400},
			{"q=lake&limit=0", "invalid_parameter", "limit", 400},
			{"q=lake&limit=51", "invalid_parameter", "limit", 400},
			{"q=lake&limit=ten", "invalid_parameter", "limit", 400},
			{"q=lake&lang=FA", "invalid_parameter", "lang", 400},
			{"q=lake&lang=english", "invalid_parameter", "lang", 400},
			{"q=lake&bbox=1,2,3", "invalid_parameter", "bbox", 400},
			{"q=lake&bbox=3,0,1,1", "invalid_parameter", "bbox", 400},
			{"q=lake&bbox=0,0,1,NaN", "invalid_parameter", "bbox", 400},
			{"q=lake&q=pond", "invalid_parameter", "q", 400},
			{"q=lake&foo=1", "unknown_parameter", "foo", 400},
			{"q=lake&release_id=latest", "invalid_parameter", "release_id", 400},
			{"q=lake&release_id=r000000000000000000000000", "unknown_release", "release_id", 404},
		}
		for _, c := range cases {
			expectError(t, get(t, "/v1/search?"+c.query), c.status, c.code, c.param)
		}
		post := do(t, http.MethodPost, "/v1/search?q=lake", nil, strings.NewReader("{}"))
		expectError(t, post, http.StatusMethodNotAllowed, "method_not_allowed", "")
		withBody := do(t, http.MethodGet, "/v1/search?q=lake", map[string]string{"Content-Type": "text/plain"}, strings.NewReader("x"))
		expectError(t, withBody, http.StatusRequestEntityTooLarge, "request_body_not_allowed", "")
		expectError(t, get(t, "/v2/search?q=lake"), http.StatusNotFound, "not_found", "")
	})

	var style map[string]any
	t.Run("style", func(t *testing.T) {
		r := get(t, "/v1/releases/"+releaseID+"/style.json")
		expectStatus(t, r, http.StatusOK)
		r.json(t, &style)
		src := style["sources"].(map[string]any)["karta"].(map[string]any)
		tiles := src["tiles"].([]any)
		if len(tiles) != 1 || tiles[0] != base+"/v1/releases/"+releaseID+"/tiles/{z}/{x}/{y}.pbf" {
			t.Errorf("tiles %v", tiles)
		}
		if style["glyphs"] != base+"/v1/fonts/{fontstack}/{range}.pbf" {
			t.Errorf("glyphs %v", style["glyphs"])
		}
		if _, ok := style["sprite"]; ok {
			t.Error("style references a sprite, which is not served")
		}
		// Every URL the style makes the client fetch is on this server.
		var walk func(v any)
		walk = func(v any) {
			switch x := v.(type) {
			case string:
				if (strings.HasPrefix(x, "http://") || strings.HasPrefix(x, "https://")) && !strings.HasPrefix(x, base+"/") {
					t.Errorf("style fetches external URL %q", x)
				}
			case []any:
				for _, e := range x {
					walk(e)
				}
			case map[string]any:
				for k, e := range x {
					if k != "attribution" {
						walk(e)
					}
				}
			}
		}
		walk(style)
		catalog := schema.Layers()
		for _, l := range style["layers"].([]any) {
			layer := l.(map[string]any)
			if sl, ok := layer["source-layer"].(string); ok {
				if _, ok := catalog.Layer(sl); !ok {
					t.Errorf("style layer %v uses missing tile layer %q", layer["id"], sl)
				}
			}
		}
		if !strings.Contains(src["attribution"].(string), "https://www.openstreetmap.org/copyright") {
			t.Errorf("attribution %v", src["attribution"])
		}
		if r.header.Get("ETag") == "" || r.header.Get("Cache-Control") != "public, max-age=86400" {
			t.Errorf("cache headers %v", r.header)
		}
		expectStatus(t, do(t, http.MethodGet, "/v1/releases/"+releaseID+"/style.json", map[string]string{"If-None-Match": r.header.Get("ETag")}, nil), http.StatusNotModified)
		expectError(t, get(t, "/v1/releases/r000000000000000000000000/style.json"), http.StatusNotFound, "unknown_release", "release_id")
		expectError(t, get(t, "/v1/releases/latest/style.json"), http.StatusBadRequest, "invalid_parameter", "release_id")
	})

	t.Run("glyphs", func(t *testing.T) {
		for _, stack := range []string{"Vazirmatn Regular", "Vazirmatn Bold"} {
			p := "/v1/fonts/" + url.PathEscape(stack)
			for _, rng := range []string{"0-255", "1536-1791", "64256-64511", "65024-65279", "19968-20223"} {
				r := get(t, p+"/"+rng+".pbf")
				expectStatus(t, r, http.StatusOK)
				if r.header.Get("Content-Type") != "application/x-protobuf" || len(r.body) < 10 {
					t.Errorf("%s %s: %q, %d bytes", stack, rng, r.header.Get("Content-Type"), len(r.body))
				}
			}
			expectError(t, get(t, p+"/1-256.pbf"), http.StatusBadRequest, "invalid_parameter", "range")
			expectError(t, get(t, p+"/0-255.json"), http.StatusBadRequest, "invalid_parameter", "range")
		}
		expectError(t, get(t, "/v1/fonts/Arial%20Unicode%20MS%20Regular/0-255.pbf"), http.StatusNotFound, "unknown_fontstack", "fontstack")
	})

	t.Run("tiles", func(t *testing.T) {
		path := func(z, x, y int) string { return fmt.Sprintf("/v1/releases/%s/tiles/%d/%d/%d.pbf", releaseID, z, x, y) }
		lake := maptile.At(orb.Point{0.0035, 0.0035}, 14)
		r := get(t, path(14, int(lake.X), int(lake.Y)))
		expectStatus(t, r, http.StatusOK)
		if r.header.Get("Content-Type") != "application/vnd.mapbox-vector-tile" || r.header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Errorf("headers %v", r.header)
		}
		layers, err := mvt.Unmarshal(r.body)
		if err != nil {
			t.Fatalf("tile does not decode: %v", err)
		}
		layers.ProjectToWGS84(lake)
		catalog := schema.Layers()
		byName := map[string]*mvt.Layer{}
		for _, l := range layers {
			byName[l.Name] = l
			cl, ok := catalog.Layer(l.Name)
			if !ok {
				t.Errorf("unexpected layer %s", l.Name)
				continue
			}
			if l.Extent != 4096 || l.Version != 2 {
				t.Errorf("layer %s extent %d version %d", l.Name, l.Extent, l.Version)
			}
			for _, f := range l.Features {
				for k := range f.Properties {
					if _, ok := cl.Fields[k]; !ok {
						t.Errorf("layer %s field %s is not in the catalog", l.Name, k)
					}
				}
				wantType := map[string]string{"point": "Point", "line": "LineString", "polygon": "Polygon"}[cl.Geometry]
				if gt := f.Geometry.GeoJSONType(); gt != wantType && gt != "Multi"+wantType {
					t.Errorf("layer %s has %s geometry, catalog says %s", l.Name, gt, cl.Geometry)
				}
			}
		}
		for _, want := range []string{"water", "landcover", "roads", "pois", "buildings"} {
			if l := byName[want]; l == nil || len(l.Features) == 0 {
				t.Errorf("tile has no %s features", want)
			}
		}
		var lakeLabel bool
		for _, f := range byName["pois"].Features {
			if f.Properties["name"] == "دریاچه آزمون" && f.Properties["osm_type"] == "way" {
				p := f.Geometry.(orb.Point)
				lakeLabel = p[0] > 0.0018 && p[0] < 0.0062 && p[1] > 0.0018 && p[1] < 0.0052
			}
		}
		if !lakeLabel {
			t.Error("lake label point missing or outside the lake")
		}
		// The primary road runs to lon 0.025 in the source and is clipped at 0.02.
		maxLon := 0.0
		for _, f := range byName["roads"].Features {
			for _, p := range pointsOf(f.Geometry) {
				maxLon = max(maxLon, p[0])
			}
		}
		// Tile coordinates are quantised to a 4096 grid: allow one grid step.
		if step := 360 / float64(uint(1)<<14) / 4096; maxLon > 0.02+step {
			t.Errorf("road extends to lon %v, beyond the region edge 0.02 (+%g)", maxLon, step)
		}
		again := get(t, path(14, int(lake.X), int(lake.Y)))
		if !bytes.Equal(again.body, r.body) {
			t.Error("tile bytes differ between requests")
		}
		expectStatus(t, do(t, http.MethodGet, path(14, int(lake.X), int(lake.Y)), map[string]string{"If-None-Match": r.header.Get("ETag")}, nil), http.StatusNotModified)

		empty := get(t, path(14, 0, 0))
		expectStatus(t, empty, http.StatusNoContent)
		if len(empty.body) != 0 || empty.header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Errorf("empty tile: %d bytes, %v", len(empty.body), empty.header)
		}
		expectError(t, get(t, path(17, 0, 0)), http.StatusNotFound, "tile_zoom_out_of_range", "")
		expectError(t, get(t, path(3, 8, 0)), http.StatusBadRequest, "invalid_tile_coordinates", "")
		expectError(t, get(t, "/v1/releases/"+releaseID+"/tiles/a/0/0.pbf"), http.StatusBadRequest, "invalid_tile_coordinates", "")
		expectError(t, get(t, "/v1/releases/"+releaseID+"/tiles/-1/0/0.pbf"), http.StatusBadRequest, "invalid_tile_coordinates", "")
		expectError(t, get(t, "/v1/releases/"+releaseID+"/tiles/1/0/0.mvt"), http.StatusNotFound, "not_found", "")
		expectError(t, get(t, "/v1/releases/r000000000000000000000000/tiles/0/0/0.pbf"), http.StatusNotFound, "unknown_release", "")
	})

	t.Run("cors", func(t *testing.T) {
		r := do(t, http.MethodGet, "/v1/manifest", map[string]string{"Origin": "https://app.example"}, nil)
		if r.header.Get("Access-Control-Allow-Origin") != "https://app.example" || !strings.Contains(r.header.Get("Access-Control-Expose-Headers"), "ETag") {
			t.Errorf("allowed origin headers %v", r.header)
		}
		r = do(t, http.MethodGet, "/v1/manifest", map[string]string{"Origin": "https://evil.example"}, nil)
		if r.header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("other origin was allowed: %v", r.header)
		}
		pre := do(t, http.MethodOptions, "/v1/search", map[string]string{"Origin": "https://app.example", "Access-Control-Request-Method": "GET"}, nil)
		if pre.status != http.StatusNoContent || pre.header.Get("Access-Control-Allow-Methods") != "GET, HEAD" || pre.header.Get("Access-Control-Allow-Credentials") != "" {
			t.Errorf("preflight %d %v", pre.status, pre.header)
		}
	})

	t.Run("demo and assets are local", func(t *testing.T) {
		r := get(t, "/demo/")
		expectStatus(t, r, http.StatusOK)
		csp := r.header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "connect-src 'self'") || !strings.Contains(csp, "script-src 'self'") {
			t.Errorf("CSP %q", csp)
		}
		// Only anchors (never fetched) may point elsewhere.
		for _, m := range regexp.MustCompile(`(src|href)="([^"]+)"`).FindAllStringSubmatch(string(r.body), -1) {
			if strings.HasPrefix(m[2], "http") && !regexp.MustCompile(`<a [^>]*href="`+regexp.QuoteMeta(m[2])).Match(r.body) {
				t.Errorf("demo loads external resource %s", m[2])
			}
		}
		for _, p := range []string{"/demo/app.js", "/demo/app.css", "/demo/vendor/maplibre-gl/maplibre-gl.mjs",
			"/demo/vendor/maplibre-gl/maplibre-gl-shared.mjs", "/demo/vendor/maplibre-gl/maplibre-gl-worker.mjs",
			"/demo/vendor/maplibre-gl/maplibre-gl.css", "/demo/vendor/maplibre-gl/LICENSE.txt"} {
			expectStatus(t, get(t, p), http.StatusOK)
		}
		expectError(t, get(t, "/demo/vendor/"), http.StatusNotFound, "not_found", "")
		expectError(t, get(t, "/demo/../go.mod"), http.StatusNotFound, "not_found", "")
		spec := get(t, "/v1/openapi.yaml")
		expectStatus(t, spec, http.StatusOK)
		if !bytes.Equal(spec.body, openapi.Spec) {
			t.Error("served OpenAPI document differs from openapi/openapi.yaml")
		}
	})

	t.Run("database roles are least-privilege", func(t *testing.T) {
		pw, err := os.ReadFile(filepath.Join(repoRoot, "secrets", "db_api_password"))
		if err != nil {
			t.Fatal(err)
		}
		connect := func(db string) (*pgx.Conn, error) {
			cfg, err := pgx.ParseConfig(fmt.Sprintf("host=127.0.0.1 port=%s user=karta_api dbname=%s sslmode=disable", dbPort, db))
			if err != nil {
				return nil, err
			}
			cfg.Password = strings.TrimSpace(string(pw))
			return pgx.ConnectConfig(context.Background(), cfg)
		}
		if c, err := connect("postgres"); err == nil {
			c.Close(context.Background())
			t.Error("karta_api can connect to the postgres database")
		}
		c, err := connect("karta_" + releaseID)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "DELETE FROM karta.features"); err == nil {
			t.Error("karta_api could delete in a default (read-only) session")
		}
		// Even with read-only switched off, the role has no write privileges.
		if _, err := c.Exec(context.Background(), "SET default_transaction_read_only = off"); err != nil {
			t.Fatal(err)
		}
		for _, stmt := range []string{
			"DELETE FROM karta.features",
			"UPDATE karta.release_info SET region_name = 'x'",
			"CREATE TABLE karta.x (id int)",
			"CREATE TABLE public.x (id int)",
		} {
			_, err := c.Exec(context.Background(), stmt)
			if err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Errorf("karta_api %q: err = %v, want permission denied", stmt, err)
			}
		}
		var n int
		if err := c.QueryRow(context.Background(), "SELECT count(*) FROM karta.features").Scan(&n); err != nil || n == 0 {
			t.Errorf("karta_api cannot read features: %v", err)
		}
	})

	t.Run("restart after a clean import", func(t *testing.T) {
		if _, stderr, code := compose(t, "restart", "api"); code != 0 {
			t.Fatalf("restart: %s", stderr)
		}
		st := waitReady(t, true, "ready")
		if st["release_id"] != releaseID {
			t.Fatalf("after restart release %v", st["release_id"])
		}
		s, r := search(t, url.Values{"q": {"دریاچه آزمون"}})
		expectStatus(t, r, http.StatusOK)
		if len(s.Results) == 0 || s.Results[0].ID != "way/302" {
			t.Errorf("after restart %v", ids(s))
		}
	})

	t.Run("a style that references a missing layer is refused", func(t *testing.T) {
		conn := superuser(t, "karta_"+releaseID)
		defer conn.Close(context.Background())
		ctx := context.Background()
		exec := func(sql string) {
			t.Helper()
			if _, err := conn.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
		}
		// The release database defaults to read-only; the superuser overrides
		// it for this session only.
		exec(`SET default_transaction_read_only = off`)
		exec(`CREATE TEMP TABLE saved AS SELECT style FROM karta.release_info`)
		exec(`UPDATE karta.release_info SET style = jsonb_set(style, '{layers,1,source-layer}', '"no_such_layer"')`)
		compose(t, "restart", "api")
		st := waitReady(t, false, "release_incompatible")
		if !strings.Contains(fmt.Sprint(st["detail"]), "no_such_layer") {
			t.Errorf("detail %v", st["detail"])
		}
		expectError(t, get(t, "/v1/manifest"), http.StatusServiceUnavailable, "no_active_release", "")
		exec(`UPDATE karta.release_info SET style = (SELECT style FROM saved)`)
		compose(t, "restart", "api")
		waitReady(t, true, "ready")
	})

	t.Run("a second release is refused while one is active", func(t *testing.T) {
		_, code, stderr := runImport(t, "/data/testdata/fixture/karta-fixture.osm", "/data/testdata/regions/fixture-alt.json")
		if code != 4 || !strings.Contains(stderr, "make reset") {
			t.Fatalf("exit %d, want 4; stderr:\n%s", code, stderr)
		}
		st := waitReady(t, true, "ready")
		if st["release_id"] != releaseID {
			t.Errorf("active release changed to %v", st["release_id"])
		}
	})

	t.Run("a changed default view is a new release, never the same immutable URLs", func(t *testing.T) {
		// While a release is active the importer refuses, and names the
		// different id the changed region would get.
		_, code, stderr := runImport(t, "/data/testdata/fixture/karta-fixture.osm", "/data/testdata/regions/fixture-other-view.json")
		m := regexp.MustCompile(`(r[0-9a-f]{24}) is serving; .* before importing (r[0-9a-f]{24})`).FindStringSubmatch(stderr)
		if code != 4 || m == nil || m[1] != releaseID || m[2] == releaseID {
			t.Fatalf("exit %d, ids %v; stderr:\n%s", code, m, stderr)
		}
	})

	t.Run("a change below 1e-7 to the box, center or zoom is a new release too", func(t *testing.T) {
		// fixture-sub-1e-7.json moves the box's east edge, the center and the
		// zoom by 1e-8 each: the snapshot header still matches (1e-7
		// tolerance), and the manifest and style would publish the new
		// values. Rounded identities made this "already active" (exit 0).
		_, code, stderr := runImport(t, "/data/testdata/fixture/karta-fixture.osm", "/data/testdata/regions/fixture-sub-1e-7.json")
		m := regexp.MustCompile(`(r[0-9a-f]{24}) is serving; .* before importing (r[0-9a-f]{24})`).FindStringSubmatch(stderr)
		if code != 4 || m == nil || m[1] != releaseID || m[2] == releaseID {
			t.Fatalf("exit %d, ids %v; stderr:\n%s", code, m, stderr)
		}
	})

	t.Run("database outage and recovery", func(t *testing.T) {
		if _, stderr, code := compose(t, "stop", "db"); code != 0 {
			t.Fatalf("stop db: %s", stderr)
		}
		waitReady(t, false, "database_unavailable")
		r := get(t, "/v1/search?q=lake")
		if r.status != http.StatusServiceUnavailable {
			t.Errorf("search during outage: %d %s", r.status, r.body)
		}
		expectStatus(t, get(t, "/health/live"), http.StatusOK)
		if _, stderr, code := compose(t, "start", "db"); code != 0 {
			t.Fatalf("start db: %s", stderr)
		}
		waitReady(t, true, "ready")
		s, r := search(t, url.Values{"q": {"Test Lake"}})
		expectStatus(t, r, http.StatusOK)
		if len(s.Results) == 0 || s.Results[0].ID != "way/100" {
			t.Errorf("after recovery %v", ids(s))
		}
	})

	t.Run("reset and rebuild with a changed default view gets new release URLs", func(t *testing.T) {
		oldStyle := "/v1/releases/" + releaseID + "/style.json"
		oldTile := "/v1/releases/" + releaseID + "/tiles/14/8192/8191.pbf"
		r := get(t, oldStyle)
		expectStatus(t, r, http.StatusOK)
		fixtureStyle = r.body
		dropRelease(t, releaseID)

		res, code, stderr := runImport(t, "/data/testdata/fixture/karta-fixture.osm", "/data/testdata/regions/fixture-other-view.json")
		if code != 0 || res.ReleaseID == "" || res.ReleaseID == releaseID {
			t.Fatalf("exit %d, release %q (old %s); stderr:\n%s", code, res.ReleaseID, releaseID, stderr)
		}
		otherViewID = res.ReleaseID
		st := waitReady(t, true, "ready")
		if st["release_id"] != res.ReleaseID {
			t.Fatalf("serving %v, want %s", st["release_id"], res.ReleaseID)
		}
		var m struct {
			StyleURL    string `json:"style_url"`
			DefaultView struct {
				Center [2]float64 `json:"center"`
				Zoom   float64    `json:"zoom"`
			} `json:"default_view"`
		}
		get(t, "/v1/manifest").json(t, &m)
		if m.DefaultView.Zoom != 14 || m.DefaultView.Center != [2]float64{0.005, 0.005} || !strings.Contains(m.StyleURL, res.ReleaseID) {
			t.Errorf("manifest %+v", m)
		}
		// The old immutable URLs are gone rather than serving new content.
		expectError(t, get(t, oldStyle), http.StatusNotFound, "unknown_release", "release_id")
		expectError(t, get(t, oldTile), http.StatusNotFound, "unknown_release", "release_id")
		var style struct {
			Zoom float64 `json:"zoom"`
		}
		get(t, "/v1/releases/"+res.ReleaseID+"/style.json").json(t, &style)
		if style.Zoom != 14 {
			t.Errorf("new style zoom %v", style.Zoom)
		}
		// The release records the canonical identity it was derived from.
		identity := releaseIdentity(t, res.ReleaseID)
		for _, want := range []string{`region.view.zoom="14"`, `region.view.center="0.005,0.005"`, `toolchain.osm2pgsql=`, `toolchain.geos=`, `toolchain.icu=`} {
			if !strings.Contains(identity, want) {
				t.Errorf("identity lacks %s:\n%s", want, identity)
			}
		}
		noCandidatesLeft(t)
	})

	t.Run("reset and rebuild with a change below 1e-7 gets new release URLs", func(t *testing.T) {
		if otherViewID == "" || fixtureStyle == nil {
			t.Skip("previous rebuild did not run")
		}
		dropRelease(t, otherViewID)
		res, code, stderr := runImport(t, "/data/testdata/fixture/karta-fixture.osm", "/data/testdata/regions/fixture-sub-1e-7.json")
		if code != 0 || res.ReleaseID == "" || res.ReleaseID == releaseID || res.ReleaseID == otherViewID {
			t.Fatalf("exit %d, release %q (fixture %s, other view %s); stderr:\n%s", code, res.ReleaseID, releaseID, otherViewID, stderr)
		}
		waitReady(t, true, "ready")
		newStyle := get(t, "/v1/releases/"+res.ReleaseID+"/style.json")
		expectStatus(t, newStyle, http.StatusOK)
		// The rebuilt style, placed at the fixture's old URL, would differ
		// from what that URL served; so the old URL must not be reused.
		asOld := bytes.ReplaceAll(newStyle.body, []byte(res.ReleaseID), []byte(releaseID))
		if bytes.Equal(asOld, fixtureStyle) {
			t.Fatal("the sub-1e-7 change is not visible in the style; the test does not exercise the invariant")
		}
		expectError(t, get(t, "/v1/releases/"+releaseID+"/style.json"), http.StatusNotFound, "unknown_release", "release_id")
		// The exact stored values are what is published.
		var style struct {
			Center  [2]float64 `json:"center"`
			Zoom    float64    `json:"zoom"`
			Sources map[string]struct {
				Bounds [4]float64 `json:"bounds"`
			} `json:"sources"`
		}
		newStyle.json(t, &style)
		wantBox := [4]float64{0, 0, 0.02000001, 0.015}
		if style.Zoom != 15.00000001 || style.Center != [2]float64{0.01000001, 0.0075} || style.Sources["karta"].Bounds != wantBox {
			t.Errorf("style view %v %v bounds %v", style.Center, style.Zoom, style.Sources["karta"].Bounds)
		}
		var m struct {
			Release struct {
				Region struct {
					BBox [4]float64 `json:"bbox"`
				} `json:"region"`
			} `json:"release"`
			DefaultView struct {
				Center [2]float64 `json:"center"`
				Zoom   float64    `json:"zoom"`
			} `json:"default_view"`
		}
		get(t, "/v1/manifest").json(t, &m)
		if m.Release.Region.BBox != wantBox || m.DefaultView.Zoom != 15.00000001 || m.DefaultView.Center != [2]float64{0.01000001, 0.0075} {
			t.Errorf("manifest %+v", m)
		}
		identity := releaseIdentity(t, res.ReleaseID)
		for _, want := range []string{`region.bbox="0,0,0.02000001,0.015"`, `region.view.center="0.01000001,0.0075"`, `region.view.zoom="15.00000001"`} {
			if !strings.Contains(identity, want) {
				t.Errorf("identity lacks %s:\n%s", want, identity)
			}
		}
		noCandidatesLeft(t)
	})
}

func pointsOf(g orb.Geometry) []orb.Point {
	var out []orb.Point
	switch v := g.(type) {
	case orb.Point:
		out = append(out, v)
	case orb.LineString:
		out = append(out, v...)
	case orb.MultiLineString:
		for _, l := range v {
			out = append(out, l...)
		}
	case orb.Polygon:
		for _, r := range v {
			out = append(out, r...)
		}
	case orb.MultiPolygon:
		for _, p := range v {
			for _, r := range p {
				out = append(out, r...)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
