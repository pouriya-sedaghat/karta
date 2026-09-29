package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/region"
	"github.com/pouriya-sedaghat/karta/internal/release"
	"github.com/pouriya-sedaghat/karta/internal/style"
)

// simCache is a minimal private HTTP cache (RFC 9111), enough to model what a
// browser or proxy does with Karta's responses over time: it stores 200 and
// 307 responses unless they are no-store, reuses a stored response while it is
// fresh (max-age against a fake clock; no-cache is never fresh), revalidates a
// stale one with If-None-Match, and follows redirects. The origin is the
// server behind the cache and can be swapped, as an upgrade does.
type simCache struct {
	origin  http.Handler
	now     time.Time
	entries map[string]*simEntry
	// sent records every request that reached the origin, with its
	// If-None-Match header and the status it got.
	sent []simRequest
}

type simEntry struct {
	status int
	header http.Header
	body   []byte
	stored time.Time
}

type simRequest struct {
	path, ifNoneMatch string
	status            int
}

func newSimCache(origin http.Handler, now time.Time) *simCache {
	return &simCache{origin: origin, now: now, entries: map[string]*simEntry{}}
}

// lifetime is the freshness lifetime a response allows; store reports
// whether it may be stored at all.
func lifetime(h http.Header) (fresh time.Duration, store bool) {
	store = true
	for _, d := range strings.Split(h.Get("Cache-Control"), ",") {
		d = strings.TrimSpace(d)
		switch {
		case d == "no-store":
			return 0, false
		case d == "no-cache":
			return 0, true
		case strings.HasPrefix(d, "max-age="):
			if n, err := strconv.Atoi(strings.TrimPrefix(d, "max-age=")); err == nil {
				fresh = time.Duration(n) * time.Second
			}
		}
	}
	return fresh, store
}

// remaining is how much longer the stored response for path stays fresh.
func (c *simCache) remaining(path string) time.Duration {
	e := c.entries[path]
	if e == nil {
		return 0
	}
	fresh, _ := lifetime(e.header)
	return fresh - c.now.Sub(e.stored)
}

