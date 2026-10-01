package online

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPolicyRefusesNonPublicAddresses(t *testing.T) {
	refused := []string{
		"127.0.0.1", "127.255.0.9", "0.0.0.0", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", "100.64.0.1",
		"169.254.169.254", "192.0.0.8", "192.0.2.1", "198.18.0.1", "198.51.100.7", "203.0.113.9", "224.0.0.1", "239.1.2.3",
		"240.0.0.1", "255.255.255.255", "::", "::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::a00:1", "2002:a00:1::",
		"fc00::1", "fd12:3456::1", "fe80::1", "ff02::1", "2001:db8::1", "2001::1", "100::1",
	}
	for _, s := range refused {
		if err := (Policy{}).Check(netip.MustParseAddr(s)); CodeOf(err) != CodeDestinationRefused {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "185.199.108.153", "2606:4700:4700::1111", "2a00:1450:4001::1"} {
		if err := (Policy{}).Check(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s refused: %v", s, err)
		}
	}
	p := Policy{Allowed: []netip.Prefix{netip.MustParsePrefix("10.231.0.0/24")}}
	if err := p.Check(netip.MustParseAddr("10.231.0.5")); err != nil {
		t.Errorf("allowed network refused: %v", err)
	}
	if err := p.Check(netip.MustParseAddr("10.232.0.5")); err == nil {
		t.Error("an address outside the allowed network was accepted")
	}
	if err := p.Check(netip.MustParseAddr("::ffff:10.231.0.5")); err != nil {
		t.Errorf("IPv4-mapped allowed address refused: %v", err)
	}
}

// tlsServer serves h over TLS on 127.0.0.1 and returns a source trusting
// only its certificate (and, with allowLoopback, the loopback network).
func tlsServer(t *testing.T, h http.Handler, allowLoopback bool) (*httptest.Server, *Requester) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	var allowed []netip.Prefix
	if allowLoopback {
		allowed = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	}
	c := NewHTTPClient(ClientOptions{Policy: Policy{Allowed: allowed}, Roots: pool, DialTimeout: 2 * time.Second,
		HeaderTimeout: 2 * time.Second, HandshakeLimit: 2 * time.Second})
	return srv, &Requester{Client: c, UserAgent: "karta-fetcher/test", Token: "secret-token"}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestClientRefusesLoopbackUnlessAllowed(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv, r := tlsServer(t, h, false)
	if _, err := r.FetchSmall(context.Background(), mustURL(t, srv.URL+"/m"), 100, 5*time.Second); CodeOf(err) != CodeDestinationRefused {
		t.Fatalf("loopback not refused: %v", err)
	}
	srv2, r2 := tlsServer(t, h, true)
	if b, err := r2.FetchSmall(context.Background(), mustURL(t, srv2.URL+"/m"), 100, 5*time.Second); err != nil || string(b) != "ok" {
		t.Fatalf("allowed loopback: %q %v", b, err)
	}
}

func TestClientIgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	srv, r := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("direct")) }), true)
	if b, err := r.FetchSmall(context.Background(), mustURL(t, srv.URL+"/m"), 100, 5*time.Second); err != nil || string(b) != "direct" {
		t.Fatalf("%q %v", b, err)
	}
}

func TestClientVerifiesTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	c := NewHTTPClient(ClientOptions{Policy: Policy{Allowed: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}},
		Roots: x509.NewCertPool(), DialTimeout: time.Second, HeaderTimeout: time.Second, HandshakeLimit: time.Second})
	r := &Requester{Client: c}
	if _, err := r.FetchSmall(context.Background(), mustURL(t, srv.URL+"/m"), 100, 5*time.Second); CodeOf(err) != CodeTLS {
		t.Fatalf("untrusted certificate: %v", err)
	}
}

