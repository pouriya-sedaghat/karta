package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

// stripPrefix is a reverse proxy that serves upstream under prefix and strips
// it, as `location /maps/ { proxy_pass http://karta:8080/; }` does in nginx.
// It does not rewrite Location headers, so redirects must keep the prefix
// themselves.
func stripPrefix(prefix string, upstream *url.URL) http.Handler {
	rp := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(upstream)
		pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
		pr.Out.URL.RawPath = strings.TrimPrefix(pr.In.URL.RawPath, prefix)
	}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix+"/") {
			http.NotFound(w, r)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// The demo entry points redirect relatively, so they land on the demo both at
// the root and behind a proxy that serves Karta under /maps/.
func TestDemoRedirectsKeepAPathPrefix(t *testing.T) {
	h := newServer(t)
	for _, p := range []string{"/", "/demo"} {
		if rec := call(t, h, "GET", p, nil, nil); rec.Code != http.StatusFound || rec.Header().Get("Location") != "demo/" {
			t.Errorf("GET %s: %d, Location %q; want 302 to demo/", p, rec.Code, rec.Header().Get("Location"))
		}
	}
	api := httptest.NewServer(h)
	defer api.Close()
	up, err := url.Parse(api.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(stripPrefix("/maps", up))
	defer proxy.Close()
	for _, c := range []struct{ from, want string }{
		{api.URL + "/", "/demo/"},
		{api.URL + "/demo", "/demo/"},
		{proxy.URL + "/maps/", "/maps/demo/"},
		{proxy.URL + "/maps/demo", "/maps/demo/"},
	} {
		resp, err := http.Get(c.from)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != c.want || !strings.Contains(string(body), "<title>") {
			t.Errorf("%s ended at %s with %d, want the demo at %s", c.from, resp.Request.URL.Path, resp.StatusCode, c.want)
		}
	}
}