// get fetches path through the cache, following up to 5 redirects. It
// returns the final status and body, and whether every hop was answered from
// the cache without contacting the origin.
func (c *simCache) get(t *testing.T, path string) (int, []byte, bool) {
	t.Helper()
	fromCache := true
	for hop := 0; hop < 5; hop++ {
		e := c.entries[path]
		if fresh, _ := lifetime(headerOf(e)); e == nil || c.now.Sub(e.stored) >= fresh {
			fromCache = false
			req := httptest.NewRequest(http.MethodGet, path, nil)
			inm := ""
			if e != nil {
				inm = e.header.Get("ETag")
			}
			if inm != "" {
				req.Header.Set("If-None-Match", inm)
			}
			rec := httptest.NewRecorder()
			c.origin.ServeHTTP(rec, req)
			c.sent = append(c.sent, simRequest{path: path, ifNoneMatch: inm, status: rec.Code})
			switch _, store := lifetime(rec.Header()); {
			case rec.Code == http.StatusNotModified && e != nil:
				e.stored = c.now
				for _, k := range []string{"Cache-Control", "ETag"} {
					if v := rec.Header().Get(k); v != "" {
						e.header.Set(k, v)
					}
				}
			case (rec.Code == http.StatusOK || rec.Code == http.StatusTemporaryRedirect) && store:
				e = &simEntry{status: rec.Code, header: rec.Header().Clone(), body: rec.Body.Bytes(), stored: c.now}
				c.entries[path] = e
			default:
				delete(c.entries, path)
				return rec.Code, rec.Body.Bytes(), false
			}
		}
		if e.status != http.StatusTemporaryRedirect {
			return e.status, e.body, fromCache
		}
		u, err := url.Parse(e.header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		path = u.RequestURI()
	}
	t.Fatalf("too many redirects at %s", path)
	return 0, nil, false
}

func headerOf(e *simEntry) http.Header {
	if e == nil {
		return http.Header{}
	}
	return e.header
}

// legacyStyleJSON is /v1/releases/{release_id}/style.json as builds before
// content-addressed style URLs (up to 1326cd3) served it: the rendered style
// itself, public, max-age=86400, with a content ETag.
func legacyStyleJSON(rel *release.Release, baseURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := rel.Info
		b, err := style.Render(rel.Style, style.Params{
			ReleaseID: i.ReleaseID, BaseURL: baseURL, Bounds: i.BBox, Center: i.Center,
			Zoom: i.Zoom, MinZoom: i.MinZoom, MaxZoom: i.MaxZoom, Attribution: i.Attribution,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeBytes(w, r, http.StatusOK, "application/json; charset=utf-8", "public, max-age=86400", b, true)
	})
}

// A style.json response cached from a build before content-addressed style
// URLs cannot be invalidated by the server. This follows one such cache
// entry through an upgrade: the old bytes are reused, without any request to
// the server, for the rest of their 24-hour lifetime; the first revalidation
// after that is answered with the 307 (never a 304 that would renew the old
// bytes), and the client ends up at the content-addressed URL.
func TestCachedStyleJSONFromBeforeContentAddressing(t *testing.T) {
	cfg := region.Config{
		ID: "tehran-chitgar", Name: "Chitgar Lake area, Tehran (development sample)",
		BBox: [4]float64{51.175, 35.705, 51.285, 35.785},
		View: region.View{Center: [2]float64{51.215, 35.745}, Zoom: 13},
	}
	id := releaseIDFor(t, cfg)
	rel := testRelease(cfg, id)
	const base = "http://localhost:8080"
	path := "/v1/releases/" + id + "/style.json"
	t0 := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)

	cache := newSimCache(legacyStyleJSON(rel, base), t0)
	status, oldBytes, _ := cache.get(t, path)
	if status != http.StatusOK || len(oldBytes) == 0 {
		t.Fatalf("legacy style.json: %d", status)
	}
	oldETag := cache.entries[path].header.Get("ETag")

	// One hour later the API is upgraded to a build that also renders
	// differently (a pure base URL change behaves the same way).
	upgraded := testHandler(t, rel, base, upgradedRenderer)
	cache.origin = upgraded
	cache.now = t0.Add(time.Hour)
	if got := cache.remaining(path); got != 23*time.Hour {
		t.Fatalf("remaining lifetime after the upgrade: %v, want 23h", got)
	}
	for _, at := range []time.Duration{time.Hour, 12 * time.Hour, 24*time.Hour - time.Second} {
		cache.now = t0.Add(at)
		status, b, fromCache := cache.get(t, path)
		if status != http.StatusOK || !fromCache || !bytes.Equal(b, oldBytes) {
			t.Fatalf("at +%v: status %d, from cache %v; the stored 200 must still be used", at, status, fromCache)
		}
	}
	if len(cache.sent) != 1 {
		t.Fatalf("the upgraded server was contacted during the old response's lifetime: %+v", cache.sent[1:])
	}

	// Revalidating the old 200 must not renew it: the new server answers the
	// old ETag with the redirect, not 304.
	if rec := call(t, upgraded, http.MethodGet, path, nil, map[string]string{"If-None-Match": oldETag}); rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("revalidation with the pre-upgrade ETag: %d, want 307", rec.Code)
	}

	// At expiry the cache revalidates and follows the redirect.
	cache.now = t0.Add(24 * time.Hour)
	m, _ := manifestOf(t, upgraded)
	current := fetchStyle(t, upgraded, m.StyleURL)
	status, b, fromCache := cache.get(t, path)
	if status != http.StatusOK || fromCache || !bytes.Equal(b, current) || bytes.Equal(b, oldBytes) {
		t.Fatalf("after expiry: status %d, from cache %v, current style %v", status, fromCache, bytes.Equal(b, current))
	}
	styleURL, _ := url.Parse(m.StyleURL)
	want := []simRequest{
		{path: path, status: http.StatusOK},
		{path: path, ifNoneMatch: oldETag, status: http.StatusTemporaryRedirect},
		{path: styleURL.RequestURI(), status: http.StatusOK},
	}
	if len(cache.sent) != len(want) {
		t.Fatalf("requests to the server: %+v, want %+v", cache.sent, want)
	}
	for i := range want {
		if cache.sent[i] != want[i] {
			t.Errorf("request %d: %+v, want %+v", i, cache.sent[i], want[i])
		}
	}

	// From then on the redirect is revalidated on every use and the style
	// itself comes from the cache, immutable, under its own URL.
	cache.now = cache.now.Add(time.Minute)
	sentBefore := len(cache.sent)
	if status, b, _ := cache.get(t, path); status != http.StatusOK || !bytes.Equal(b, current) {
		t.Fatalf("after the transition: %d", status)
	}
	if got := cache.sent[sentBefore:]; len(got) != 1 || got[0].path != path || got[0].status != http.StatusTemporaryRedirect {
		t.Errorf("after the transition only the redirect is revalidated: %+v", got)
	}
	if cache.remaining(styleURL.RequestURI()) < 364*24*time.Hour {
		t.Errorf("the content-addressed style is not cached as immutable: %v", cache.remaining(styleURL.RequestURI()))
	}
}
