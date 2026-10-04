package bridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/md5" // #nosec G501 -- the distributor's checksum format in the stand-in
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/inbox"
	"github.com/pouriya-sedaghat/karta/internal/online"
	"github.com/pouriya-sedaghat/karta/internal/pbfwrite"
	"github.com/pouriya-sedaghat/karta/internal/provenance"
	"github.com/pouriya-sedaghat/karta/internal/region"
)

const pbfPath = "/asia/iran-latest.osm.pbf"

// geofabrik is the controlled HTTPS stand-in for the distributor.
type geofabrik struct {
	mu         sync.Mutex
	body       []byte
	md5        string // overrides the computed checksum when set
	status     int
	retryAfter string
	location   string
	closeAfter int64
	closeTimes int
	noETag     bool
	// next replaces body after the next complete GET of the snapshot (the
	// file changes during a download).
	next []byte
	hits map[string]int
	gets []http.Header
	srv  *httptest.Server
}

func (g *geofabrik) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.hits[r.Method+" "+r.URL.Path]++
	body, status, ra, loc := g.body, g.status, g.retryAfter, g.location
	closeAfter := int64(0)
	if g.closeAfter > 0 && r.Method == http.MethodGet && r.URL.Path == pbfPath && g.closeTimes > 0 {
		closeAfter = g.closeAfter
		g.closeTimes--
	}
	if r.Method == http.MethodGet && r.URL.Path == pbfPath {
		g.gets = append(g.gets, r.Header.Clone())
	}
	sum := md5.Sum(body) // #nosec G401 -- stand-in checksum
	m := hex.EncodeToString(sum[:])
	if g.md5 != "" {
		m = g.md5
	}
	noETag := g.noETag
	g.mu.Unlock()
	if status != 0 {
		if ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		if loc != "" {
			w.Header().Set("Location", loc)
		}
		w.WriteHeader(status)
		return
	}
	switch r.URL.Path {
	case pbfPath + ".md5":
		_, _ = io.WriteString(w, m+"  iran-latest.osm.pbf\n")
	case pbfPath:
		if !noETag {
			s := sha256.Sum256(body)
			w.Header().Set("ETag", `"`+hex.EncodeToString(s[:8])+`"`)
		}
		var out http.ResponseWriter = w
		if closeAfter > 0 {
			out = &cutWriter{ResponseWriter: w, left: closeAfter}
		}
		http.ServeContent(out, r, "", time.Time{}, bytes.NewReader(body))
		if r.Method == http.MethodGet {
			g.mu.Lock()
			if g.next != nil {
				g.body, g.next = g.next, nil
			}
			g.mu.Unlock()
		}
	default:
		http.NotFound(w, r)
	}
}

type cutWriter struct {
	http.ResponseWriter
	left int64
}

