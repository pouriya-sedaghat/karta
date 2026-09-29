//go:build browser

package browser

// Path-prefix deployment: with KARTA_TEST_PREFIX_UPSTREAM set, every browser
// test in this package runs through a reverse proxy that serves Karta under
// the path of KARTA_TEST_BASE_URL (e.g. http://127.0.0.1:18091/maps) and
// strips it before forwarding to the API at KARTA_TEST_PREFIX_UPSTREAM, as
// `location /maps/ { proxy_pass http://karta:8080/; }` does in nginx. The
// proxy answers 404 to anything outside the prefix and does not rewrite
// Location headers, so the demo and the API must keep the prefix themselves.
// The API must run with KARTA_PUBLIC_BASE_URL set to the same prefixed URL
// (`make test-browser-prefix`).

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// pathProxy is the prefix-stripping reverse proxy. It logs every request it
// receives, including those of MapLibre's web worker (tiles), which the page's
// CDP network events do not show.
type pathProxy struct {
	prefix string
	rp     *httputil.ReverseProxy

	mu      sync.Mutex
	log     []proxied // every request, with the status the client got
	outside []string  // paths outside the prefix (answered 404)
}

type proxied struct {
	uri, location string
	status        int
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (p *pathProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sw := &statusWriter{ResponseWriter: w}
	inside := strings.HasPrefix(r.URL.Path, p.prefix+"/")
	switch {
	case r.URL.Path == p.prefix:
		// nginx redirects the bare location to its trailing-slash form.
		http.Redirect(sw, r, p.prefix+"/", http.StatusMovedPermanently)
	case inside:
		p.rp.ServeHTTP(sw, r)
	default:
		http.Error(sw, "outside the proxied prefix", http.StatusNotFound)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log = append(p.log, proxied{uri: r.URL.RequestURI(), location: sw.Header().Get("Location"), status: sw.status})
	if !inside && r.URL.Path != p.prefix {
		p.outside = append(p.outside, r.URL.RequestURI())
	}
}

// since returns the requests logged after the first n.
func (p *pathProxy) since(n int) []proxied {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]proxied(nil), p.log[n:]...)
}

func (p *pathProxy) logged() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.log)
}

var prefixProxy *pathProxy

func TestMain(m *testing.M) {
	upstream := os.Getenv("KARTA_TEST_PREFIX_UPSTREAM")
	if upstream == "" {
		os.Exit(m.Run())
	}
	stop, err := startPathProxy(strings.TrimRight(env("KARTA_TEST_BASE_URL", ""), "/"), upstream)
	if err != nil {
		fmt.Fprintln(os.Stderr, "path-prefix proxy:", err)
		os.Exit(1)
	}
	code := m.Run()
	stop()
	os.Exit(code)
}

func startPathProxy(base, upstream string) (func(), error) {
	bu, err := url.Parse(base)
	if err != nil || bu.Path == "" || bu.Path == "/" {
		return nil, fmt.Errorf("KARTA_TEST_BASE_URL %q must have a path prefix", base)
	}
	up, err := url.Parse(upstream)
	if err != nil || up.Host == "" {
		return nil, fmt.Errorf("KARTA_TEST_PREFIX_UPSTREAM %q is not an origin", upstream)
	}
	p := &pathProxy{prefix: bu.Path}
	p.rp = &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(up)
		pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, p.prefix)
		pr.Out.URL.RawPath = strings.TrimPrefix(pr.In.URL.RawPath, p.prefix)
		pr.SetXForwarded()
	}}
	ln, err := net.Listen("tcp", bu.Host)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "path-prefix proxy:", err)
		}
	}()
	prefixProxy = p
	fmt.Fprintf(os.Stderr, "path-prefix proxy: %s -> %s (prefix %s stripped)\n", base, upstream, p.prefix)
	return func() { _ = srv.Close() }, nil
}

