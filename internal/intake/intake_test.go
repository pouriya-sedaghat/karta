package intake

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/inbox"
	"github.com/pouriya-sedaghat/karta/internal/pbfwrite"
)

const testToken = "4444444444444444444444444444444444444444444444444444444444444444"

// fakeAPI is an in-memory stand-in for the operator API's intake endpoints:
// one credential, the limits the publisher enforces, and a hook to give a
// handoff name a submission outcome.
type fakeAPI struct {
	mu       sync.Mutex
	maxOpen  int
	maxTTL   float64
	region   string
	nextID   int64
	auths    []Authorization
	subs     map[string]*Submission
	down     bool
	requests []string
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{maxOpen: 2, maxTTL: 3600, region: "fixture", nextID: 1, subs: map[string]*Submission{}}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if f.down {
		http.Error(w, `{"error":{"code":"service_unavailable","message":"down"}}`, http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"unauthenticated","message":"no"}}`)
		return
	}
	now := time.Now()
	writeJSON := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/operator/intake/authorizations":
		recs := []Record{}
		for _, a := range f.auths {
			recs = append(recs, Record{Authorization: a, Submission: f.subs[*a.IntakeName]})
		}
		writeJSON(200, List{Limits: Limits{RegionID: f.region, MaxTTLSeconds: f.maxTTL, MaxOpen: f.maxOpen, MaxSnapshotBytes: 1 << 30}, Records: recs})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/operator/intake/authorizations":
		var req AuthorizeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(400, map[string]any{"error": map[string]string{"code": "invalid_request", "message": err.Error()}})
			return
		}
		if req.RegionID != f.region || float64(req.TTLSeconds) > f.maxTTL || req.SizeBytes < 1 {
			writeJSON(409, map[string]any{"error": map[string]string{"code": "intake_refused", "message": "bounds"}})
			return
		}
		open := 0
		for _, a := range f.auths {
			if a.Open(now) {
				open++
			}
		}
		if open >= f.maxOpen {
			writeJSON(409, map[string]any{"error": map[string]string{"code": "intake_limit_reached", "message": "limit"}})
			return
		}
		exp := now.Add(time.Duration(req.TTLSeconds) * time.Second)
		size, name := req.SizeBytes, req.Name
		a := Authorization{ID: f.nextID, RegionID: req.RegionID, SHA256: req.SHA256, SizeBytes: &size, CreatedBy: "local-intake",
			CreatedAt: now, ExpiresAt: &exp, Channel: "intake_watch", IntakeName: &name}
		f.nextID++
		f.auths = append(f.auths, a)
		writeJSON(201, map[string]any{"authorization": a, "created": true})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
		id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/operator/intake/authorizations/"), "/close"), 10, 64)
		for i := range f.auths {
			if f.auths[i].ID == id {
				closed := f.auths[i].RevokedAt == nil
				if closed {
					f.auths[i].RevokedAt = &now
				}
				writeJSON(200, map[string]bool{"closed": closed})
				return
			}
		}
		writeJSON(404, map[string]any{"error": map[string]string{"code": "not_found", "message": "no"}})
	default:
		writeJSON(404, map[string]any{"error": map[string]string{"code": "not_found", "message": "no"}})
	}
}

func (f *fakeAPI) authorizations() []Authorization {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Authorization(nil), f.auths...)
}

func (f *fakeAPI) openCount() int {
	n := 0
	for _, a := range f.authorizations() {
		if a.Open(time.Now()) {
			n++
		}
	}
	return n
}

func (f *fakeAPI) finish(name, state, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	s := &Submission{ID: 1, Source: "intake", Name: name, State: state, FinishedAt: &now}
	if code != "" {
		s.ReasonCode = &code
	}
	f.subs[name] = s
}

func (f *fakeAPI) addAuthorization(name, digest string, size int64) Authorization {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	exp := now.Add(time.Hour)
	a := Authorization{ID: f.nextID, RegionID: f.region, SHA256: digest, SizeBytes: &size, CreatedBy: "local-intake", CreatedAt: now,
		ExpiresAt: &exp, Channel: "intake_watch", IntakeName: &name}
	f.nextID++
	f.auths = append(f.auths, a)
	return a
}

// --- fixtures -------------------------------------------------------------------

func repoFile(t *testing.T, p string) string { t.Helper(); return filepath.Join("..", "..", p) }

func fixtureA(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(repoFile(t, "testdata/fixture/snapshots/karta-fixture-a.osm.pbf"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// otherBox is fixture B's data with another header box (another extract).
func otherBox(t *testing.T) []byte {
	t.Helper()
	f, err := os.Open(repoFile(t, "testdata/fixture/karta-fixture-b.osm"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d, err := pbfwrite.ParseXML(f)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	d.Timestamp, d.BBox = &ts, &[4]float64{1, 1, 1.02, 1.015}
	b, err := pbfwrite.Encode(d, pbfwrite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// lastBlobStart returns the offset of the last blob: cutting there leaves a
// structurally valid PBF that lacks its tail.
func lastBlobStart(b []byte) int {
	off, last := 0, 0
	for off < len(b) {
		hl := int(b[off])<<24 | int(b[off+1])<<16 | int(b[off+2])<<8 | int(b[off+3])
		hdr := b[off+4 : off+4+hl]
		size, i := 0, 0
		for i < len(hdr) {
			key := hdr[i]
			i++
			if key == 0x18 {
				shift := 0
				for {
					c := hdr[i]
					i++
					size |= int(c&0x7f) << shift
					if c < 0x80 {
						break
					}
					shift += 7
				}
				continue
			}
			if key&7 == 2 {
				l := int(hdr[i])
				i += 1 + l
			}
		}
		last = off
		off += 4 + hl + size
	}
	return last
}

func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func completion(name string, b []byte) []byte {
	return []byte(fmt.Sprintf(`{"format":"karta-delivery/1","file":"%s.osm.pbf","sha256":"%s","size_bytes":%d}`, name, digest(b), len(b)))
}

type rig struct {
	t       *testing.T
	root    string // stands in for the host root
	landing string // landing directory (inside root)
	handoff string
	api     *fakeAPI
	srv     *httptest.Server
	w       *Watcher
	fs      int64 // filesystem type the preflight sees
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	r := &rig{t: t, root: root, landing: filepath.Join(root, "srv", "landing"), handoff: filepath.Join(root, "handoff"), api: newFakeAPI(), fs: 0xEF53}
	for _, d := range []string{filepath.Dir(r.landing), r.landing, r.handoff} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.srv = httptest.NewServer(r.api)
	t.Cleanup(r.srv.Close)
	tok := filepath.Join(root, "token")
	if err := os.WriteFile(tok, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(r.srv.URL, tok, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWatcher(WatcherConfig{Landing: "/srv/landing", Root: root, OwnerUID: uint32(os.Getuid()), Settle: 0, Poll: time.Second, // #nosec G115 -- test uid
		Statfs: func(string) (int64, error) { return r.fs, nil },
		Handoff: HandoffConfig{Dir: r.handoff, RegionPath: repoFile(t, "config/regions/fixture.json"), Client: c, MaxInputBytes: 1 << 30}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	r.w = w
	return r
}

// put writes a landing file the producer's way: a temporary name, then a
// rename.
func (r *rig) put(name string, b []byte) {
	r.t.Helper()
	tmp := filepath.Join(r.landing, "."+name+".part")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(r.landing, name)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) scan() State {
	r.t.Helper()
	r.w.ScanOnce(context.Background())
	st, err := ReadState(r.handoff)
	if err != nil || st == nil {
		r.t.Fatalf("state: %v %v", st, err)
	}
	return *st
}

func (r *rig) entry(st State, name string) LandingEntry {
	r.t.Helper()
	for _, e := range st.Entries {
		if e.Name == name {
			return e
		}
	}
	r.t.Fatalf("no landing entry %s in %+v", name, st.Entries)
	return LandingEntry{}
}

// handoffs lists the visible handoff names with a ready marker.
func (r *rig) handoffs() []string {
	r.t.Helper()
	entries, err := inbox.Scan(r.handoff, 100)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.Complete() {
			out = append(out, e.Name)
		}
	}
	return out
}

// --- tests ----------------------------------------------------------------------

func TestParseCompletion(t *testing.T) {
	d := strings.Repeat("a", 64)
	good := `{"format":"karta-delivery/1","file":"x.osm.pbf","sha256":"` + strings.ToUpper(d) + `","size_bytes":5}`
	c, err := ParseCompletion([]byte("\xef\xbb\xbf"+strings.ReplaceAll(good, ",", ",\r\n")+"\r\n"), "x.osm.pbf")
	if err != nil || c.SHA256 != d || c.SizeBytes != 5 {
		t.Fatalf("BOM, CRLF and upper case: %+v %v", c, err)
	}
	for name, b := range map[string]string{
		"other file":    strings.Replace(good, "x.osm.pbf", "y.osm.pbf", 1),
		"other format":  strings.Replace(good, "karta-delivery/1", "karta-delivery/2", 1),
		"short digest":  strings.Replace(good, strings.ToUpper(d), "abc", 1),
		"zero size":     strings.Replace(good, `"size_bytes":5`, `"size_bytes":0`, 1),
		"unknown field": strings.Replace(good, `}`, `,"source":"geofabrik"}`, 1),
		"repeated key":  strings.Replace(good, `}`, `,"size_bytes":6}`, 1),
		"a sha256sum":   d + "  x.osm.pbf\n",
		"two objects":   good + good,
	} {
		if _, err := ParseCompletion([]byte(b), "x.osm.pbf"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPreflight(t *testing.T) {
	uid := uint32(os.Getuid()) // #nosec G115 -- test uid
	gid := uint32(os.Getgid()) // #nosec G115 -- test gid
	run := func(t *testing.T, root, dir string, fsType int64, o PreflightOptions) Preflight {
		o.Dir, o.Root = dir, root
		if o.OwnerUID == 0 {
			o.OwnerUID = uid
		}
		o.Statfs = func(string) (int64, error) { return fsType, nil }
		return RunPreflight(o)
	}
	codes := func(p Preflight) string {
		var c []string
		for _, x := range p.Problems {
			c = append(c, x.Code)
		}
		return strings.Join(c, ",")
	}
	setup := func(t *testing.T) string {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "srv", "landing"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, d := range []string{"srv", "srv/landing"} {
			_ = os.Chmod(filepath.Join(root, d), 0o755)
		}
		return root
	}
	t.Run("a local directory of the owner passes", func(t *testing.T) {
		root := setup(t)
		for _, fs := range []int64{0xEF53, 0x58465342, 0x01021994} {
			if p := run(t, root, "/srv/landing", fs, PreflightOptions{}); !p.OK {
				t.Errorf("fs 0x%x: %+v", fs, p.Problems)
			}
		}
	})
	t.Run("shared, network and FUSE filesystems are refused", func(t *testing.T) {
		root := setup(t)
		for fs, name := range map[int64]string{0x786f4256: "vboxsf", 0xFF534D42: "cifs", 0x65735546: "fuse", 0x01021997: "9p", 0x6969: "nfs",
			0x5346544e: "ntfs", 0x794c7630: "overlay", 0x12345678: "unknown"} {
			p := run(t, root, "/srv/landing", fs, PreflightOptions{})
			if p.OK || codes(p) != ProblemFilesystem || !strings.Contains(p.FSType, name) {
				t.Errorf("%s: %+v %s", name, p, p.FSType)
			}
		}
	})
	t.Run("owner, mode and parents", func(t *testing.T) {
		root := setup(t)
		landing := filepath.Join(root, "srv", "landing")
		if p := run(t, root, "/srv/landing", 0xEF53, PreflightOptions{OwnerUID: uid + 1}); p.OK || !strings.Contains(codes(p), ProblemOwner) {
			t.Errorf("wrong owner: %+v", p)
		}
		_ = os.Chmod(landing, 0o757)
		if p := run(t, root, "/srv/landing", 0xEF53, PreflightOptions{}); p.OK || codes(p) != ProblemWorldWritable {
			t.Errorf("world-writable: %+v", p)
		}
		_ = os.Chmod(landing, 0o775)
		if p := run(t, root, "/srv/landing", 0xEF53, PreflightOptions{}); p.OK || codes(p) != ProblemGroupWritable {
			t.Errorf("group-writable without a writer group: %+v", p)
		}
		if p := run(t, root, "/srv/landing", 0xEF53, PreflightOptions{WriterGID: &gid}); !p.OK {
			t.Errorf("group-writable by the writer group: %+v", p)
		}
		_ = os.Chmod(landing, 0o755)
		_ = os.Chmod(filepath.Join(root, "srv"), 0o777)
		if p := run(t, root, "/srv/landing", 0xEF53, PreflightOptions{}); p.OK || !strings.Contains(codes(p), ProblemParentWritable) ||
			strings.Contains(codes(p), ProblemWorldWritable) {
			t.Errorf("writable parent: %+v", p)
		}
		_ = os.Chmod(filepath.Join(root, "srv"), 0o755)
	})
	t.Run("symlinks are refused", func(t *testing.T) {
		root := setup(t)
		if err := os.Symlink(filepath.Join(root, "srv", "landing"), filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		if p := run(t, root, "/link", 0xEF53, PreflightOptions{}); p.OK || codes(p) != ProblemSymlink {
			t.Errorf("symlinked landing: %+v", p)
		}
		if err := os.Symlink(filepath.Join(root, "srv"), filepath.Join(root, "srvlink")); err != nil {
			t.Fatal(err)
		}
		if p := run(t, root, "/srvlink/landing", 0xEF53, PreflightOptions{}); p.OK || !strings.Contains(codes(p), ProblemParentSymlink) {
			t.Errorf("symlinked parent: %+v", p)
		}
		if p := run(t, root, "/missing", 0xEF53, PreflightOptions{}); p.OK || codes(p) != ProblemMissing {
			t.Errorf("missing: %+v", p)
		}
	})
}

func TestWatcherDeliversACompleteDelivery(t *testing.T) {
	r := newRig(t)
	data := fixtureA(t)
	r.put("iran-1.osm.pbf", data)
	if st := r.scan(); r.entry(st, "iran-1").State != WaitingForCompletion {
		t.Fatalf("no marker: %+v", st.Entries)
	}
	r.put("iran-1.osm.pbf.complete", completion("iran-1", data))
	st := r.scan()
	if e := r.entry(st, "iran-1"); e.State != StateDelivered {
		t.Fatalf("delivery: %+v %+v", e, st.LastError)
	}
	names := r.handoffs()
	if len(names) != 1 || !strings.HasPrefix(names[0], "fixture-w") || !strings.HasSuffix(names[0], digest(data)[:12]) {
		t.Fatalf("handoff %v", names)
	}
	got, err := os.ReadFile(filepath.Join(r.handoff, names[0]+".osm.pbf"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("handed-off bytes differ: %v", err)
	}
	marker, _ := os.ReadFile(filepath.Join(r.handoff, names[0]+".osm.pbf.ready"))
	if d, err := inbox.ParseMarker(marker, names[0]+".osm.pbf"); err != nil || d != digest(data) {
		t.Fatalf("marker %q %v", marker, err)
	}
	if fi, _ := os.Stat(filepath.Join(r.handoff, names[0]+".osm.pbf")); fi.Mode().Perm() != 0o644 {
		t.Errorf("handoff mode %v", fi.Mode())
	}
	auths := r.api.authorizations()
	if len(auths) != 1 || auths[0].SHA256 != digest(data) || *auths[0].SizeBytes != int64(len(data)) || *auths[0].IntakeName != names[0] {
		t.Fatalf("authorizations %+v", auths)
	}
	// Consumed: the same files are not delivered twice.
	r.scan()
	if len(r.api.authorizations()) != 1 || len(r.handoffs()) != 1 {
		t.Fatal("a consumed delivery was handed off again")
	}
	// The publisher's outcome: the handoff is cleaned up and its
	// authorization closed.
	r.api.finish(names[0], "published", "")
	st = r.scan()
	if len(r.handoffs()) != 0 || r.api.openCount() != 0 {
		t.Fatalf("not cleaned up: %v open %d", r.handoffs(), r.api.openCount())
	}
	if st.LastOutcome == nil || st.LastOutcome.SubmissionState != "published" || st.LastOutcome.From != "iran-1" {
		t.Errorf("last outcome %+v", st.LastOutcome)
	}
	for _, n := range dirNames(t, r.handoff) {
		if n != StateDirName {
			t.Errorf("left in the handoff directory: %s", n)
		}
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// A copy cut exactly at a blob boundary is a structurally valid PBF; a
// checksum the destination computed of itself matches it. Neither is a
// completion signal: only the producer's marker with the source's digest
// is, and that refuses the truncated copy.
func TestWatcherRefusesIncompleteCopies(t *testing.T) {
	r := newRig(t)
	data := fixtureA(t)
	cut := data[:lastBlobStart(data)]
	r.put("cut.osm.pbf", cut)
	r.put("cut.osm.pbf.sha256", []byte(digest(cut)+"  cut.osm.pbf\n"))
	r.put("cut.osm.pbf.md5", []byte("d41d8cd98f00b204e9800998ecf8427e  cut.osm.pbf\n"))
	if e := r.entry(r.scan(), "cut"); e.State != WaitingForCompletion {
		t.Fatalf("a self-made checksum was taken as completion: %+v", e)
	}
	// The producer's marker (the full source's digest and size).
	r.put("cut.osm.pbf.complete", completion("cut", data))
	if e := r.entry(r.scan(), "cut"); e.State != StateRefused || e.Code != CodeSizeMismatch {
		t.Fatalf("truncated copy: %+v", e)
	}
	// Same size, different bytes (corruption in transfer).
	bad := append([]byte(nil), data...)
	bad[len(bad)/2] ^= 0xff
	r.put("bad.osm.pbf", bad)
	r.put("bad.osm.pbf.complete", completion("bad", data))
	if e := r.entry(r.scan(), "bad"); e.State != StateRefused || e.Code != CodeDigestMismatch {
		t.Fatalf("corrupted copy: %+v", e)
	}
	// A marker written before the snapshot was replaced is stale.
	r.put("stale.osm.pbf.complete", completion("stale", data))
	time.Sleep(20 * time.Millisecond)
	r.put("stale.osm.pbf", data)
	if e := r.entry(r.scan(), "stale"); e.State != StateRefused || e.Code != CodeStaleCompletion {
		t.Fatalf("stale marker: %+v", e)
	}
	// A malformed marker.
	r.put("junk.osm.pbf", data)
	r.put("junk.osm.pbf.complete", []byte(digest(data)))
	if e := r.entry(r.scan(), "junk"); e.State != StateRefused || e.Code != CodeInvalidCompletion {
		t.Fatalf("malformed marker: %+v", e)
	}
	// Another extract (header box) is refused before authorizing.
	other := otherBox(t)
	r.put("other.osm.pbf", other)
	r.put("other.osm.pbf.complete", completion("other", other))
	if e := r.entry(r.scan(), "other"); e.State != StateRefused || e.Code != CodeRegionMismatch {
		t.Fatalf("other region: %+v", e)
	}
	if n := len(r.api.authorizations()); n != 0 {
		t.Fatalf("%d authorizations for refused deliveries", n)
	}
	if h := r.handoffs(); len(h) != 0 {
		t.Fatalf("refused deliveries handed off: %v", h)
	}
	// Fixing a refusal is a new delivery: the producer writes a new marker.
	r.put("bad.osm.pbf", data)
	time.Sleep(20 * time.Millisecond)
	r.put("bad.osm.pbf.complete", completion("bad", data))
	if e := r.entry(r.scan(), "bad"); e.State != StateDelivered {
		t.Fatalf("re-delivery: %+v", e)
	}
}

func TestWatcherRefusesUnsafeFiles(t *testing.T) {
	r := newRig(t)
	data := fixtureA(t)
	outside := filepath.Join(r.root, "outside.osm.pbf")
	if err := os.WriteFile(outside, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(r.landing, "sym.osm.pbf")); err != nil {
		t.Fatal(err)
	}
	r.put("sym.osm.pbf.complete", completion("sym", data))
	if err := syscall.Mkfifo(filepath.Join(r.landing, "fifo.osm.pbf"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.put("fifo.osm.pbf.complete", completion("fifo", data))
	if err := os.Link(outside, filepath.Join(r.landing, "hard.osm.pbf")); err != nil {
		t.Fatal(err)
	}
	r.put("hard.osm.pbf.complete", completion("hard", data))
	if err := os.Mkdir(filepath.Join(r.landing, "dir.osm.pbf"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.put("dir.osm.pbf.complete", completion("dir", data))
	st := r.scan()
	for name, code := range map[string]string{"sym": CodeSymlink, "fifo": CodeNotRegular, "hard": CodeHardLink, "dir": CodeNotRegular} {
		if e := r.entry(st, name); e.State != StateRefused || e.Code != code {
			t.Errorf("%s: %+v", name, e)
		}
	}
	if len(r.api.authorizations()) != 0 {
		t.Fatal("an unsafe file was authorized")
	}
	// An insecure landing directory: nothing is processed at all.
	_ = os.Chmod(r.landing, 0o777)
	r.put("ok.osm.pbf", data)
	r.put("ok.osm.pbf.complete", completion("ok", data))
	st = r.scan()
	if st.Preflight.OK || len(st.Entries) != 0 || len(r.api.authorizations()) != 0 {
		t.Fatalf("insecure landing processed: %+v", st)
	}
	// A shared folder: refused even with correct mode bits.
	_ = os.Chmod(r.landing, 0o755)
	r.fs = 0x786f4256
	if st = r.scan(); st.Preflight.OK || len(r.api.authorizations()) != 0 {
		t.Fatalf("vboxsf landing processed: %+v", st.Preflight)
	}
	r.fs = 0xEF53
	if e := r.entry(r.scan(), "ok"); e.State != StateDelivered {
		t.Fatalf("after the fix: %+v", e)
	}
}

func TestWatcherQueueAndOutages(t *testing.T) {
	r := newRig(t)
	r.api.maxOpen = 1
	data := fixtureA(t)
	r.put("one.osm.pbf", data)
	r.put("one.osm.pbf.complete", completion("one", data))
	r.put("two.osm.pbf", data)
	r.put("two.osm.pbf.complete", completion("two", data))
	st := r.scan()
	if r.entry(st, "one").State != StateDelivered || r.entry(st, "two").State != WaitingQueueFull {
		t.Fatalf("queue: %+v", st.Entries)
	}
	// The publisher (operator API) is down: nothing is consumed or lost.
	r.api.mu.Lock()
	r.api.down = true
	r.api.mu.Unlock()
	if st = r.scan(); st.LastError == nil || st.LastError.Code != "operator_api" {
		t.Fatalf("outage not reported: %+v", st.LastError)
	}
	r.api.mu.Lock()
	r.api.down = false
	r.api.mu.Unlock()
	names := r.handoffs()
	r.api.finish(names[0], "duplicate", "duplicate_active")
	if st = r.scan(); r.entry(st, "two").State != StateDelivered {
		t.Fatalf("second delivery after the first finished: %+v", st.Entries)
	}
}

func TestWatcherSettles(t *testing.T) {
	r := newRig(t)
	r.w.cfg.Settle = time.Hour
	data := fixtureA(t)
	r.put("s.osm.pbf", data)
	r.put("s.osm.pbf.complete", completion("s", data))
	if e := r.entry(r.scan(), "s"); e.State != WaitingSettling {
		t.Fatalf("%+v", e)
	}
	r.w.cfg.Settle = 0
	if e := r.entry(r.scan(), "s"); e.State != StateDelivered {
		t.Fatalf("%+v", e)
	}
}

// Crash recovery: a handoff interrupted after its authorization is completed
// only if its copy still has the authorized digest and the landing delivery
// is unchanged; otherwise it is discarded and the authorization closed.
func TestWatcherRecoversInterruptedHandoffs(t *testing.T) {
	data := fixtureA(t)
	crashAfterAuthorize := func(t *testing.T, r *rig, from string) (string, Authorization) {
		t.Helper()
		r.put(from+".osm.pbf", data)
		r.put(from+".osm.pbf.complete", completion(from, data))
		entries, err := scanLanding(r.landing, uint32(os.Getuid()), 1<<30) // #nosec G115 -- test uid
		if err != nil {
			t.Fatal(err)
		}
		var e landingEntry
		for _, x := range entries {
			if x.Name == from {
				e = x
			}
		}
		name := "fixture-w20260101T000000Z-" + digest(data)[:12]
		_, _, _, hidden, _ := r.w.h.paths(name)
		if err := os.WriteFile(hidden, data, 0o644); err != nil {
			t.Fatal(err)
		}
		a := r.api.addAuthorization(name, digest(data), int64(len(data)))
		r.w.state.Handoffs = append(r.w.state.Handoffs, Handoff{Name: name, From: from, SHA256: digest(data), SizeBytes: int64(len(data)),
			Channel: "intake_watch", LandingFingerprint: e.fingerprint()})
		return name, a
	}
	t.Run("completed while the landing delivery is unchanged", func(t *testing.T) {
		r := newRig(t)
		name, _ := crashAfterAuthorize(t, r, "c1")
		st := r.scan()
		if h := r.handoffs(); len(h) != 1 || h[0] != name {
			t.Fatalf("not completed: %v", h)
		}
		if e := r.entry(st, "c1"); e.State != StateDelivered || len(r.api.authorizations()) != 1 {
			t.Fatalf("delivered again: %+v %d", e, len(r.api.authorizations()))
		}
	})
	t.Run("discarded when the landing delivery changed", func(t *testing.T) {
		r := newRig(t)
		_, a := crashAfterAuthorize(t, r, "c2")
		time.Sleep(20 * time.Millisecond)
		r.put("c2.osm.pbf.complete", completion("c2", data)) // a new marker: a new delivery
		r.scan()
		var closed bool
		for _, x := range r.api.authorizations() {
			if x.ID == a.ID {
				closed = x.RevokedAt != nil
			}
		}
		if !closed {
			t.Fatal("the interrupted handoff's authorization stays open")
		}
		// The new delivery itself is then handed off normally.
		if h := r.handoffs(); len(h) != 1 || strings.HasPrefix(h[0], "fixture-w20260101") {
			t.Fatalf("handoffs %v", h)
		}
	})
	t.Run("discarded when the copy is not the authorized bytes", func(t *testing.T) {
		r := newRig(t)
		name, a := crashAfterAuthorize(t, r, "c3")
		_, _, _, hidden, _ := r.w.h.paths(name)
		bad := append([]byte(nil), data...)
		bad[100] ^= 1
		if err := os.WriteFile(hidden, bad, 0o644); err != nil {
			t.Fatal(err)
		}
		r.w.state.Consumed = append(r.w.state.Consumed, Consumed{Fingerprint: r.w.state.Handoffs[0].LandingFingerprint, Name: "c3", Outcome: StateDelivered})
		r.scan()
		if exists(hidden) || len(r.handoffs()) != 0 {
			t.Fatal("a copy that is not the authorized bytes was kept")
		}
		for _, x := range r.api.authorizations() {
			if x.ID == a.ID && x.RevokedAt == nil {
				t.Fatal("its authorization stays open")
			}
		}
	})
	t.Run("an orphan authorization is closed", func(t *testing.T) {
		r := newRig(t)
		a := r.api.addAuthorization("fixture-w20260101T000000Z-000000000000", strings.Repeat("0", 64), 10)
		r.scan()
		for _, x := range r.api.authorizations() {
			if x.ID == a.ID && x.RevokedAt == nil {
				t.Fatal("orphan authorization stays open")
			}
		}
	})
	t.Run("temporary copies are swept", func(t *testing.T) {
		r := newRig(t)
		p := filepath.Join(r.handoff, ".part-w-0011223344556677.osm.pbf")
		if err := os.WriteFile(p, data[:100], 0o600); err != nil {
			t.Fatal(err)
		}
		r.scan()
		if exists(p) {
			t.Fatal("a temporary copy survived")
		}
	})
}

func TestWatcherRefusesAnUnreadableState(t *testing.T) {
	r := newRig(t)
	p := filepath.Join(r.handoff, StateDirName, StateFileName)
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWatcher(r.w.cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("started from an unreadable state")
	}
}

func TestSubmit(t *testing.T) {
	r := newRig(t)
	data := fixtureA(t)
	share := filepath.Join(r.root, "share")
	_ = os.Mkdir(share, 0o777) // an untrusted transfer space is fine for the command
	file := filepath.Join(share, "iran.osm.pbf")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := NewHandoffer(HandoffConfig{Dir: r.handoff, RegionPath: repoFile(t, "config/regions/fixture.json"), Client: r.w.h.cfg.Client,
		Letter: LetterSubmit, MaxInputBytes: 1 << 30, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	for name, o := range map[string]SubmitOptions{
		"no expectation":       {File: file},
		"attest without why":   {File: file, Attest: true},
		"two expectations":     {File: file, ExpectSHA256: digest(data), Attest: true, Reason: "x"},
		"size without digest":  {File: file, ExpectSize: 5},
		"malformed digest":     {File: file, ExpectSHA256: "abc"},
		"wrong digest":         {File: file, ExpectSHA256: strings.Repeat("0", 64)},
		"wrong size":           {File: file, ExpectSHA256: digest(data), ExpectSize: 5},
		"marker of other file": {File: file, ExpectFile: writeTmp(t, share, "m.complete", completion("other", data))},
	} {
		if _, err := Submit(ctx, h, o, log, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if n := len(r.api.authorizations()); n != 0 {
		t.Fatalf("%d authorizations for refused submissions", n)
	}
	res, err := Submit(ctx, h, SubmitOptions{File: file, ExpectFile: writeTmp(t, share, "iran.complete", completion("iran", data))}, log, nil)
	if err != nil || !strings.HasPrefix(res.Name, "fixture-c") || res.SHA256 != digest(data) {
		t.Fatalf("%+v %v", res, err)
	}
	// Waiting for the outcome: the handoff is cleaned up when it is final.
	go func() {
		time.Sleep(300 * time.Millisecond)
		names := r.handoffs()
		for _, n := range names {
			r.api.finish(n, "published", "")
		}
	}()
	res, err = Submit(ctx, h, SubmitOptions{File: file, Attest: true, Reason: "copied from the USB stick, checked by eye", Wait: true,
		Poll: 100 * time.Millisecond}, log, nil)
	if err != nil || res.Submission == nil || res.Submission.State != "published" {
		t.Fatalf("wait: %+v %v", res, err)
	}
}

func writeTmp(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