func (c *cutWriter) Write(b []byte) (int, error) {
	if int64(len(b)) > c.left {
		_, _ = c.ResponseWriter.Write(b[:c.left])
		c.left = 0
		if f, ok := c.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	c.left -= int64(len(b))
	return c.ResponseWriter.Write(b)
}

func (g *geofabrik) set(f func(*geofabrik)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f(g)
}

func (g *geofabrik) count(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits[key]
}

// --- fixtures -------------------------------------------------------------------

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixture", "snapshots", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// variantB is fixture B's data with another data timestamp (and optionally
// one more tag, so the bytes differ at the same timestamp).
func variantB(t *testing.T, ts time.Time, extra bool) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "fixture", "karta-fixture-b.osm"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d, err := pbfwrite.ParseXML(f)
	if err != nil {
		t.Fatal(err)
	}
	d.Timestamp = &ts
	if extra {
		d.Nodes[0].Tags = append(d.Nodes[0].Tags, pbfwrite.Tag{K: "note", V: "changed"})
	}
	b, err := pbfwrite.Encode(d, pbfwrite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type brig struct {
	t       *testing.T
	dir     string
	g       *geofabrik
	src     string
	region  string
	spool   string
	publish string
	state   string
	keyFile []string
	keys    []ed25519.PrivateKey
	conf    string
	a       *Acquirer
	last    *Signer
	clock   time.Time
	log     *slog.Logger
}

func writeFile(t *testing.T, p string, b []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, mode); err != nil {
		t.Fatal(err)
	}
}

func newBrig(t *testing.T, body []byte, sourceExtra map[string]any) *brig {
	t.Helper()
	dir := t.TempDir()
	g := &geofabrik{body: body, hits: map[string]int{}}
	g.srv = httptest.NewTLSServer(g)
	t.Cleanup(g.srv.Close)
	ca := filepath.Join(dir, "ca.pem")
	writeFile(t, ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: g.srv.Certificate().Raw}), 0o644)
	b := &brig{t: t, dir: dir, g: g, src: filepath.Join(dir, "source.json"), region: filepath.Join("..", "..", "config", "regions", "fixture.json"),
		spool: filepath.Join(dir, "spool"), publish: filepath.Join(dir, "publish"), state: filepath.Join(dir, "signer"),
		conf: filepath.Join(dir, "signer.json"), clock: time.Now().UTC(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	src := map[string]any{"region_id": "fixture", "snapshot_url": g.srv.URL + pbfPath, "md5_url": g.srv.URL + pbfPath + ".md5",
		"distributor": "Geofabrik stand-in (test only)", "user_agent": "karta-bridge-test (ops@example.invalid)",
		"allowed_networks": []string{"127.0.0.0/8"}, "ca_file": ca, "poll_interval": "1h", "reverify_interval": "168h",
		"download_timeout": "30s", "stall_timeout": "5s", "retry_initial": "1m", "retry_max": "1h"}
	for k, v := range sourceExtra {
		if v == nil {
			delete(src, k)
		} else {
			src[k] = v
		}
	}
	js, _ := json.Marshal(src)
	writeFile(t, b.src, js, 0o644)
	if err := os.MkdirAll(b.spool, 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := NewAcquirer(AcquireConfig{SourcePath: b.src, Spool: b.spool, MaxInputBytes: 64 << 20, Version: "test"}, b.log)
	if err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return b.clock }
	b.a = a
	b.addKey("bridge-test-a")
	b.writeSignerConf(nil)
	return b
}

func (b *brig) addKey(id string) {
	b.t.Helper()
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	p := filepath.Join(b.dir, "keys", id+".pem")
	writeFile(b.t, p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
	b.keyFile = append(b.keyFile, id+"="+p)
	b.keys = append(b.keys, k)
}

func (b *brig) writeSignerConf(extra map[string]any) {
	b.t.Helper()
	var keys []map[string]string
	for _, kf := range b.keyFile {
		id, p, _ := strings.Cut(kf, "=")
		keys = append(keys, map[string]string{"id": id, "file": p})
	}
	c := map[string]any{"bridge_id": "bridge-test", "keys": keys, "manifest_validity": "168h"}
	for k, v := range extra {
		c[k] = v
	}
	js, _ := json.Marshal(c)
	writeFile(b.t, b.conf, js, 0o644)
}

func (b *brig) signer(raise *Raise) (*Signer, error) {
	// One signer at a time (the state directory's lock), as in a deployment.
	if b.last != nil {
		_ = b.last.Close()
		b.last = nil
	}
	s, err := NewSigner(SignConfig{ConfigPath: b.conf, RegionPath: b.region, Spool: b.spool, Publish: b.publish, StateDir: b.state,
		MaxInputBytes: 64 << 20, MaxFutureSkew: 10 * time.Minute, Poll: time.Second, Version: "test"}, b.log, raise)
	if s != nil {
		s.now = func() time.Time { return b.clock }
		b.last = s
	}
	return s, err
}

func (b *brig) mustSigner() *Signer {
	b.t.Helper()
	s, err := b.signer(nil)
	if err != nil {
		b.t.Fatal(err)
	}
	return s
}

func (b *brig) manifest() online.Manifest {
	b.t.Helper()
	raw, err := os.ReadFile(filepath.Join(b.publish, ManifestFile))
	if err != nil {
		b.t.Fatal(err)
	}
	m, err := online.ParseUnverified(raw)
	if err != nil {
		b.t.Fatal(err)
	}
	return *m
}

func (b *brig) spooled() []string {
	b.t.Helper()
	es, err := inbox.Scan(b.spool, 100)
	if err != nil {
		b.t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if e.Complete() {
			out = append(out, e.Name)
		}
	}
	return out
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// sourceFor is a fetcher source file trusting keys, for the bridge's serve.
func sourceFor(t *testing.T, manifestURL, ca string, keys map[string]ed25519.PublicKey) *online.Source {
	t.Helper()
	var tk []map[string]string
	for id, k := range keys {
		tk = append(tk, map[string]string{"id": id, "ed25519_public_key": base64.StdEncoding.EncodeToString(k)})
	}
	js, _ := json.Marshal(map[string]any{"region_id": "fixture", "manifest_url": manifestURL, "allowed_networks": []string{"127.0.0.0/8"},
		"ca_file": ca, "trusted_keys": tk, "poll_interval": "1h", "max_manifest_validity": "192h"})
	s, err := online.ParseSource(js)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// --- tests ----------------------------------------------------------------------

// The whole online leg: the stand-in serves fixture A; acquire downloads it
// once, sign verifies and signs it, serve publishes it, and Karta's own
// fetcher accepts the manifest and delivers the exact bytes.
func TestBridgeEndToEnd(t *testing.T) {
	a := fixture(t, "karta-fixture-a.osm.pbf")
	b := newBrig(t, a, nil)
	ctx := context.Background()
	if err := b.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := b.spooled(); len(got) != 1 || got[0] != sha(a) {
		t.Fatalf("spool %v", got)
	}
	st := b.a.State()
	if st.Last == nil || st.Last.MD5Matched == nil || !*st.Last.MD5Matched || st.Last.SizeBytes != int64(len(a)) {
		t.Fatalf("acquisition %+v", st.Last)
	}
	s := b.mustSigner()
	s.RunOnce(ctx)
	if s.State().LastError != nil {
		t.Fatalf("sign: %+v", s.State().LastError)
	}
	m := b.manifest()
	if m.Serial != 1 || m.Snapshot.SHA256 != sha(a) || m.Snapshot.URL != "snapshots/"+sha(a)+".osm.pbf" || m.Provenance == nil ||
		!m.Snapshot.DataTimestamp.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("manifest %+v", m)
	}
	// The provenance sidecar passes Karta's checks and records what the
	// bridge observed, nothing it did not.
	side, err := os.ReadFile(filepath.Join(b.publish, SnapshotsDir, sha(a)+".osm.pbf.provenance.json"))
	if err != nil {
		t.Fatal(err)
	}
	sc, err := provenance.Parse(side)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sc.Verify(sha(a), int64(len(a))); err != nil {
		t.Fatalf("sidecar does not verify: %v", err)
	}
	if !strings.Contains(string(side), `"karta-bridge-provenance/1"`) || !strings.Contains(string(side), "not the distributor's signature") {
		t.Errorf("sidecar %s", side)
	}
	// Serve it over TLS and fetch it with Karta's fetcher.
	srv := httptest.NewTLSServer(Handler(b.publish, b.log))
	defer srv.Close()
	ca := filepath.Join(b.dir, "serve-ca.pem")
	writeFile(t, ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644)
	src := sourceFor(t, srv.URL+"/manifest.json", ca, s.PublicKeys())
	cfg, err := region.Load(b.region)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(b.publish, ManifestFile))
	if _, err := online.Verify(raw, online.VerifyOptions{Source: src, Region: cfg, Now: time.Now(), Skew: time.Minute}); err != nil {
		t.Fatalf("Karta refuses the bridge's manifest: %v", err)
	}
	srcPath := filepath.Join(b.dir, "fetcher-source.json")
	js, _ := json.Marshal(map[string]any{"region_id": "fixture", "manifest_url": srv.URL + "/manifest.json", "allowed_networks": []string{"127.0.0.0/8"},
		"ca_file": ca, "trusted_keys": []map[string]string{{"id": "bridge-test-a",
			"ed25519_public_key": base64.StdEncoding.EncodeToString(b.keys[0].Public().(ed25519.PublicKey))}},
		"poll_interval": "1h", "max_manifest_validity": "192h"})
	writeFile(t, srcPath, js, 0o644)
	outbox := filepath.Join(b.dir, "outbox")
	_ = os.Mkdir(outbox, 0o755)
	f, err := online.NewFetcher(online.FetcherConfig{SourcePath: srcPath, RegionPath: b.region, Dir: outbox, MaxInputBytes: 64 << 20,
		MaxFutureSkew: time.Minute, Version: "test"}, b.log)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.CheckOnce(ctx); err != nil {
		t.Fatalf("the fetcher refuses the bridge: %v", err)
	}
	name := online.DeliveryName("fixture", 1, sha(a))
	got, err := os.ReadFile(filepath.Join(outbox, name+".osm.pbf"))
	if err != nil || !bytes.Equal(got, a) {
		t.Fatalf("delivered bytes: %v", err)
	}
	if b.g.count("GET "+pbfPath) != 1 {
		t.Errorf("the distributor served the snapshot %d times", b.g.count("GET "+pbfPath))
	}
}

func TestAcquireAvoidsRedundantDownloads(t *testing.T) {
	a := fixture(t, "karta-fixture-a.osm.pbf")
	b := newBrig(t, a, nil)
	ctx := context.Background()
	if err := b.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// Unchanged hints: no download.
	for i := 0; i < 3; i++ {
		if err := b.a.CheckOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := b.g.count("GET " + pbfPath); n != 1 {
		t.Fatalf("%d downloads of unchanged bytes", n)
	}
	// The bounded re-verification downloads the bytes again even though no
	// hint changed, and finds the same bytes: nothing new is spooled.
	b.clock = b.clock.Add(169 * time.Hour)
	if err := b.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := b.g.count("GET " + pbfPath); n != 2 || len(b.spooled()) != 1 || !b.a.State().Last.VerifiedAt.Equal(b.clock) {
		t.Fatalf("re-verification: %d downloads, spool %v, %+v", n, b.spooled(), b.a.State().Last)
	}
	// Without any hint but the size, a change of the same length is found
	// by the re-verification schedule.
	b.g.set(func(g *geofabrik) { g.noETag = true })
	newer := variantB(t, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), false)
	b.g.set(func(g *geofabrik) { g.body = newer })
	if err := b.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := b.spooled(); len(got) != 2 {
		t.Fatalf("changed bytes not spooled: %v", got)
	}
}

func TestAcquireRefusesChangedOrInconsistentTransfers(t *testing.T) {
	a := fixture(t, "karta-fixture-a.osm.pbf")
	ctx := context.Background()
	t.Run("iran-latest replaced during the download", func(t *testing.T) {
		b := newBrig(t, a, nil)
		b.g.set(func(g *geofabrik) { g.next = fixture(t, "karta-fixture-b.osm.pbf") })
		err := b.a.CheckOnce(ctx)
		if online.CodeOf(err) != CodeSourceChanged || len(b.spooled()) != 0 {
			t.Fatalf("%v spool %v", err, b.spooled())
		}
		// The next check takes the new file whole.
		if err := b.a.CheckOnce(ctx); err != nil || len(b.spooled()) != 1 || b.spooled()[0] != sha(fixture(t, "karta-fixture-b.osm.pbf")) {
			t.Fatalf("%v %v", err, b.spooled())
		}
	})
	t.Run("the bytes do not match the distributor's MD5", func(t *testing.T) {
		b := newBrig(t, a, nil)
		b.g.set(func(g *geofabrik) { g.md5 = strings.Repeat("0", 32) })
		if err := b.a.CheckOnce(ctx); online.CodeOf(err) != CodeChecksumMismatch || len(b.spooled()) != 0 {
			t.Fatalf("%v", err)
		}
	})
	t.Run("an interrupted transfer resumes with If-Range", func(t *testing.T) {
		b := newBrig(t, a, nil)
		b.g.set(func(g *geofabrik) { g.closeAfter, g.closeTimes = int64(len(a)/2), 1 })
		if err := b.a.CheckOnce(ctx); err == nil {
			t.Fatal("the cut transfer succeeded")
		}
		if d := b.a.State().Download; d == nil || d.Bytes == 0 {
			t.Fatalf("no partial download recorded: %+v", d)
		}
		if err := b.a.CheckOnce(ctx); err != nil {
			t.Fatal(err)
		}
		b.g.mu.Lock()
		last := b.g.gets[len(b.g.gets)-1]
		b.g.mu.Unlock()
		if !strings.HasPrefix(last.Get("Range"), "bytes=") || last.Get("If-Range") == "" {
			t.Errorf("the resumed request had Range %q If-Range %q", last.Get("Range"), last.Get("If-Range"))
		}
		if got := b.spooled(); len(got) != 1 || got[0] != sha(a) {
			t.Fatalf("resumed download: %v", got)
		}
	})
}

func TestAcquireBounds(t *testing.T) {
	a := fixture(t, "karta-fixture-a.osm.pbf")
	ctx := context.Background()
	for name, c := range map[string]struct {
		setup func(*brig)
		code  string
	}{
		"429 with Retry-After": {func(b *brig) { b.g.set(func(g *geofabrik) { g.status, g.retryAfter = 429, "7200" }) }, CodeRateLimited},
		"503":                  {func(b *brig) { b.g.set(func(g *geofabrik) { g.status = 503 }) }, CodeRateLimited},
		"missing":              {func(b *brig) { b.g.set(func(g *geofabrik) { g.status = 404 }) }, online.CodeHTTPStatus},
		"redirect": {func(b *brig) {
			b.g.set(func(g *geofabrik) { g.status, g.location = 302, "https://elsewhere.example/x" })
		}, online.CodeRedirectRefused},
		"oversized": {func(b *brig) {
			b.a.cfg.MaxInputBytes = int64(len(a) - 1)
		}, online.CodeTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBrig(t, a, nil)
			c.setup(b)
			b.a.RunOnce(ctx)
			st := b.a.State()
			if st.LastError == nil || st.LastError.Code != c.code || len(b.spooled()) != 0 {
				t.Fatalf("%+v", st.LastError)
			}
			if name == "429 with Retry-After" && st.NextAttemptAt.Before(b.clock.Add(2*time.Hour)) {
				t.Errorf("Retry-After not honored: next %s", st.NextAttemptAt)
			}
		})
	}
	t.Run("untrusted TLS and private destinations", func(t *testing.T) {
		b := newBrig(t, a, map[string]any{"ca_file": nil})
		if err := b.a.CheckOnce(ctx); online.CodeOf(err) != online.CodeTLS {
			t.Fatalf("system roots trusted the stand-in: %v", err)
		}
		b = newBrig(t, a, map[string]any{"allowed_networks": nil})
		if err := b.a.CheckOnce(ctx); online.CodeOf(err) != online.CodeDestinationRefused {
			t.Fatalf("loopback was connected to: %v", err)
		}
	})
	t.Run("an invalid source file", func(t *testing.T) {
		for name, extra := range map[string]map[string]any{
			"http":            {"snapshot_url": "http://download.example/asia/iran-latest.osm.pbf"},
			"query":           {"snapshot_url": "https://download.example/asia/iran-latest.osm.pbf?x=1"},
			"other md5 host":  {"md5_url": "https://other.example/asia/iran-latest.osm.pbf.md5"},
			"no user agent":   {"user_agent": nil},
			"no reverify":     {"reverify_interval": nil},
			"wide network":    {"allowed_networks": []string{"0.0.0.0/0"}},
			"not a .osm.pbf":  {"snapshot_url": "https://download.example/asia/iran.html"},
			"unknown setting": {"follow_redirects": true},
		} {
			dir := t.TempDir()
			src := map[string]any{"region_id": "fixture", "snapshot_url": "https://download.example/asia/iran-latest.osm.pbf",
				"distributor": "x", "user_agent": "karta-bridge-test (ops@example.invalid)", "poll_interval": "1h", "reverify_interval": "168h"}
			for k, v := range extra {
				if v == nil {
					delete(src, k)
				} else {
					src[k] = v
				}
			}
			js, _ := json.Marshal(src)
			p := filepath.Join(dir, "s.json")
			writeFile(t, p, js, 0o644)
			if _, err := LoadSource(p); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	})
}

func TestSignerHoldsWhatItMustNotSign(t *testing.T) {
	a := fixture(t, "karta-fixture-a.osm.pbf")
	ctx := context.Background()
	b := newBrig(t, fixture(t, "karta-fixture-b.osm.pbf"), nil)
	if err := b.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s := b.mustSigner()
	s.RunOnce(ctx)
	if s.State().HighWater != 1 {
		t.Fatalf("%+v", s.State())
	}
	// Older data (fixture A, 2026-01-01) and different bytes at the same
	// timestamp (2026-02-01) are held, not signed.
	for _, body := range [][]byte{a, variantB(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), true)} {
		b.g.set(func(g *geofabrik) { g.body = body })
		if err := b.a.CheckOnce(ctx); err != nil {
			t.Fatal(err)
		}
		s.RunOnce(ctx)
		st := s.State()
		if st.HighWater != 1 || st.Held == nil || st.Held.Code != CodeNotNewer || st.Held.SHA256 != sha(body) {
			t.Fatalf("not held: %+v", st)
		}
	}
	// Structurally invalid bytes are held by Karta's own checks.
	bad := append([]byte(nil), a...)
	bad = bad[:len(bad)-7]
	b.g.set(func(g *geofabrik) { g.body = bad })
	if err := b.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s.RunOnce(ctx)
	if st := s.State(); st.HighWater != 1 || st.Held == nil || st.Held.SHA256 != sha(bad) || st.Held.Code != "malformed_snapshot" {
		t.Fatalf("invalid bytes: %+v", st.Held)
	}
	// Newer data is signed (and clears the hold).
	newer := variantB(t, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), false)
	b.g.set(func(g *geofabrik) { g.body = newer })
	if err := b.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s.RunOnce(ctx)
	if st := s.State(); st.HighWater != 2 || st.Held != nil || b.manifest().Snapshot.SHA256 != sha(newer) {
		t.Fatalf("newer: %+v", st)
	}
}

