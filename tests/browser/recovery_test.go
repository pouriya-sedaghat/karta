//go:build browser

package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// staleRelease is a well-formed release id no server serves.
const staleRelease = "r000000000000000000000000"

var styleIDPattern = regexp.MustCompile(`[0-9a-f]{32}\.json$`)

// manifestSwap intercepts the page's /v1/manifest responses. The ones for
// which stale(n) is true (n counts from 1) are rewritten into what a manifest
// fetched from the API before an upgrade looks like to the API after it: a
// style URL, or release, that is no longer served. The rest pass through. It
// keeps the release id the server actually returned.
type manifestSwap struct {
	stale   func(n int) bool
	rewrite func(m map[string]any)

	mu          sync.Mutex
	n           int
	realRelease string
}

// pageLog records the requests a tab makes and the statuses it gets.
type pageLog struct {
	mu       sync.Mutex
	urls     []string
	statuses map[string][]int64
	errs     []string
}

func (l *pageLog) count(contains string) (n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, u := range l.urls {
		if strings.Contains(u, contains) {
			n++
		}
	}
	return n
}

func (l *pageLog) find(contains string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, u := range l.urls {
		if strings.Contains(u, contains) {
			out = append(out, u)
		}
	}
	return out
}

func (l *pageLog) status(u string) []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int64(nil), l.statuses[u]...)
}

// openDemo opens the demo in a new tab of the browser with the manifest swap
// in place. The tab's HTTP cache is disabled, so every request reaches the
// server (or the swap).
func openDemo(t *testing.T, browser context.Context, base string, swap *manifestSwap) (context.Context, *pageLog) {
	t.Helper()
	tab, cancel := chromedp.NewContext(browser)
	t.Cleanup(cancel)
	log := &pageLog{statuses: map[string][]int64{}}
	chromedp.ListenTarget(tab, func(ev any) {
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			log.mu.Lock()
			log.urls = append(log.urls, e.Request.URL)
			log.mu.Unlock()
		case *network.EventResponseReceived:
			log.mu.Lock()
			log.statuses[e.Response.URL] = append(log.statuses[e.Response.URL], e.Response.Status)
			log.mu.Unlock()
		case *runtime.EventExceptionThrown:
			log.mu.Lock()
			log.errs = append(log.errs, e.ExceptionDetails.Error())
			log.mu.Unlock()
		case *fetch.EventRequestPaused:
			// Answer from a goroutine: the listener must not block on CDP.
			go swap.answer(tab, e)
		}
	})
	started := time.Now()
	if err := chromedp.Run(tab, network.Enable()); err != nil {
		t.Fatal(err)
	}
	logStarted(t, tab, started)
	err := chromedp.Run(tab, runtime.Enable(), network.SetCacheDisabled(true),
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*/v1/manifest*", RequestStage: fetch.RequestStageResponse}}),
		chromedp.Navigate(base+"/demo/"))
	if err != nil {
		t.Fatal(err)
	}
	return tab, log
}

func (s *manifestSwap) answer(tab context.Context, e *fetch.EventRequestPaused) {
	ctx := cdp.WithExecutor(tab, chromedp.FromContext(tab).Target)
	body, err := fetch.GetResponseBody(e.RequestID).Do(ctx)
	if err != nil {
		_ = fetch.ContinueRequest(e.RequestID).Do(ctx)
		return
	}
	s.mu.Lock()
	s.n++
	n := s.n
	var m map[string]any
	if e.ResponseStatusCode == 200 && json.Unmarshal(body, &m) == nil {
		if rel, ok := m["release"].(map[string]any); ok {
			s.realRelease, _ = rel["release_id"].(string)
		}
		if s.stale(n) {
			s.rewrite(m)
			body, _ = json.Marshal(m)
		}
	}
	s.mu.Unlock()
	// The rewritten body must not be revalidated against the real ETag.
	var headers []*fetch.HeaderEntry
	for _, h := range e.ResponseHeaders {
		switch strings.ToLower(h.Name) {
		case "etag", "content-length", "cache-control":
		default:
			headers = append(headers, h)
		}
	}
	headers = append(headers, &fetch.HeaderEntry{Name: "Cache-Control", Value: "no-store"})
	_ = fetch.FulfillRequest(e.RequestID, e.ResponseStatusCode).WithResponseHeaders(headers).
		WithBody(base64.StdEncoding.EncodeToString(body)).Do(ctx)
}

func (s *manifestSwap) real() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.realRelease
}

func (s *manifestSwap) served() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// staleStyle makes the manifest name a style id the API does not produce, as
// one issued before an upgrade or a base URL change does.
func staleStyle(m map[string]any) {
	m["style_url"] = styleIDPattern.ReplaceAllString(m["style_url"].(string), strings.Repeat("0", 32)+".json")
}