// The demo behind the prefix: the prefix root and the bare /demo redirect to
// <prefix>/demo/, and the manifest, style, tiles, glyphs and search are all
// requested under the prefix and answered by the API.
func TestDemoBehindPathPrefix(t *testing.T) {
	if prefixProxy == nil {
		t.Skip("KARTA_TEST_PREFIX_UPSTREAM is not set; the root deployment is covered by the other tests")
	}
	ctx, base, _ := startBrowser(t)
	sc := testSearch(t)

	type redirect struct {
		from, location string
		status         int64
	}
	var (
		mu        sync.Mutex
		requests  []string
		redirects []redirect
	)
	chromedp.ListenTarget(ctx, func(ev any) {
		mu.Lock()
		defer mu.Unlock()
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			requests = append(requests, e.Request.URL)
			if r := e.RedirectResponse; r != nil {
				loc, _ := r.Headers["Location"].(string)
				redirects = append(redirects, redirect{r.URL, loc, r.Status})
			}
		}
	})
	started := time.Now()
	if err := chromedp.Run(ctx, network.Enable()); err != nil {
		t.Fatal(err)
	}
	logStarted(t, ctx, started)

	start := prefixProxy.logged()
	demo := base + "/demo/"
	var loc string
	lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := chromedp.Run(lctx, chromedp.Navigate(base+"/"), chromedp.Location(&loc), waitIdle()); err != nil {
		var status string
		_ = chromedp.Run(ctx, chromedp.Text("#status", &status, chromedp.ByID))
		t.Fatalf("demo at %s/ did not load: %v; location %s; status %q", base, err, loc, status)
	}
	if loc != demo {
		t.Errorf("%s/ led to %s, want %s", base, loc, demo)
	}

	var first string
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := chromedp.Run(sctx,
		chromedp.SetValue("#q", sc.Q, chromedp.ByID),
		chromedp.Click("#search button[type=submit]", chromedp.ByQuery),
		chromedp.WaitVisible("#results li bdi", chromedp.ByQuery),
		chromedp.Text("#results li bdi", &first, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("search behind the prefix: %v", err)
	}
	if first != sc.Expect {
		t.Errorf("first search result %q, want %q", first, sc.Expect)
	}

	bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := chromedp.Run(bctx, chromedp.Navigate(base+"/demo"), chromedp.Location(&loc)); err != nil {
		t.Fatal(err)
	}
	if loc != demo {
		t.Errorf("%s/demo led to %s, want %s", base, loc, demo)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, want := range []redirect{{base + "/", "demo/", 302}, {base + "/demo", "demo/", 302}} {
		found := false
		for _, r := range redirects {
			found = found || r == want
		}
		if !found {
			t.Errorf("no %d redirect from %s to %q; redirects seen: %+v", want.status, want.from, want.location, redirects)
		}
	}
	// Each kind of request the demo makes (the page's and its worker's), as
	// the proxy received it: under the prefix, and answered by the API.
	seen := prefixProxy.since(start)
	pre := regexp.QuoteMeta(prefixProxy.prefix)
	kinds := []struct {
		name string
		re   *regexp.Regexp
	}{
		{"demo page", regexp.MustCompile(`^` + pre + `/demo/$`)},
		{"manifest", regexp.MustCompile(`^` + pre + `/v1/manifest$`)},
		{"style", regexp.MustCompile(`^` + pre + `/v1/releases/r[0-9a-f]{24}/styles/[0-9a-f]{32}\.json$`)},
		{"tile", regexp.MustCompile(`^` + pre + `/v1/releases/r[0-9a-f]{24}/tiles/\d+/\d+/\d+\.pbf$`)},
		{"glyph", regexp.MustCompile(`^` + pre + `/v1/fonts/[^/]+/\d+-\d+\.pbf$`)},
		{"search", regexp.MustCompile(`^` + pre + `/v1/search\?`)},
	}
	for _, k := range kinds {
		n, ok := 0, 0
		for _, r := range seen {
			if k.re.MatchString(r.uri) {
				n++
				if r.status == 200 || r.status == 204 || r.status == 304 {
					ok++
				}
			}
		}
		t.Logf("%s: %d requests under %s/, %d answered 200/204/304", k.name, n, prefixProxy.prefix, ok)
		if n == 0 || ok != n {
			t.Errorf("%s: %d requests under the prefix, %d answered 200/204/304", k.name, n, ok)
		}
	}
	for _, want := range []proxied{{prefixProxy.prefix + "/", "demo/", 302}, {prefixProxy.prefix + "/demo", "demo/", 302}} {
		found := false
		for _, r := range seen {
			found = found || r == want
		}
		if !found {
			t.Errorf("the proxy did not pass on a %d from %s with Location %q", want.status, want.uri, want.location)
		}
	}
	var outside []string
	for _, u := range requests {
		if !strings.HasPrefix(u, base+"/") && u != base && !strings.HasPrefix(u, "data:") && !strings.HasPrefix(u, "blob:") {
			outside = append(outside, u)
		}
	}
	if len(outside) > 0 {
		t.Errorf("the page requested URLs outside %s/: %v", base, outside)
	}
	prefixProxy.mu.Lock()
	defer prefixProxy.mu.Unlock()
	if len(prefixProxy.outside) > 0 {
		t.Errorf("requests reached the proxy outside the prefix %s: %v", prefixProxy.prefix, prefixProxy.outside)
	}
	t.Logf("page requests: %d, all under %s/; the proxy received %d in this test, none outside the prefix", len(requests), base, len(seen))
}
