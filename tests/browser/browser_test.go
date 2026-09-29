//go:build browser

// Browser test: loads the MapLibre demo in headless Chromium (WebGL via
// SwiftShader) against a running stack and checks what is actually rendered.
//
// The browser is started with a proxy that does not exist, bypassed only for
// the Karta host, so any request to another host fails, and every request is
// recorded and checked against the Karta origin: the map must work with no
// network other than the Karta server.
//
// Environment:
//
//	KARTA_TEST_BASE_URL    Karta public base URL as configured in KARTA_PUBLIC_BASE_URL
//	                       (default http://localhost:8080); the demo CSP allows only that origin
//	KARTA_TEST_RESOLVE     optional "host=ip" to map the base URL host to an IP
//	                       (used for the disconnected-network run)
//	KARTA_TEST_CHROME      Chromium binary (default: /opt/pw-browsers/chromium if present)
//	KARTA_TEST_VIEWS       JSON views to check (default: fixture views)
//	KARTA_TEST_SEARCH      JSON {"q": ..., "expect": ...} for the demo search box
//	KARTA_TEST_ARTIFACTS   directory for screenshots (default artifacts/browser)
//	KARTA_TEST_PREFIX_UPSTREAM
//	                       optional API origin; when set, all tests run through a
//	                       reverse proxy serving Karta under the path of
//	                       KARTA_TEST_BASE_URL (prefix_test.go)
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

type view struct {
	Name string `json:"name"`
	// Hash is the demo URL fragment zoom/lat/lon.
	Hash string `json:"hash"`
	// Layers are style layer ids that must have rendered features.
	Layers []string `json:"layers"`
	// Labels are names that must be placed by a symbol layer.
	Labels []string `json:"labels"`
}

var defaultViews = []view{
	{
		Name:   "fixture-lake",
		Hash:   "16/0.0035/0.0040",
		Layers: []string{"water", "landcover-urban", "landcover-natural", "paths", "poi-labels"},
		Labels: []string{"دریاچه آزمون", "پارک نمونه"},
	},
	{
		Name:   "fixture-centre",
		Hash:   "15/0.0080/0.0090",
		Layers: []string{"landcover-natural", "roads-fill", "place-labels", "road-labels"},
		Labels: []string{"محله آزمون", "بزرگراه آزمون"},
	},
}

type searchCheck struct {
	Q      string `json:"q"`
	Expect string `json:"expect"`
}

var defaultSearch = searchCheck{Q: "درياچه", Expect: "دریاچه آزمون"}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type recorder struct {
	mu        sync.Mutex
	requests  []string
	failed    []string
	statuses  map[string]int64
	console   []string
	exception []string
}

// startBrowser launches headless Chromium whose only network is the Karta
// host and returns a tab context, the Karta base URL and its parsed form.
func startBrowser(t *testing.T) (context.Context, string, *url.URL) {
	t.Helper()
	base := strings.TrimRight(env("KARTA_TEST_BASE_URL", "http://localhost:8080"), "/")
	bu, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", "new"),
		chromedp.Flag("use-angle", "swiftshader"),
		chromedp.Flag("enable-unsafe-swiftshader", true),
		chromedp.Flag("ignore-gpu-blocklist", true),
		chromedp.NoSandbox,
		// No network except the Karta host: everything else goes to a dead proxy.
		chromedp.ProxyServer("http://127.0.0.1:9"),
		chromedp.Flag("proxy-bypass-list", "<-loopback>;"+bu.Hostname()),
		chromedp.WindowSize(1280, 900),
		// chromedp gives Chromium 20 s to print its DevTools address by
		// default. On shared CI runners the first start has taken about that
		// long (run 36536186101: "websocket url timeout reached", and the next
		// start took ~20 s too), which is the runner, not the map. A browser
		// that never starts still fails the test.
		chromedp.WSURLReadTimeout(2*time.Minute),
	)
	if resolve := os.Getenv("KARTA_TEST_RESOLVE"); resolve != "" {
		host, ip, ok := strings.Cut(resolve, "=")
		if !ok {
			t.Fatalf("KARTA_TEST_RESOLVE must be host=ip")
		}
		opts = append(opts, chromedp.Flag("host-resolver-rules", fmt.Sprintf("MAP %s %s", host, ip)))
	}
	chrome := os.Getenv("KARTA_TEST_CHROME")
	if chrome == "" {
		if _, err := os.Stat("/opt/pw-browsers/chromium"); err == nil {
			chrome = "/opt/pw-browsers/chromium"
		}
	}
	if chrome != "" {
		opts = append(opts, chromedp.ExecPath(chrome))
	}
	waitForQuietHostNetwork(t, 2*time.Second, 30*time.Second)
	actx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancelTab := chromedp.NewContext(actx)
	ctx, cancelTimeout := context.WithTimeout(ctx, 4*time.Minute)
	t.Cleanup(func() { cancelTimeout(); cancelTab(); cancelAlloc() })
	// The browser starts lazily, on the first chromedp.Run of ctx or of a
	// child context, so each test's page is the browser's only tab. Starting
	// it here kept an extra tab in front, and on the CI runner's Chrome 153 the
	// recovery test's maps in tabs behind it never became idle (run
	// 36536765536).
	return ctx, base, bu
}