// staleReleaseAndStyle makes the manifest name a release that is no longer
// served, as one fetched before an upgrade that also replaced the release.
func staleReleaseAndStyle(m map[string]any) {
	u := m["style_url"].(string)
	real := m["release"].(map[string]any)["release_id"].(string)
	m["style_url"] = styleIDPattern.ReplaceAllString(strings.Replace(u, real, staleRelease, 1), strings.Repeat("0", 32)+".json")
	m["release"].(map[string]any)["release_id"] = staleRelease
}

// searchRelease submits a search in the demo and returns the release_id the
// request carried and its status.
func searchRelease(t *testing.T, tab context.Context, log *pageLog, q string) (string, []int64) {
	t.Helper()
	sctx, cancel := context.WithTimeout(tab, 30*time.Second)
	defer cancel()
	if err := chromedp.Run(sctx, chromedp.SetValue("#q", q, chromedp.ByID),
		chromedp.Click("#search button[type=submit]", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if found := log.find("/v1/search?"); len(found) > 0 {
			if st := log.status(found[0]); len(st) > 0 {
				u, err := url.Parse(found[0])
				if err != nil {
					t.Fatal(err)
				}
				return u.Query().Get("release_id"), st
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the demo sent no search request")
	return "", nil
}

// An upgrade between the demo's manifest request and its style request (the
// API before the upgrade issued the manifest, the API after it gets the style
// request) must not leave the map broken: the demo refetches the manifest,
// loads the style it names and searches that manifest's release. A persistent
// failure must end after a bounded number of attempts.
func TestDemoRecoversFromStaleStyleURL(t *testing.T) {
	browser, base, _ := startBrowser(t)
	q := testSearch(t).Q

	for _, tc := range []struct {
		name    string
		rewrite func(map[string]any)
		code    string
	}{
		{"stale style id (unknown_style)", staleStyle, "unknown_style"},
		{"stale release (unknown_release)", staleReleaseAndStyle, "unknown_release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			swap := &manifestSwap{stale: func(n int) bool { return n == 1 }, rewrite: tc.rewrite}
			tab, log := openDemo(t, browser, base, swap)
			ictx, cancel := context.WithTimeout(tab, 60*time.Second)
			defer cancel()
			if err := chromedp.Run(ictx, waitIdle()); err != nil {
				var status string
				_ = chromedp.Run(tab, chromedp.Text("#status", &status, chromedp.ByID))
				t.Fatalf("map did not load after the stale manifest: %v; status %q", err, status)
			}
			styles := log.find("/styles/")
			if swap.served() != 2 || len(styles) != 2 || strings.Contains(styles[1], "/styles/"+strings.Repeat("0", 32)) {
				t.Fatalf("manifests %d, style requests %v", swap.served(), styles)
			}
			if st := log.status(styles[0]); len(st) != 1 || st[0] != 404 {
				t.Errorf("stale style URL %s: %v, want 404 %s", styles[0], st, tc.code)
			}
			if st := log.status(styles[1]); len(st) != 1 || st[0] != 200 {
				t.Errorf("current style URL %s: %v", styles[1], st)
			}
			var shown string
			if err := chromedp.Run(tab, chromedp.Evaluate(`window.kartaRelease`, &shown)); err != nil {
				t.Fatal(err)
			}
			real := swap.real()
			if real == "" || shown != real {
				t.Fatalf("the demo shows release %q, the server serves %q", shown, real)
			}
			rel, st := searchRelease(t, tab, log, q)
			if rel != real || len(st) != 1 || st[0] != 200 {
				t.Errorf("search used release_id %q (%v), want the current %s", rel, st, real)
			}
			if len(log.errs) > 0 {
				t.Errorf("exceptions: %v", log.errs)
			}
		})
	}

	t.Run("a persistent stale style URL gives up after three attempts", func(t *testing.T) {
		swap := &manifestSwap{stale: func(int) bool { return true }, rewrite: staleStyle}
		tab, log := openDemo(t, browser, base, swap)
		var status string
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if err := chromedp.Run(tab, chromedp.Text("#status", &status, chromedp.ByID)); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(status, "Could not load the map") {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !strings.HasPrefix(status, "Could not load the map") {
			t.Fatalf("status %q", status)
		}
		// Nothing more is requested once the demo has given up.
		time.Sleep(3 * time.Second)
		if n, s := swap.served(), log.count("/styles/"); n != 3 || s != 3 {
			t.Errorf("manifest requests %d, style requests %d; want 3 each", n, s)
		}
		var hasMap bool
		if err := chromedp.Run(tab, chromedp.Evaluate(`!!window.kartaMap`, &hasMap)); err != nil {
			t.Fatal(err)
		}
		if hasMap {
			t.Error("a map was created from a style that could not be loaded")
		}
		t.Logf("status after giving up: %q", status)
	})
}