// Serials come from durable high-water state, bound to the exact envelope
// before it is visible; a lost or older state fails closed until an
// operator raises the high-water mark; a clock behind the last issue time
// fails closed; several issues in one second are consecutive serials.
func TestSignerSerialRecovery(t *testing.T) {
	ctx := context.Background()
	b := newBrig(t, fixture(t, "karta-fixture-a.osm.pbf"), nil)
	if err := b.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s := b.mustSigner()
	s.RunOnce(ctx)
	stateFile := filepath.Join(b.state, stateFileName)
	old, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	// Renewals in the same second: consecutive serials, distinct envelopes.
	var envs []string
	for i := 0; i < 3; i++ {
		c := s.State().Current
		if err := s.sign(c.SHA256, c.SizeBytes, c.DataTimestamp, sha(mustRead(t, filepath.Join(b.publish, SnapshotsDir, c.SHA256+".osm.pbf.provenance.json"))),
			int64(len(mustRead(t, filepath.Join(b.publish, SnapshotsDir, c.SHA256+".osm.pbf.provenance.json")))), "renewal"); err != nil {
			t.Fatal(err)
		}
		envs = append(envs, s.State().Current.EnvelopeSHA256)
	}
	if s.State().HighWater != 4 || b.manifest().Serial != 4 || envs[0] == envs[1] || envs[1] == envs[2] {
		t.Fatalf("serials %d %d envelopes %v", s.State().HighWater, b.manifest().Serial, envs)
	}
	// A crash after the envelope was persisted but before it was published:
	// the same serial is published with the same bytes.
	st := s.State()
	cur := *st.Current
	cur.Serial, cur.Promoted = 5, false
	m, _ := online.ParseUnverified([]byte(cur.Envelope))
	m.Serial = 5
	env, err := online.Sign(*m, online.Signer{KeyID: "bridge-test-a", Key: b.keys[0]})
	if err != nil {
		t.Fatal(err)
	}
	cur.Envelope = string(env) + "\n"
	st.Current, st.HighWater = &cur, 5
	if err := writeJSONState(stateFile, st); err != nil {
		t.Fatal(err)
	}
	if s = b.mustSigner(); !s.State().Current.Promoted {
		t.Fatal("the recovered envelope is not marked published")
	}
	if got, _ := os.ReadFile(filepath.Join(b.publish, ManifestFile)); string(got) != cur.Envelope || b.manifest().Serial != 5 {
		t.Fatalf("the pending envelope was not published as it was: serial %d", b.manifest().Serial)
	}
	// The state restored from an older backup (serial 1) is below the
	// published manifest (serial 5): fail closed.
	writeFile(t, stateFile, old, 0o600)
	if _, err := b.signer(nil); !errors.Is(err, ErrFailClosed) || !strings.Contains(err.Error(), CodeStateBehind) {
		t.Fatalf("an older state started: %v", err)
	}
	if _, err := b.signer(&Raise{To: 4, Reason: "too low"}); err == nil {
		t.Fatal("a raise below the published serial was accepted")
	}
	if _, err := b.signer(&Raise{To: 7}); err == nil {
		t.Fatal("a raise without a reason was accepted")
	}
	s, err = b.signer(&Raise{To: 7, Reason: "Karta's operator status shows verified serial 7"})
	if err != nil {
		t.Fatal(err)
	}
	if s.State().HighWater != 7 || len(s.State().Raises) != 1 {
		t.Fatalf("%+v", s.State())
	}
	// The state lost entirely: fail closed too.
	_ = os.Remove(stateFile)
	if _, err := b.signer(nil); !errors.Is(err, ErrFailClosed) {
		t.Fatalf("a lost state started: %v", err)
	}
	if _, err = b.signer(&Raise{To: 9, Reason: "state lost; Karta verified 8"}); err != nil {
		t.Fatal(err)
	}
	// A clock behind the last issue: no signature.
	s = b.mustSigner()
	s.RunOnce(ctx)
	hw := s.State().HighWater
	b.clock = b.clock.Add(-24 * time.Hour)
	s.state.Current = &Signed{Serial: hw, SHA256: cur.SHA256, SizeBytes: cur.SizeBytes, DataTimestamp: cur.DataTimestamp,
		IssuedAt: b.clock.Add(24 * time.Hour), ExpiresAt: b.clock.Add(24*time.Hour + time.Minute), Promoted: true}
	s.RunOnce(ctx)
	if s.State().HighWater != hw || s.State().LastError == nil || s.State().LastError.Code != CodeClockBehind {
		t.Fatalf("signed with a clock behind: %+v", s.State().LastError)
	}
	// Serial exhaustion: never beyond the fetcher's 12-digit names.
	b.clock = b.clock.Add(48 * time.Hour)
	s.state.HighWater = MaxSerial
	if err := s.sign(cur.SHA256, cur.SizeBytes, cur.DataTimestamp, strings.Repeat("0", 64), 1, "renewal"); online.CodeOf(err) != CodeSerialExhausted {
		t.Fatalf("beyond the serial range: %v", err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Before expiry the held snapshot is re-signed under a new serial, without
// the distributor: renewal needs no network. The data timestamp does not
// change. Assets no unexpired manifest names are removed.
func TestSignerRenewsWithoutTheDistributor(t *testing.T) {
	ctx := context.Background()
	b := newBrig(t, fixture(t, "karta-fixture-a.osm.pbf"), nil)
	b.writeSignerConf(map[string]any{"manifest_validity": "2h", "renew_before": "1h"})
	if err := b.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s := b.mustSigner()
	s.RunOnce(ctx)
	first := b.manifest()
	b.g.srv.Close() // the distributor is gone
	b.clock = b.clock.Add(61 * time.Minute)
	s.RunOnce(ctx)
	m := b.manifest()
	if m.Serial != first.Serial+1 || m.Snapshot.SHA256 != first.Snapshot.SHA256 || !m.Snapshot.DataTimestamp.Equal(first.Snapshot.DataTimestamp) ||
		!m.ExpiresAt.After(first.ExpiresAt) {
		t.Fatalf("renewal %+v after %+v", m, first)
	}
	if s.State().LastError != nil {
		t.Fatalf("%+v", s.State().LastError)
	}
	// A missing asset is not renewed (fail closed), never re-signed blind.
	_ = os.Remove(filepath.Join(b.publish, SnapshotsDir, m.Snapshot.SHA256+".osm.pbf"))
	b.clock = b.clock.Add(61 * time.Minute)
	s.RunOnce(ctx)
	if s.State().LastError == nil || s.State().LastError.Code != CodeAssetMissing || b.manifest().Serial != m.Serial {
		t.Fatalf("renewed a missing asset: %+v", s.State().LastError)
	}
}

func TestSignerSignsWithEveryKeyDuringARotation(t *testing.T) {
	ctx := context.Background()
	b := newBrig(t, fixture(t, "karta-fixture-a.osm.pbf"), nil)
	b.addKey("bridge-test-b")
	b.writeSignerConf(nil)
	if err := b.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s := b.mustSigner()
	s.RunOnce(ctx)
	raw, _ := os.ReadFile(filepath.Join(b.publish, ManifestFile))
	cfg, _ := region.Load(b.region)
	for id, k := range s.PublicKeys() {
		src := sourceFor(t, "https://bridge.example/manifest.json", "/dev/null", map[string]ed25519.PublicKey{id: k})
		if _, err := online.Verify(raw, online.VerifyOptions{Source: src, Region: cfg, Now: time.Now(), Skew: time.Minute}); err != nil {
			t.Errorf("a deployment trusting only %s refuses the manifest: %v", id, err)
		}
	}
	var env online.Envelope
	_ = json.Unmarshal(raw, &env)
	if len(env.Signatures) != 2 {
		t.Fatalf("%d signatures", len(env.Signatures))
	}
}

func TestSignerRefusesAMissingKey(t *testing.T) {
	b := newBrig(t, fixture(t, "karta-fixture-a.osm.pbf"), nil)
	_, p, _ := strings.Cut(b.keyFile[0], "=")
	_ = os.Remove(p)
	if _, err := b.signer(nil); err == nil {
		t.Fatal("started without its key")
	}
}

func TestServe(t *testing.T) {
	ctx := context.Background()
	b := newBrig(t, fixture(t, "karta-fixture-a.osm.pbf"), nil)
	if err := b.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s := b.mustSigner()
	s.RunOnce(ctx)
	h := Handler(b.publish, b.log)
	do := func(method, path string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	asset := "/" + SnapshotsDir + "/" + sha(fixture(t, "karta-fixture-a.osm.pbf")) + ".osm.pbf"
	if r := do("GET", "/manifest.json", nil); r.Code != 200 || r.Header().Get("Cache-Control") != "no-cache" || r.Header().Get("ETag") == "" {
		t.Errorf("manifest: %d %v", r.Code, r.Header())
	}
	r := do("GET", asset, map[string]string{"Range": "bytes=10-"})
	etag := r.Header().Get("ETag")
	if r.Code != 206 || !strings.HasPrefix(etag, `"sha256:`) {
		t.Errorf("range: %d %q", r.Code, etag)
	}
	if r := do("GET", asset, map[string]string{"Range": "bytes=10-", "If-Range": etag}); r.Code != 206 {
		t.Errorf("If-Range with the asset's ETag: %d", r.Code)
	}
	if r := do("GET", asset, map[string]string{"Range": "bytes=10-", "If-Range": `"other"`}); r.Code != 200 {
		t.Errorf("If-Range with another ETag: %d", r.Code)
	}
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"POST", "/manifest.json", 405},
		{"GET", "/manifest.json?x=1", 400},
		{"GET", "/.status/signer.json", 404},
		{"GET", "/snapshots/../manifest.json", 404},
		{"GET", "/" + SnapshotsDir + "/" + strings.Repeat("0", 64) + ".osm.pbf", 404},
		{"GET", "/" + stagingDirName + "/x", 404},
	} {
		if r := do(c.method, c.path, nil); r.Code != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, r.Code, c.want)
		}
	}
	if !strings.Contains(string(Metrics(b.spool, b.publish, time.Now())), "karta_bridge_manifest_serial 1") {
		t.Errorf("metrics: %s", Metrics(b.spool, b.publish, time.Now()))
	}
}

