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
	"github.com/pouriya-sedaghat/karta/internal/release"
	"github.com/pouriya-sedaghat/karta/internal/style"
)

// fixedLookup answers Lookup from a table; the active release is served.
type fixedLookup struct {
	active *release.Release
	others map[string]release.Lookup
	pinned map[string]*release.Release
}

func (f fixedLookup) Active() *release.Release { return f.active }
func (f fixedLookup) Lookup(id string) (*release.Release, release.Lookup) {
	if f.active != nil && id == f.active.Info.ReleaseID {
		return f.active, release.Served
	}
	if r, ok := f.pinned[id]; ok {
		return r, release.Served
	}
	return nil, f.others[id]
}
func (f fixedLookup) Status() release.Status {
	return release.Status{Ready: true, Reason: release.ReasonReady, ReleaseID: f.active.Info.ReleaseID}
}

const (
	expiredID     = "r111111111111111111111111"
	unavailableID = "r222222222222222222222222"
	unknownID     = "r333333333333333333333333"
)

// A pinned release that is no longer served is 410 release_expired on every
// release-pinned path; an unknown id stays 404 unknown_release; one that
// cannot be loaded is 503 with Retry-After. None of them is cacheable.
func TestExpiredUnavailableAndUnknownPins(t *testing.T) {
	cfg, err := region.Load("../../config/regions/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	active := testRelease(cfg, releaseIDFor(t, cfg))
	h := testHandlerWith(t, fixedLookup{active: active, others: map[string]release.Lookup{
		expiredID: release.Expired, unavailableID: release.Unavailable}})
	paths := func(id string) []string {
		return []string{
			"/v1/releases/" + id + "/tiles/14/8192/8191.pbf",
			"/v1/releases/" + id + "/style.json",
			"/v1/releases/" + id + "/styles/" + strings.Repeat("a", 32) + ".json",
			"/v1/search?q=lake&release_id=" + id,
		}
	}
	for _, p := range paths(expiredID) {
		expectErr(t, call(t, h, http.MethodGet, p, nil, nil), http.StatusGone, CodeReleaseExpired, "release_id")
	}
	for _, p := range paths(unknownID) {
		expectErr(t, call(t, h, http.MethodGet, p, nil, nil), http.StatusNotFound, CodeUnknownRelease, "release_id")
	}
	for _, p := range paths(unavailableID) {
		rec := call(t, h, http.MethodGet, p, nil, nil)
		expectErr(t, rec, http.StatusServiceUnavailable, CodeUnavailable, "release_id")
		if rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s: no Retry-After", p)
		}
	}
}

// A retired release inside its pin grace serves its own style at its own
// URL next to the active release; the manifest names only the active one.
func TestPinnedReleaseServedNextToActive(t *testing.T) {
	cfg, err := region.Load("../../config/regions/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	other := cfg
	other.View.Zoom = 14
	active := testRelease(cfg, releaseIDFor(t, cfg))
	active.ActivatedAt = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	until := time.Now().Add(time.Hour)
	old := testRelease(other, releaseIDFor(t, other))
	old.PinnedUntil = &until
	h := testHandlerWith(t, fixedLookup{active: active, pinned: map[string]*release.Release{old.Info.ReleaseID: old}})
	m, rec := manifestOf(t, h)
	if m.Release.ReleaseID != active.Info.ReleaseID || !strings.Contains(rec.Body.String(), `"activated_at":"2026-09-29T10:00:00Z"`) ||
		!strings.Contains(rec.Body.String(), `"update_mode":"manual"`) || !strings.Contains(rec.Body.String(), `"manual_updates":true`) {
		t.Fatalf("manifest %s", rec.Body)
	}
	red := call(t, h, http.MethodGet, "/v1/releases/"+old.Info.ReleaseID+"/style.json", nil, nil)
	if red.Code != http.StatusTemporaryRedirect || !strings.Contains(red.Header().Get("Location"), "/v1/releases/"+old.Info.ReleaseID+"/styles/") {
		t.Fatalf("pinned style redirect %d %v", red.Code, red.Header())
	}
	s := getURL(t, h, red.Header().Get("Location"), nil)
	if s.Code != http.StatusOK || !strings.Contains(s.Body.String(), `"zoom":14`) {
		t.Fatalf("pinned style %d %s", s.Code, s.Body)
	}
}

func testHandlerWith(t *testing.T, r Releases) http.Handler {
	t.Helper()
	g := testGlyphs(t)
	h, err := newHandler(Config{PublicBaseURL: "http://localhost:8080", RequestTimeout: 5 * time.Second, WebDir: t.TempDir()},
		r, g, discardLogger(), style.Render)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func testGlyphs(t *testing.T) *glyphs.Set {
	t.Helper()
	g, err := glyphs.New()
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