// logStarted logs the browser version and how long the first chromedp.Run,
// which starts the browser, took.
func logStarted(t *testing.T, ctx context.Context, since time.Time) {
	t.Helper()
	elapsed := time.Since(since)
	product := "Chromium"
	_ = chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		_, p, _, _, _, err := browser.GetVersion().Do(c)
		if err == nil {
			product = p
		}
		return err
	}))
	t.Logf("%s started and attached in %.1fs", product, elapsed.Seconds())
}

// testSearch is the demo search to run (KARTA_TEST_SEARCH or the fixture's).
func testSearch(t *testing.T) searchCheck {
	t.Helper()
	sc := defaultSearch
	if v := os.Getenv("KARTA_TEST_SEARCH"); v != "" {
		if err := json.Unmarshal([]byte(v), &sc); err != nil {
			t.Fatalf("KARTA_TEST_SEARCH: %v", err)
		}
	}
	return sc
}

func TestDemoRendersOffline(t *testing.T) {
	views := defaultViews
	if v := os.Getenv("KARTA_TEST_VIEWS"); v != "" {
		if err := json.Unmarshal([]byte(v), &views); err != nil {
			t.Fatalf("KARTA_TEST_VIEWS: %v", err)
		}
	}
	sc := testSearch(t)
	artifacts := env("KARTA_TEST_ARTIFACTS", "artifacts/browser")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, base, _ := startBrowser(t)

	rec := &recorder{statuses: map[string]int64{}}
	chromedp.ListenTarget(ctx, func(ev any) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			rec.requests = append(rec.requests, e.Request.URL)
		case *network.EventResponseReceived:
			rec.statuses[e.Response.URL] = e.Response.Status
		case *network.EventLoadingFailed:
			rec.failed = append(rec.failed, fmt.Sprintf("%s: %s", e.RequestID, e.ErrorText))
		case *runtime.EventConsoleAPICalled:
			if e.Type == runtime.APITypeError || e.Type == runtime.APITypeWarning {
				var parts []string
				for _, a := range e.Args {
					parts = append(parts, strings.Trim(string(a.Value), `"`)+a.Description)
				}
				rec.console = append(rec.console, string(e.Type)+": "+strings.Join(parts, " "))
			}
		case *runtime.EventExceptionThrown:
			rec.exception = append(rec.exception, e.ExceptionDetails.Error())
		}
	})
	started := time.Now()
	if err := chromedp.Run(ctx, network.Enable(), runtime.Enable()); err != nil {
		t.Fatal(err)
	}
	logStarted(t, ctx, started)

	for i, v := range views {
		t.Run(v.Name, func(t *testing.T) {
			target := base + "/demo/#" + v.Hash
			actions := []chromedp.Action{}
			if i == 0 {
				actions = append(actions, chromedp.Navigate(target))
			} else {
				actions = append(actions, chromedp.Evaluate(fmt.Sprintf(`location.hash = %q`, v.Hash), nil))
			}
			vctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			if err := chromedp.Run(vctx, append(actions, waitIdle())...); err != nil {
				var status string
				_ = chromedp.Run(ctx, chromedp.Text("#status", &status, chromedp.ByID))
				rec.mu.Lock()
				console, exceptions := append([]string(nil), rec.console...), append([]string(nil), rec.exception...)
				rec.mu.Unlock()
				t.Fatalf("map did not become idle: %v; page status %q; console %v; exceptions %v", err, status, console, exceptions)
			}
			var rendered struct {
				Layers map[string]int `json:"layers"`
				Labels []string       `json:"labels"`
			}
			if err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
				const layers = {}, labels = [];
				for (const f of window.kartaMap.queryRenderedFeatures()) {
					layers[f.layer.id] = (layers[f.layer.id] || 0) + 1;
					if (f.layer.type === 'symbol' && f.properties.name) labels.push(f.properties.name);
				}
				return {layers, labels};
			})()`, &rendered)); err != nil {
				t.Fatal(err)
			}
			var png []byte
			if err := chromedp.Run(ctx, chromedp.CaptureScreenshot(&png)); err != nil {
				t.Fatal(err)
			}
			shot := filepath.Join(artifacts, v.Name+".png")
			if err := os.WriteFile(shot, png, 0o644); err != nil {
				t.Fatal(err)
			}
			keys := make([]string, 0, len(rendered.Layers))
			for k, n := range rendered.Layers {
				keys = append(keys, fmt.Sprintf("%s=%d", k, n))
			}
			sort.Strings(keys)
			t.Logf("rendered layers: %s", strings.Join(keys, " "))
			t.Logf("placed labels (%d): %s", len(rendered.Labels), strings.Join(firstN(rendered.Labels, 25), " | "))
			t.Logf("screenshot: %s", shot)
			for _, l := range v.Layers {
				if rendered.Layers[l] == 0 {
					t.Errorf("style layer %q rendered no features", l)
				}
			}
			for _, want := range v.Labels {
				if !contains(rendered.Labels, want) {
					t.Errorf("label %q was not placed", want)
				}
			}
		})
	}

	t.Run("search", func(t *testing.T) {
		var first string
		sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		// Clicking the button fires the form's submit event, as a user would.
		err := chromedp.Run(sctx,
			chromedp.SetValue("#q", sc.Q, chromedp.ByID),
			chromedp.Click("#search button[type=submit]", chromedp.ByQuery),
			chromedp.WaitVisible("#results li bdi", chromedp.ByQuery),
			chromedp.Text("#results li bdi", &first, chromedp.ByQuery),
		)
		if err != nil {
			t.Fatal(err)
		}
		if first != sc.Expect {
			t.Errorf("first search result = %q, want %q", first, sc.Expect)
		}
		var png []byte
		if err := chromedp.Run(sctx, chromedp.Click("#results li button", chromedp.ByQuery), waitIdle(), chromedp.CaptureScreenshot(&png)); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(artifacts, "search.png"), png, 0o644)
	})

	t.Run("network", func(t *testing.T) {
		// Evaluate before locking: the CDP event listener takes the same lock.
		var mapErrors []string
		if err := chromedp.Run(ctx, chromedp.Evaluate(`window.kartaErrors || []`, &mapErrors)); err != nil {
			t.Fatal(err)
		}
		if len(mapErrors) > 0 {
			t.Errorf("map reported errors: %v", mapErrors)
		}
		rec.mu.Lock()
		defer rec.mu.Unlock()
		// The base URL, including any path prefix (`make test-browser-prefix`).
		origin := base + "/"
		var outside []string
		glyphRanges := map[string]int64{}
		for _, u := range rec.requests {
			if strings.HasPrefix(u, "data:") || strings.HasPrefix(u, "blob:") {
				continue
			}
			if !strings.HasPrefix(u, origin) {
				outside = append(outside, u)
			}
		}
		for u, st := range rec.statuses {
			if st >= 400 {
				t.Errorf("request failed with %d: %s", st, u)
			}
			if strings.Contains(u, "/v1/fonts/") {
				glyphRanges[u[strings.LastIndex(u, "/")+1:]] = st
			}
		}
		t.Logf("%d requests, all to %s: %v", len(rec.requests), origin, len(outside) == 0)
		t.Logf("glyph ranges fetched: %v", glyphRanges)
		if len(outside) > 0 {
			t.Errorf("requests left the Karta origin: %v", outside)
		}
		if len(rec.failed) > 0 {
			t.Errorf("failed loads: %v", rec.failed)
			if strings.Contains(strings.Join(rec.failed, " "), "ERR_NETWORK_CHANGED") {
				t.Logf("net::ERR_NETWORK_CHANGED is raised by Chromium, not the server: a host interface address changed during the test")
			}
		}
		// MapLibre shapes Persian into Arabic Presentation Forms-B (U+FE70-U+FEFF).
		if glyphRanges["65024-65279.pbf"] != 200 {
			t.Errorf("Persian presentation-form glyphs (65024-65279) were not fetched: %v", glyphRanges)
		}
		if len(rec.exception) > 0 {
			t.Errorf("uncaught exceptions: %v", rec.exception)
		}
		for _, c := range rec.console {
			t.Logf("console %s", c)
		}
	})
}

// waitForQuietHostNetwork waits until the host's interface addresses have not
// changed for quiet and no IPv6 address is still tentative. Chromium aborts
// in-flight requests with net::ERR_NETWORK_CHANGED whenever a host address
// changes, and the targets that run this test have just started containers:
// each gets a veth whose IPv6 link-local address finishes duplicate address
// detection a second or two later. After limit it logs and goes on.
func waitForQuietHostNetwork(t *testing.T, quiet, limit time.Duration) {
	start := time.Now()
	last, _ := hostAddrs()
	changed, changes := start, 0
	for {
		now := time.Now()
		cur, tentative := hostAddrs()
		if cur != last {
			last, changed = cur, now
			changes++
		}
		if !tentative && now.Sub(changed) >= quiet {
			t.Logf("host network quiet for %v after %d change(s), waited %v", quiet, changes, now.Sub(start).Round(time.Millisecond))
			return
		}
		if now.Sub(start) >= limit {
			t.Logf("host network still changing after %v (tentative IPv6: %v); starting the browser anyway", limit, tentative)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// hostAddrs returns a snapshot of every interface's state and addresses and
// whether an IPv6 address is tentative (package net does not expose IPv6
// address flags; /proc/net/if_inet6 does, on Linux).
func hostAddrs() (string, bool) {
	var b strings.Builder
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		addrs, _ := i.Addrs()
		fmt.Fprintf(&b, "%s %v %v\n", i.Name, i.Flags, addrs)
	}
	tentative := false
	if raw, err := os.ReadFile("/proc/net/if_inet6"); err == nil {
		b.Write(raw)
		for _, line := range strings.Split(string(raw), "\n") {
			// address ifindex prefixlen scope flags name; IFA_F_TENTATIVE = 0x40
			if f := strings.Fields(line); len(f) == 6 {
				if flags, err := strconv.ParseUint(f[4], 16, 32); err == nil && flags&0x40 != 0 {
					tentative = true
				}
			}
		}
	}
	return b.String(), tentative
}

// waitIdle waits until the map exists, its style and tiles are loaded and it
// has emitted "idle" (no pending tiles, transitions or label placement).
func waitIdle() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		deadline := time.Now().Add(55 * time.Second)
		for time.Now().Before(deadline) {
			var ok bool
			if err := chromedp.Evaluate(`!!(window.kartaMap && window.kartaMap.isStyleLoaded())`, &ok).Do(ctx); err != nil {
				return err
			}
			if ok {
				var done bool
				err := chromedp.Evaluate(`new Promise((resolve) => {
					const m = window.kartaMap;
					const t = setTimeout(() => resolve(m.loaded() && m.areTilesLoaded()), 20000);
					m.once('idle', () => { clearTimeout(t); resolve(true); });
					m.triggerRepaint();
				})`, &done, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }).Do(ctx)
				if err != nil {
					return err
				}
				if done {
					return nil
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		return fmt.Errorf("timed out waiting for the map")
	})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
