package online

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
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
)

// fakeSource is a TLS server publishing files by path, counting requests.
type fakeSource struct {
	mu      sync.Mutex
	files   map[string][]byte
	corrupt map[string]bool
	hits    map[string]int
	srv     *httptest.Server
}

func (s *fakeSource) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	b, ok := s.files[r.URL.Path]
	s.hits[r.URL.Path]++
	bad := s.corrupt[r.URL.Path]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if bad {
		b = append([]byte(nil), b...)
		b[0] ^= 0xff
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
}

func (s *fakeSource) set(path string, b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = b
}

func (s *fakeSource) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

type fetcherEnv struct {
	src     *fakeSource
	key     ed25519.PrivateKey
	dir     string
	source  string
	region  string
	now     time.Time
	fetcher *Fetcher
}

func newFetcherEnv(t *testing.T, extra string) *fetcherEnv {
	t.Helper()
	e := &fetcherEnv{src: &fakeSource{files: map[string][]byte{}, corrupt: map[string]bool{}, hits: map[string]int{}}, key: testKey("fetcher"),
		dir: t.TempDir(), now: time.Now().UTC().Truncate(time.Second)}
	e.src.srv = httptest.NewTLSServer(e.src)
	t.Cleanup(e.src.srv.Close)
	cfgDir := t.TempDir()
	ca := filepath.Join(cfgDir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: e.src.srv.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	e.source = filepath.Join(cfgDir, "source.json")
	js := sourceJSON(e.src.srv.URL+"/karta/manifest.json",
		fmt.Sprintf(`,"ca_file":%q,"allowed_networks":["127.0.0.0/8"],"retry_initial":"10s","retry_max":"5m"%s`, ca, extra),
		map[string]ed25519.PrivateKey{"k1": e.key})
	if err := os.WriteFile(e.source, []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	e.region = "../../config/regions/fixture.json"
	return e
}

func (e *fetcherEnv) start(t *testing.T) *Fetcher {
	t.Helper()
	f, err := NewFetcher(FetcherConfig{SourcePath: e.source, RegionPath: e.region, Dir: e.dir, MaxInputBytes: 1 << 30,
		MaxFutureSkew: time.Minute, Version: "test"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f.jitter = func() float64 { return 0.5 }
	e.fetcher = f
	return f
}

// publish puts a snapshot (random bytes of size n) with a sidecar on the
// source under a manifest with serial.
func (e *fetcherEnv) publish(t *testing.T, serial int64, snap []byte) []byte {
	t.Helper()
	prov := []byte(fmt.Sprintf(`{"note":"sidecar for serial %d"}`, serial))
	name := fmt.Sprintf("snap-%d.osm.pbf", serial)
	e.src.set("/karta/"+name, snap)
	e.src.set("/karta/"+name+".provenance.json", prov)
	m := Manifest{Format: ManifestFormat, RegionID: "fixture", BBox: BBox{0, 0, 0.02, 0.015}, Serial: serial,
		IssuedAt: e.now.Add(-time.Minute), ExpiresAt: e.now.Add(24 * time.Hour),
		Snapshot:   Snapshot{URL: name, SHA256: digestHex(snap), SizeBytes: int64(len(snap)), DataTimestamp: e.now.Add(-time.Hour)},
		Provenance: &File{URL: name + ".provenance.json", SHA256: digestHex(prov), SizeBytes: int64(len(prov))}}
	raw := signed(t, m, Signer{"k1", e.key})
	e.src.set("/karta/manifest.json", raw)
	return raw
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func TestFetcherDeliversVerifiedSnapshotsOnce(t *testing.T) {
	e := newFetcherEnv(t, "")
	f := e.start(t)
	ctx := context.Background()
	s1 := randomBytes(300000)
	raw1 := e.publish(t, 1, s1)
	f.RunOnce(ctx)
	st := f.State()
	if st.LastError != nil || st.LastSuccessAt == nil || st.Delivered == nil || st.Delivered.Serial != 1 || st.HighestSerial != 1 {
		t.Fatalf("state after the first check: %+v %+v", st, st.LastError)
	}
	name := DeliveryName("fixture", 1, digestHex(s1))
	if st.Delivered.Name != name || !deliveryPattern.MatchString(name) || !inbox.NamePattern.MatchString(name) {
		t.Fatalf("delivery name %q", st.Delivered.Name)
	}
	for suf, want := range map[string][]byte{inbox.SnapshotSuffix: s1, inbox.ManifestSuffix: raw1,
		inbox.MarkerSuffix: []byte(digestHex(s1) + "  " + name + ".osm.pbf\n")} {
		b, err := os.ReadFile(filepath.Join(e.dir, name+suf))
		if err != nil || !bytes.Equal(b, want) {
			t.Errorf("%s: %v (%d bytes)", suf, err, len(b))
		}
		if fi, _ := os.Stat(filepath.Join(e.dir, name+suf)); fi == nil || fi.Mode().Perm() != 0o644 {
			t.Errorf("%s mode %v", suf, fi)
		}
	}
	if left, _ := os.ReadDir(filepath.Join(e.dir, PartialDirName)); len(left) != 0 {
		t.Errorf("partial files left: %v", left)
	}
	// The publisher sees exactly one complete delivery with its manifest,
	// and can stage it.
	entries, err := inbox.ScanWith(e.dir, 100, inbox.ScanOptions{Manifests: true})
	if err != nil || len(entries) != 1 || !entries[0].Complete() || entries[0].Manifest == nil || entries[0].Sidecar == nil {
		t.Fatalf("publisher view: %v %+v", err, entries)
	}
	staged, err := inbox.Stage(e.dir, entries[0], t.TempDir(), "job", inbox.Options{MaxSnapshotBytes: 1 << 30, RequireManifest: true})
	if err != nil || staged.SHA256 != digestHex(s1) || staged.ManifestPath == "" {
		t.Fatalf("stage: %v %+v", err, staged)
	}
	// The publisher reads the state file.
	if rs, err := ReadState(e.dir); err != nil || rs == nil || rs.Delivered == nil || rs.Delivered.Name != name {
		t.Fatalf("ReadState: %v %+v", err, rs)
	}

	// Unchanged source: checked, nothing downloaded again.
	f.RunOnce(ctx)
	if n := e.src.count("/karta/snap-1.osm.pbf"); n != 1 {
		t.Errorf("snapshot requested %d times", n)
	}
	if f.State().LastError != nil {
		t.Errorf("unchanged source: %+v", f.State().LastError)
	}

	// A different manifest under the same serial is a conflicting claim.
	m, _ := ParseUnverified(raw1)
	m.IssuedAt = m.IssuedAt.Add(time.Second)
	e.src.set("/karta/manifest.json", signed(t, *m, Signer{"k1", e.key}))
	f.RunOnce(ctx)
	if st := f.State(); st.LastError == nil || st.LastError.Code != CodeManifestConflict || st.ConsecutiveFailures != 1 {
		t.Fatalf("conflict: %+v", st.LastError)
	}
	next := *f.State().NextAttemptAt
	if d := time.Until(next); d < 4*time.Second || d > 11*time.Second {
		t.Errorf("backoff after one failure: next attempt in %s", d)
	}

	// Serial 2 delivers a new snapshot; serial 3 too; only two stay.
	s2, s3 := randomBytes(1000), randomBytes(1000)
	e.publish(t, 2, s2)
	f.RunOnce(ctx)
	if st := f.State(); st.LastError != nil || st.ConsecutiveFailures != 0 || st.Delivered.Serial != 2 {
		t.Fatalf("serial 2: %+v", st.LastError)
	}
	old := e.src.files["/karta/manifest.json"]
	e.publish(t, 3, s3)
	f.RunOnce(ctx)
	entries, _ = inbox.ScanWith(e.dir, 100, inbox.ScanOptions{Manifests: true})
	if len(entries) != 2 || entries[0].Name != DeliveryName("fixture", 2, digestHex(s2)) || entries[1].Name != DeliveryName("fixture", 3, digestHex(s3)) {
		t.Fatalf("outbox after serial 3: %+v", entries)
	}

	// The source going back to an older manifest is a replay.
	e.src.set("/karta/manifest.json", old)
	f.RunOnce(ctx)
	if st := f.State(); st.LastError == nil || st.LastError.Code != CodeManifestReplayed || st.HighestSerial != 3 {
		t.Fatalf("replay: %+v", st.LastError)
	}

	// The same snapshot under a newer manifest is not downloaded again.
	before := e.src.count("/karta/snap-3.osm.pbf")
	m3, _ := ParseUnverified(e.publish(t, 3, s3))
	m3.Serial = 4
	e.src.set("/karta/manifest.json", signed(t, *m3, Signer{"k1", e.key}))
	f.RunOnce(ctx)
	if st := f.State(); st.LastError != nil || st.HighestSerial != 4 || st.Delivered.Serial != 3 || e.src.count("/karta/snap-3.osm.pbf") != before {
		t.Fatalf("same digest, new serial: %+v %+v", st.LastError, st.Delivered)
	}

	// An expired manifest is refused (the source went stale).
	m3.Serial, m3.IssuedAt, m3.ExpiresAt = 5, e.now.Add(-3*time.Hour), e.now.Add(-time.Hour)
	e.src.set("/karta/manifest.json", signed(t, *m3, Signer{"k1", e.key}))
	f.RunOnce(ctx)
	if st := f.State(); st.LastError == nil || st.LastError.Code != CodeManifestExpired {
		t.Fatalf("expired: %+v", st.LastError)
	}
}

func TestFetcherSetsASnapshotAsideAfterRepeatedFailures(t *testing.T) {
	e := newFetcherEnv(t, `,"max_download_attempts":2,"abandon_for":"1h"`)
	f := e.start(t)
	ctx := context.Background()
	s1 := randomBytes(5000)
	e.publish(t, 1, s1)
	e.src.corrupt["/karta/snap-1.osm.pbf"] = true
	for i := 1; i <= 2; i++ {
		f.RunOnce(ctx)
		if st := f.State(); st.LastError == nil || st.LastError.Code != CodeDigestMismatch || st.ConsecutiveFailures != i {
			t.Fatalf("attempt %d: %+v", i, st.LastError)
		}
	}
	if d := f.State().Download; d == nil || d.AbandonedUntil == nil || d.Attempts != 2 {
		t.Fatalf("not set aside: %+v", d)
	}
	f.RunOnce(ctx)
	if st := f.State(); st.LastError == nil || st.LastError.Code != CodeAbandoned || e.src.count("/karta/snap-1.osm.pbf") != 2 {
		t.Fatalf("set aside: %+v, %d downloads", st.LastError, e.src.count("/karta/snap-1.osm.pbf"))
	}
	// A different snapshot is tried at once.
	s2 := randomBytes(5000)
	e.publish(t, 2, s2)
	f.RunOnce(ctx)
	if st := f.State(); st.LastError != nil || st.Delivered == nil || st.Delivered.Serial != 2 {
		t.Fatalf("new snapshot: %+v", st.LastError)
	}
}

func TestFetcherRecoversFromAnInterruptedDelivery(t *testing.T) {
	e := newFetcherEnv(t, "")
	f := e.start(t)
	ctx := context.Background()
	s1 := randomBytes(20000)
	e.publish(t, 1, s1)
	f.RunOnce(ctx)
	name := f.State().Delivered.Name

	// Simulate a crash between linking the snapshot and writing the marker:
	// the verified bytes are still in the partial directory, the delivery
	// has no marker, and the state does not record it.
	part := PartialPath(filepath.Join(e.dir, PartialDirName), digestHex(s1))
	if err := os.Link(filepath.Join(e.dir, name+inbox.SnapshotSuffix), part); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.dir, name+inbox.MarkerSuffix)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, ".tmp-x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.dir, StateDirName, StateFileName)); err != nil {
		t.Fatal(err)
	}
	f2 := e.start(t)
	for _, n := range []string{name + inbox.SnapshotSuffix, name + inbox.ManifestSuffix, name + inbox.SidecarSuffix, ".tmp-x"} {
		if _, err := os.Lstat(filepath.Join(e.dir, n)); !os.IsNotExist(err) {
			t.Errorf("%s survived recovery", n)
		}
	}
	f2.RunOnce(ctx)
	if st := f2.State(); st.LastError != nil || st.Delivered == nil || st.Delivered.Name != name {
		t.Fatalf("re-delivery: %+v", st.LastError)
	}
	if n := e.src.count("/karta/snap-1.osm.pbf"); n != 1 {
		t.Errorf("the verified partial was downloaded again (%d requests)", n)
	}

	// A complete delivery the state does not know (crash after the marker)
	// is adopted and not downloaded again.
	if err := os.Remove(filepath.Join(e.dir, StateDirName, StateFileName)); err != nil {
		t.Fatal(err)
	}
	f3 := e.start(t)
	if st := f3.State(); st.Delivered == nil || st.Delivered.Name != name || st.HighestSerial != 1 {
		t.Fatalf("not adopted: %+v", st.Delivered)
	}
	f3.RunOnce(ctx)
	if n := e.src.count("/karta/snap-1.osm.pbf"); n != 1 || f3.State().LastError != nil {
		t.Errorf("adopted delivery: %d downloads, %+v", n, f3.State().LastError)
	}

	// A second fetcher cannot share the outbox.
	l1, err := LockOutbox(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Close()
	if _, err := LockOutbox(e.dir); err == nil {
		t.Error("two fetchers locked one outbox")
	}
}

func TestFetcherRefusesAForeignRegionOrKey(t *testing.T) {
	e := newFetcherEnv(t, "")
	f := e.start(t)
	s1 := randomBytes(100)
	e.publish(t, 1, s1)
	m, _ := ParseUnverified(e.src.files["/karta/manifest.json"])
	e.src.set("/karta/manifest.json", signed(t, *m, Signer{"k1", testKey("intruder")}))
	f.RunOnce(context.Background())
	if st := f.State(); st.LastError == nil || st.LastError.Code != CodeSignatureInvalid || st.Delivered != nil {
		t.Fatalf("forged signature: %+v", st.LastError)
	}
	if n := e.src.count("/karta/snap-1.osm.pbf"); n != 0 {
		t.Errorf("the snapshot of a forged manifest was downloaded")
	}
	e.region = "../../config/regions/tehran-chitgar.json"
	f2 := e.start(t)
	f2.RunOnce(context.Background())
	if st := f2.State(); st.LastError == nil || st.LastError.Code != CodeConfig {
		t.Fatalf("region mismatch: %+v", st.LastError)
	}
}

func TestBackoff(t *testing.T) {
	for _, c := range []struct {
		failures int
		jitter   float64
		want     time.Duration
	}{
		{1, 0, 30 * time.Second}, {1, 0.999999, 60 * time.Second}, {2, 0, time.Minute}, {3, 0, 2 * time.Minute},
		{20, 0, 30 * time.Minute}, {20, 0.999999, time.Hour}, {1000, 0.5, 45 * time.Minute},
	} {
		got := Backoff(time.Minute, time.Hour, c.failures, c.jitter)
		if d := got - c.want; d < -time.Second || d > time.Second {
			t.Errorf("Backoff(%d, %v) = %s, want %s", c.failures, c.jitter, got, c.want)
		}
	}
}

func TestReadStateIsStrictAndBounded(t *testing.T) {
	dir := t.TempDir()
	if s, err := ReadState(dir); s != nil || err != nil {
		t.Fatalf("missing state: %v %v", s, err)
	}
	if err := os.Mkdir(filepath.Join(dir, StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, StateDirName, StateFileName)
	for name, content := range map[string]string{
		"unknown field": `{"version":1,"updated_at":"2026-10-01T00:00:00Z","source":"","last_check_at":null,"last_success_at":null,"last_error":null,"consecutive_failures":0,"next_attempt_at":null,"highest_serial":0,"current":null,"delivered":null,"download":null,"x":1}`,
		"other version": `{"version":2}`,
		"oversized":     `{"version":1,"source":"` + strings.Repeat("x", MaxStateBytes) + `"}`,
	} {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadState(dir); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_ = os.Remove(p)
	if err := os.Symlink("/etc/hostname", p); err == nil {
		if _, err := ReadState(dir); err == nil {
			t.Error("a symlinked state file was followed")
		}
	}
}

func TestLimitsAgreeWithTheInbox(t *testing.T) {
	if inbox.MaxManifestBytes != MaxEnvelopeBytes || inbox.MaxSidecarBytes != MaxProvenanceBytes {
		t.Fatal("the publisher's staging limits differ from the fetcher's")
	}
	if !inbox.NamePattern.MatchString(DeliveryName("a-region-id-of-sixty-three-characters-is-still-a-valid-name-xx", 1<<40, strings.Repeat("f", 64))) {
		t.Fatal("a delivery name is not a valid submission name")
	}
}