func TestFetchSmall(t *testing.T) {
	var gotAuth, gotEnc, gotUA string
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotEnc, gotUA = r.Header.Get("Authorization"), r.Header.Get("Accept-Encoding"), r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("hello"))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(bytes.Repeat([]byte("x"), 200)) })
	mux.HandleFunc("/big-chunked", func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 20; i++ {
			_, _ = w.Write(bytes.Repeat([]byte("x"), 20))
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) })
	mux.HandleFunc("/gzip", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write([]byte("hello"))
	})
	mux.HandleFunc("/missing", http.NotFound)
	mux.HandleFunc("/short", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "50")
		_, _ = w.Write([]byte("only a few"))
	})
	mux.HandleFunc("/slow-headers", func(w http.ResponseWriter, _ *http.Request) { time.Sleep(3 * time.Second) })
	srv, r := tlsServer(t, mux, true)
	ctx := context.Background()
	if b, err := r.FetchSmall(ctx, mustURL(t, srv.URL+"/ok"), 100, 5*time.Second); err != nil || string(b) != "hello" {
		t.Fatalf("%q %v", b, err)
	}
	if gotAuth != "Bearer secret-token" || gotEnc != "identity" || gotUA != "karta-fetcher/test" {
		t.Errorf("headers: auth %q encoding %q agent %q", gotAuth, gotEnc, gotUA)
	}
	for path, code := range map[string]string{
		"/big": CodeTooLarge, "/big-chunked": CodeTooLarge, "/redirect": CodeRedirectRefused, "/gzip": CodeEncoding,
		"/missing": CodeHTTPStatus, "/short": CodeTruncated, "/slow-headers": CodeTimeout,
	} {
		_, err := r.FetchSmall(ctx, mustURL(t, srv.URL+path), 100, 5*time.Second)
		if CodeOf(err) != code {
			t.Errorf("%s: %v, want %s", path, err, code)
		}
		if err != nil && strings.Contains(err.Error(), "secret-token") {
			t.Errorf("%s: the token leaked into an error", path)
		}
	}
}

// snapshotServer serves body at /snap with configurable misbehaviour and
// records the Range and If-Range headers it saw.
type snapshotServer struct {
	mu          sync.Mutex
	body        []byte
	etag        string
	cutAfter    int  // close the connection after this many bytes (first cuts times)
	cuts        int  // remaining cuts
	ignoreRange bool // answer 200 to range requests
	badRange    bool // answer a wrong Content-Range
	extra       int  // append bytes
	stallAfter  int  // stop sending after this many bytes, keep the connection
	flip        bool // flip the last byte
	requests    []string
}

func (s *snapshotServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Header.Get("Range")+"|"+r.Header.Get("If-Range"))
	body := append([]byte(nil), s.body...)
	if s.flip {
		body[len(body)-1] ^= 0xff
	}
	body = append(body, bytes.Repeat([]byte("z"), s.extra)...)
	cut := 0
	if s.cuts > 0 {
		cut, s.cuts = s.cutAfter, s.cuts-1
	}
	s.mu.Unlock()
	if s.etag != "" {
		w.Header().Set("ETag", s.etag)
	}
	start := 0
	if rg := r.Header.Get("Range"); rg != "" && !s.ignoreRange && (r.Header.Get("If-Range") == "" || r.Header.Get("If-Range") == s.etag) {
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
		start = n
		cr := fmt.Sprintf("bytes %d-%d/%d", n, len(s.body)-1, len(s.body))
		if s.badRange {
			cr = fmt.Sprintf("bytes 0-%d/%d", len(s.body)-1, len(s.body))
		}
		w.Header().Set("Content-Range", cr)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)-n))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	out := body[start:]
	if cut > 0 && cut < len(out) {
		_, _ = w.Write(out[:cut])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // drop the connection mid-body
	}
	if s.stallAfter > 0 && s.stallAfter < len(out) {
		_, _ = w.Write(out[:s.stallAfter])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return
	}
	_, _ = w.Write(out)
}

func digestHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestDownloadAndResume(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789abcdef"), 40000) // 640 000 bytes
	spec := func(t *testing.T, dir string, u *url.URL) DownloadSpec {
		return DownloadSpec{URL: u, SHA256: digestHex(body), Size: int64(len(body)), Dir: dir, Timeout: 10 * time.Second, Stall: 2 * time.Second}
	}
	ctx := context.Background()

	t.Run("complete download", func(t *testing.T) {
		s := &snapshotServer{body: body, etag: `"v1"`}
		srv, r := tlsServer(t, s, true)
		dir := t.TempDir()
		p, err := r.Download(ctx, spec(t, dir, mustURL(t, srv.URL+"/snap")))
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); !bytes.Equal(b, body) {
			t.Fatal("content differs")
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Errorf("partial mode %v", fi.Mode())
		}
		// A complete partial is reused without a request.
		n := len(s.requests)
		if _, err := r.Download(ctx, spec(t, dir, mustURL(t, srv.URL+"/snap"))); err != nil || len(s.requests) != n {
			t.Errorf("complete partial: %v, %d requests", err, len(s.requests)-n)
		}
	})

	t.Run("a broken transfer resumes with a verified byte identity", func(t *testing.T) {
		s := &snapshotServer{body: body, etag: `"v1"`, cutAfter: 200000, cuts: 1}
		srv, r := tlsServer(t, s, true)
		dir := t.TempDir()
		_, err := r.Download(ctx, spec(t, dir, mustURL(t, srv.URL+"/snap")))
		if CodeOf(err) != CodeTruncated && CodeOf(err) != CodeNetwork {
			t.Fatalf("cut transfer: %v", err)
		}
		fi, err := os.Stat(PartialPath(dir, digestHex(body)))
		if err != nil || fi.Size() == 0 || fi.Size() >= int64(len(body)) {
			t.Fatalf("partial after the cut: %v %v", fi, err)
		}
		p, err := r.Download(ctx, spec(t, dir, mustURL(t, srv.URL+"/snap")))
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); !bytes.Equal(b, body) {
			t.Fatal("resumed content differs")
		}
		if last := s.requests[len(s.requests)-1]; last != fmt.Sprintf("bytes=%d-|\"v1\"", fi.Size()) {
			t.Errorf("resume request %q", last)
		}
	})

	t.Run("a server ignoring the range sends the whole file again", func(t *testing.T) {
		s := &snapshotServer{body: body, etag: `"v1"`, cutAfter: 100000, cuts: 1, ignoreRange: true}
		srv, r := tlsServer(t, s, true)
		dir := t.TempDir()
		_, _ = r.Download(ctx, spec(t, dir, mustURL(t, srv.URL+"/snap")))
		p, err := r.Download(ctx, spec(t, dir, mustURL(t, srv.URL+"/snap")))
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); !bytes.Equal(b, body) {
			t.Fatal("content differs")
		}
	})

	t.Run("a wrong Content-Range discards the partial file", func(t *testing.T) {
		s := &snapshotServer{body: body, etag: `"v1"`, cutAfter: 100000, cuts: 1, badRange: true}
		srv, r := tlsServer(t, s, true)
		dir := t.TempDir()
		_, _ = r.Download(ctx, spec(t, dir, mustURL(t, srv.URL+"/snap")))
		if _, err := r.Download(ctx, spec(t, dir, mustURL(t, srv.URL+"/snap"))); CodeOf(err) != CodeSizeMismatch {
			t.Fatalf("bad range: %v", err)
		}
		if fi, err := os.Stat(PartialPath(dir, digestHex(body))); err != nil || fi.Size() != 0 {
			t.Fatalf("partial kept: %v %v", fi, err)
		}
	})

	t.Run("corrupt bytes are discarded", func(t *testing.T) {
		s := &snapshotServer{body: body, flip: true}
		srv, r := tlsServer(t, s, true)
		dir := t.TempDir()
		if _, err := r.Download(ctx, spec(t, dir, mustURL(t, srv.URL+"/snap"))); CodeOf(err) != CodeDigestMismatch {
			t.Fatalf("corrupt: %v", err)
		}
		if _, err := os.Stat(PartialPath(dir, digestHex(body))); !os.IsNotExist(err) {
			t.Error("a corrupt download was kept")
		}
	})

	t.Run("more bytes than signed are refused", func(t *testing.T) {
		s := &snapshotServer{body: body, extra: 10}
		srv, r := tlsServer(t, s, true)
		if _, err := r.Download(ctx, spec(t, t.TempDir(), mustURL(t, srv.URL+"/snap"))); CodeOf(err) != CodeSizeMismatch {
			t.Fatalf("declared longer: %v", err)
		}
		// Without a Content-Length, the body is cut at the signed size.
		chunked := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(body)
			w.(http.Flusher).Flush()
			_, _ = w.Write([]byte("more"))
		})
		srv2, r2 := tlsServer(t, chunked, true)
		dir := t.TempDir()
		if _, err := r2.Download(ctx, spec(t, dir, mustURL(t, srv2.URL+"/snap"))); CodeOf(err) != CodeTooLarge {
			t.Fatalf("chunked longer: %v", err)
		}
		if fi, err := os.Stat(PartialPath(dir, digestHex(body))); err != nil || fi.Size() != 0 {
			t.Errorf("oversized partial kept: %v %v", fi, err)
		}
	})

	t.Run("a stalled transfer is aborted and resumed later", func(t *testing.T) {
		s := &snapshotServer{body: body, etag: `"v1"`, stallAfter: 300000}
		srv, r := tlsServer(t, s, true)
		dir := t.TempDir()
		sp := spec(t, dir, mustURL(t, srv.URL+"/snap"))
		sp.Stall = 500 * time.Millisecond
		start := time.Now()
		if _, err := r.Download(ctx, sp); CodeOf(err) != CodeStalled {
			t.Fatalf("stall: %v", err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("stall detected after %s", d)
		}
		s.mu.Lock()
		s.stallAfter = 0
		s.mu.Unlock()
		if _, err := r.Download(ctx, sp); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a slow transfer is bounded by the download timeout", func(t *testing.T) {
		slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			for i := 0; i < len(body); i += 1000 {
				if _, err := w.Write(body[i : i+1000]); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		})
		srv, r := tlsServer(t, slow, true)
		sp := spec(t, t.TempDir(), mustURL(t, srv.URL+"/snap"))
		sp.Timeout = time.Second
		if _, err := r.Download(ctx, sp); CodeOf(err) != CodeTimeout {
			t.Fatalf("timeout: %v", err)
		}
	})

	t.Run("a redirect is refused", func(t *testing.T) {
		srv, r := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, "https://elsewhere.test/snap", http.StatusMovedPermanently)
		}), true)
		if _, err := r.Download(ctx, spec(t, t.TempDir(), mustURL(t, srv.URL+"/snap"))); CodeOf(err) != CodeRedirectRefused {
			t.Fatalf("redirect: %v", err)
		}
	})

	t.Run("a full volume is reported as insufficient storage", func(t *testing.T) {
		if _, err := os.Stat("/dev/full"); err != nil {
			t.Skip("no /dev/full")
		}
		dir := t.TempDir()
		if err := os.Symlink("/dev/full", PartialPath(dir, digestHex(body))); err != nil {
			t.Skip(err)
		}
		srv, r := tlsServer(t, &snapshotServer{body: body}, true)
		_, err := r.Download(ctx, spec(t, dir, mustURL(t, srv.URL+"/snap")))
		if err == nil {
			t.Fatal("a symlinked partial file was followed")
		}
	})
	_ = io.Discard
	_ = filepath.Join
}