// The committed examples stay valid (they are what deployments copy).
func TestExampleConfigs(t *testing.T) {
	s, err := LoadSource(filepath.Join("..", "..", "config", "bridge", "geofabrik-iran.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.PollInterval.D() < time.Hour || s.MD5URL == "" {
		t.Errorf("the example polls more often than hourly or has no md5: %+v", s)
	}
	c, err := LoadSignerConfig(filepath.Join("..", "..", "config", "bridge", "signer.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Within the 192h a fetcher example accepts, and renewed at half.
	if c.ManifestValidity.D() > 192*time.Hour || c.RenewBefore.D() != c.ManifestValidity.D()/2 {
		t.Errorf("%+v", c)
	}
}

// spoolNewer makes the stand-in serve a newer variant and acquires it.
func spoolNewer(t *testing.T, b *brig, ts time.Time) []byte {
	t.Helper()
	nb := variantB(t, ts, false)
	b.g.set(func(g *geofabrik) { g.body = nb })
	b.clock = b.clock.Add(time.Minute)
	if err := b.a.CheckOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	return nb
}

// blockManifestWrite makes the next atomic write of manifest.json fail
// (its temporary name is taken by a directory), as a full disk would.
func blockManifestWrite(t *testing.T, b *brig) func() {
	t.Helper()
	d := filepath.Join(b.publish, ".tmp-"+ManifestFile)
	if err := os.MkdirAll(filepath.Join(d, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.RemoveAll(d) }
}

// A manifest whose publication failed is published at the next run, not
// only at the next start, and is not reported as published meanwhile.
func TestSignerRetriesAFailedPublication(t *testing.T) {
	b := newBrig(t, fixture(t, "karta-fixture-a.osm.pbf"), nil)
	ctx := context.Background()
	if err := b.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s := b.mustSigner()
	s.RunOnce(ctx)
	nb := spoolNewer(t, b, time.Now().UTC().Add(-time.Hour))
	unblock := blockManifestWrite(t, b)
	s.RunOnce(ctx)
	if st := s.State(); st.Current.Promoted || st.LastError == nil || b.manifest().Serial != 1 {
		t.Fatalf("a failed publication: promoted %v, error %+v, published %d", st.Current.Promoted, st.LastError, b.manifest().Serial)
	}
	if m := string(Metrics(b.spool, b.publish, b.clock)); !strings.Contains(m, "karta_bridge_manifest_pending 1") ||
		strings.Contains(m, "karta_bridge_manifest_serial 2") {
		t.Errorf("metrics report an unpublished manifest as published:\n%s", m)
	}
	unblock()
	s.RunOnce(ctx)
	if m := b.manifest(); m.Serial != 2 || m.Snapshot.SHA256 != sha(nb) || s.State().LastError != nil {
		t.Fatalf("not retried: published %d %s, error %+v", m.Serial, m.Snapshot.SHA256, s.State().LastError)
	}
}

// An acquisition that keeps failing (here: its staging name is taken, as
// with no space) does not stop the current manifest's renewal.
func TestSignerRenewsDespiteAFailingAcquisition(t *testing.T) {
	b := newBrig(t, fixture(t, "karta-fixture-a.osm.pbf"), nil)
	ctx := context.Background()
	if err := b.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s := b.mustSigner()
	s.RunOnce(ctx)
	first := b.manifest()
	nb := spoolNewer(t, b, time.Now().UTC().Add(-time.Hour))
	if err := os.WriteFile(filepath.Join(b.publish, stagingDirName, "sign-"+sha(nb)[:16]), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.clock = first.ExpiresAt.Add(-time.Hour) // renewal due
	s.RunOnce(ctx)
	m := b.manifest()
	if m.Serial <= first.Serial || !m.ExpiresAt.After(first.ExpiresAt) || m.Snapshot.SHA256 != first.Snapshot.SHA256 {
		t.Fatalf("not renewed: %+v after %+v", m, first)
	}
	if s.State().LastError == nil {
		t.Error("the failing acquisition is not reported")
	}
}

// Restoring an older state whose current envelope was never published,
// then raising the high-water serial, never publishes that older envelope
// over the newer published manifest: the published one becomes current.
func TestSignerRaiseNeverLowersThePublishedSerial(t *testing.T) {
	b := newBrig(t, fixture(t, "karta-fixture-a.osm.pbf"), nil)
	ctx := context.Background()
	if err := b.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s := b.mustSigner()
	s.RunOnce(ctx) // serial 1
	spoolNewer(t, b, time.Now().UTC().Add(-2*time.Hour))
	unblock := blockManifestWrite(t, b)
	s.RunOnce(ctx) // serial 2 persisted, not published
	unblock()
	backup, err := os.ReadFile(filepath.Join(b.state, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	s = b.mustSigner() // publishes serial 2
	n3 := spoolNewer(t, b, time.Now().UTC().Add(-time.Hour))
	s.RunOnce(ctx) // serial 3
	if m := b.manifest(); m.Serial != 3 {
		t.Fatalf("setup: published serial %d", m.Serial)
	}
	if err := os.WriteFile(filepath.Join(b.state, stateFileName), backup, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.signer(nil); !errors.Is(err, ErrFailClosed) {
		t.Fatalf("the older state started: %v", err)
	}
	s, err = b.signer(&Raise{To: 3, Reason: "restored from backup"})
	if err != nil {
		t.Fatal(err)
	}
	if m := b.manifest(); m.Serial != 3 || m.Snapshot.SHA256 != sha(n3) {
		t.Fatalf("the published manifest went back to serial %d", m.Serial)
	}
	if c := s.State().Current; c == nil || c.Serial != 3 || c.SHA256 != sha(n3) || !c.Promoted {
		t.Fatalf("current %+v, want the published serial 3", c)
	}
	// Renewal continues above the published serial, with the published data.
	b.clock = b.manifest().ExpiresAt.Add(-time.Hour)
	s.RunOnce(ctx)
	if m := b.manifest(); m.Serial != 4 || m.Snapshot.SHA256 != sha(n3) {
		t.Fatalf("after the raise: serial %d %s", m.Serial, m.Snapshot.SHA256)
	}
}

// One signer per state directory: a second one (or a raise while the
// signer runs) is refused instead of overwriting the first one's serials.
func TestSignerSingleInstance(t *testing.T) {
	b := newBrig(t, fixture(t, "karta-fixture-a.osm.pbf"), nil)
	s1 := b.mustSigner()
	open := func(raise *Raise) (*Signer, error) {
		return NewSigner(SignConfig{ConfigPath: b.conf, RegionPath: b.region, Spool: b.spool, Publish: b.publish, StateDir: b.state,
			MaxInputBytes: 64 << 20, MaxFutureSkew: 10 * time.Minute, Poll: time.Second, Version: "test"}, b.log, raise)
	}
	if _, err := open(nil); err == nil {
		t.Fatal("a second signer started")
	}
	if _, err := open(&Raise{To: 50, Reason: "Karta verified 49"}); err == nil {
		t.Fatal("a raise ran while the signer runs")
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := open(&Raise{To: 50, Reason: "Karta verified 49"})
	if err != nil {
		t.Fatalf("after the first signer stopped: %v", err)
	}
	_ = s2.Close()
	if _, err := NewAcquirer(AcquireConfig{SourcePath: b.src, Spool: b.spool, MaxInputBytes: 1 << 20, Version: "test"}, b.log); err == nil {
		t.Error("a second downloader opened the spool")
	}
}

func TestServeBounds(t *testing.T) {
	t.Run("concurrency", func(t *testing.T) {
		release := make(chan struct{})
		entered := make(chan struct{}, 2)
		h := limitConcurrency(1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-release
		}))
		done := make(chan struct{})
		go func() {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/manifest.json", nil))
			close(done)
		}()
		<-entered
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/manifest.json", nil))
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
			t.Errorf("beyond the limit: %d %v", rec.Code, rec.Header())
		}
		close(release)
		<-done
	})
	t.Run("a stalled download is abandoned", func(t *testing.T) {
		old := writeStall
		writeStall = 300 * time.Millisecond
		defer func() { writeStall = old }()
		dir := t.TempDir()
		name := strings.Repeat("ab", 32) + ".osm.pbf"
		if err := os.MkdirAll(filepath.Join(dir, SnapshotsDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, SnapshotsDir, name), make([]byte, 64<<20), 0o644); err != nil {
			t.Fatal(err)
		}
		finished := make(chan struct{})
		inner := Handler(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inner.ServeHTTP(w, r)
			close(finished)
		}))
		defer srv.Close()
		resp, err := http.Get(srv.URL + "/" + SnapshotsDir + "/" + name) // never read
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatal("the handler is still writing to a client that reads nothing")
		}
	})
}

// writePublished replaces the published manifest with m signed by signers.
func writePublished(t *testing.T, b *brig, m online.Manifest, signers ...online.Signer) []byte {
	t.Helper()
	env, err := online.Sign(m, signers...)
	if err != nil {
		t.Fatal(err)
	}
	env = append(env, '\n')
	if err := os.WriteFile(filepath.Join(b.publish, ManifestFile), env, 0o644); err != nil {
		t.Fatal(err)
	}
	return env
}

// A published manifest the signer cannot prove it made is never adopted,
// whatever its serial: a foreign key, a signature that claims the signer's
// key id but does not verify, or the signer's key over another region.
func TestSignerRefusesToAdoptAnUnverifiedPublication(t *testing.T) {
	ctx := context.Background()
	_, foreign, _ := ed25519.GenerateKey(rand.Reader)
	for name, c := range map[string]func(m *online.Manifest, own online.Signer) []online.Signer{
		"a foreign key": func(_ *online.Manifest, _ online.Signer) []online.Signer {
			return []online.Signer{{KeyID: "intruder", Key: foreign}}
		},
		"a forged signature with the signer's key id": func(_ *online.Manifest, _ online.Signer) []online.Signer {
			return []online.Signer{{KeyID: "bridge-test-a", Key: foreign}}
		},
		"the signer's key over another region": func(m *online.Manifest, own online.Signer) []online.Signer {
			m.RegionID = "elsewhere"
			return []online.Signer{own}
		},
		"the signer's key over another box": func(m *online.Manifest, own online.Signer) []online.Signer {
			m.BBox = online.BBox{0, 0, 1, 1}
			return []online.Signer{own}
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBrig(t, fixture(t, "karta-fixture-a.osm.pbf"), nil)
			if err := b.a.CheckOnce(ctx); err != nil {
				t.Fatal(err)
			}
			s := b.mustSigner()
			s.RunOnce(ctx) // serial 1
			stateBefore, _ := os.ReadFile(filepath.Join(b.state, stateFileName))
			m := b.manifest()
			m.Serial = 7 // newer than the state: what an adoption would take
			signers := c(&m, online.Signer{KeyID: "bridge-test-a", Key: b.keys[0]})
			forged := writePublished(t, b, m, signers...)
			for _, raise := range []*Raise{nil, {To: 7, Reason: "Karta verified 7"}} {
				_, err := b.signer(raise)
				if !errors.Is(err, ErrFailClosed) || !strings.Contains(err.Error(), CodePublishedUntrusted) {
					t.Fatalf("raise %v: %v", raise, err)
				}
			}
			if got, _ := os.ReadFile(filepath.Join(b.publish, ManifestFile)); !bytes.Equal(got, forged) {
				t.Error("the refused manifest was replaced")
			}
			if after, _ := os.ReadFile(filepath.Join(b.state, stateFileName)); !bytes.Equal(after, stateBefore) {
				t.Error("the state changed although the publication was refused")
			}
		})
	}
}

// The published manifest has the serial of the persisted envelope but other
// bytes (even validly signed by the signer's own key): fail closed, at the
// start and during a run, and sign nothing.
func TestSignerFailsClosedOnASameSerialDifferentEnvelope(t *testing.T) {
	ctx := context.Background()
	b := newBrig(t, fixture(t, "karta-fixture-a.osm.pbf"), nil)
	if err := b.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s := b.mustSigner()
	s.RunOnce(ctx) // serial 1, published
	own := online.Signer{KeyID: "bridge-test-a", Key: b.keys[0]}
	m := b.manifest()
	m.IssuedAt = m.IssuedAt.Add(time.Second) // the same serial, other bytes, a valid signature
	writePublished(t, b, m, own)
	if _, err := b.signer(nil); !errors.Is(err, ErrFailClosed) || !strings.Contains(err.Error(), CodeEnvelopeConflict) {
		t.Fatalf("at start: %v", err)
	}
	// During a run: a pending envelope (its publication failed) and a
	// foreign write of that serial meanwhile.
	b2 := newBrig(t, fixture(t, "karta-fixture-a.osm.pbf"), nil)
	if err := b2.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s2 := b2.mustSigner()
	s2.RunOnce(ctx) // serial 1
	spoolNewer(t, b2, time.Now().UTC().Add(-time.Hour))
	unblock := blockManifestWrite(t, b2)
	s2.RunOnce(ctx) // serial 2 persisted, not published
	unblock()
	pending := s2.State().Current
	if pending.Promoted || pending.Serial != 2 {
		t.Fatalf("setup: %+v", pending)
	}
	m2, err := online.ParseUnverified([]byte(pending.Envelope))
	if err != nil {
		t.Fatal(err)
	}
	m2.IssuedAt = m2.IssuedAt.Add(time.Second)
	other := writePublished(t, b2, *m2, online.Signer{KeyID: "bridge-test-a", Key: b2.keys[0]})
	hw := s2.State().HighWater
	b2.clock = pending.ExpiresAt.Add(-time.Minute) // renewal would be due
	s2.RunOnce(ctx)
	st := s2.State()
	if st.LastError == nil || st.LastError.Code != CodeEnvelopeConflict || st.HighWater != hw {
		t.Fatalf("during a run: error %+v, high water %d (was %d)", st.LastError, st.HighWater, hw)
	}
	if got, _ := os.ReadFile(filepath.Join(b2.publish, ManifestFile)); !bytes.Equal(got, other) {
		t.Error("the signer overwrote or renewed over the conflicting manifest")
	}
}

// A genuinely newer, valid publication of the signer (its state restored
// from an older backup) is verified and adopted exactly as published.
func TestSignerAdoptsAVerifiedNewerPublication(t *testing.T) {
	ctx := context.Background()
	b := newBrig(t, fixture(t, "karta-fixture-a.osm.pbf"), nil)
	b.addKey("bridge-test-b")
	b.writeSignerConf(nil)
	if err := b.a.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s := b.mustSigner()
	s.RunOnce(ctx) // serial 1
	old, _ := os.ReadFile(filepath.Join(b.state, stateFileName))
	n := spoolNewer(t, b, time.Now().UTC().Add(-time.Hour))
	s.RunOnce(ctx) // serial 2: newer data, both keys
	published, _ := os.ReadFile(filepath.Join(b.publish, ManifestFile))
	if err := os.WriteFile(filepath.Join(b.state, stateFileName), old, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := b.signer(&Raise{To: 2, Reason: "state restored; Karta verified 2"})
	if err != nil {
		t.Fatal(err)
	}
	c := s.State().Current
	if c == nil || c.Serial != 2 || c.SHA256 != sha(n) || c.Envelope != string(published) || !c.Promoted || len(c.KeyIDs) != 2 {
		t.Fatalf("adopted %+v", c)
	}
	if got, _ := os.ReadFile(filepath.Join(b.publish, ManifestFile)); !bytes.Equal(got, published) {
		t.Error("the verified publication was rewritten")
	}
}
