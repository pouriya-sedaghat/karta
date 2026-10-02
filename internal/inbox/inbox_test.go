package inbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func put(t *testing.T, dir, name string, b []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func scanOne(t *testing.T, dir, name string) Entry {
	t.Helper()
	entries, err := Scan(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("%s not listed in %+v", name, entries)
	return Entry{}
}

var opts = Options{MaxSnapshotBytes: 1 << 20}

func TestScanGroupsSubmissionsAndIgnoresTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "b.osm.pbf", []byte("x"))
	put(t, dir, "a.osm.pbf", []byte("x"))
	put(t, dir, "a.osm.pbf.provenance.json", []byte("{}"))
	put(t, dir, "a.osm.pbf.ready", []byte(digest([]byte("x"))))
	put(t, dir, ".c.osm.pbf", []byte("in progress"))     // hidden temporary name
	put(t, dir, "d.osm.pbf.part", []byte("in progress")) // other temporary name
	put(t, dir, "notes.txt", []byte("unrelated"))
	entries, err := Scan(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "a" || entries[1].Name != "b" {
		t.Fatalf("entries %+v", entries)
	}
	a, b := entries[0], entries[1]
	if !a.Complete() || a.Sidecar == nil || a.Snapshot == nil || a.Problem != "" {
		t.Errorf("a %+v", a)
	}
	if b.Complete() || b.Waiting() != "waiting_for_ready_marker" {
		t.Errorf("b %+v waiting %q", b, b.Waiting())
	}
}

func TestScanFlagsSymlinksDirectoriesFIFOsAndBadNames(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "real.osm.pbf")
	put(t, filepath.Dir(target), "real.osm.pbf", []byte("x"))
	if err := os.Symlink(target, filepath.Join(dir, "link.osm.pbf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dir.osm.pbf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo.osm.pbf"), 0o644); err != nil {
		t.Fatal(err)
	}
	put(t, dir, "ok.osm.pbf", []byte("x"))
	if err := os.Symlink("/etc/hostname", filepath.Join(dir, "ok.osm.pbf.ready")); err != nil {
		t.Fatal(err)
	}
	put(t, dir, "bad name.osm.pbf", []byte("x"))
	want := map[string]string{"link": CodeSymlink, "dir": CodeNotRegular, "fifo": CodeNotRegular, "ok": CodeSymlink, "bad name": CodeInvalidName}
	entries, err := Scan(dir, 100) // must not block on the FIFO
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if want[e.Name] != e.Problem {
			t.Errorf("%s: problem %q, want %q", e.Name, e.Problem, want[e.Name])
		}
		if _, err := Stage(dir, e, t.TempDir(), "job", opts); err == nil || CodeOf(err) != want[e.Name] {
			t.Errorf("%s: stage err %v, want %s", e.Name, err, want[e.Name])
		}
	}
}

func TestScanRefusesAFloodedInbox(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		put(t, dir, strings.Repeat("x", i+1)+".osm.pbf", nil)
	}
	if _, err := Scan(dir, 4); err == nil {
		t.Fatal("scan of 5 entries with limit 4 succeeded")
	}
}

func TestFingerprintAndSettle(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "a.osm.pbf", []byte("x"))
	put(t, dir, "a.osm.pbf.ready", []byte(digest([]byte("x"))))
	e1 := scanOne(t, dir, "a")
	if e1.Fingerprint() != scanOne(t, dir, "a").Fingerprint() {
		t.Fatal("fingerprint of unchanged files differs")
	}
	s := NewSettler()
	now := time.Unix(1000, 0)
	if s.Stable(e1, now, 5*time.Second) || s.Stable(e1, now.Add(4*time.Second), 5*time.Second) {
		t.Fatal("stable before the settle interval")
	}
	if !s.Stable(e1, now.Add(5*time.Second), 5*time.Second) {
		t.Fatal("not stable after the settle interval")
	}
	// Touching the marker (a re-submission) is a new fingerprint and restarts settling.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "a.osm.pbf.ready"), later, later); err != nil {
		t.Fatal(err)
	}
	e2 := scanOne(t, dir, "a")
	if e2.Fingerprint() == e1.Fingerprint() {
		t.Fatal("touching the marker kept the fingerprint")
	}
	if s.Stable(e2, now.Add(6*time.Second), 5*time.Second) {
		t.Fatal("a changed submission counted as stable")
	}
	s.Forget(nil)
	if len(s.seen) != 0 {
		t.Fatal("Forget kept entries")
	}
}

func TestParseMarker(t *testing.T) {
	d := strings.Repeat("ab", 32)
	for content, ok := range map[string]bool{
		d:                         true,
		d + "\n":                  true,
		strings.ToUpper(d) + "\n": true,
		d + "  a.osm.pbf\n":       true,
		d + " *a.osm.pbf\n":       true,
		d + "  b.osm.pbf\n":       false,
		d[:63]:                    false,
		"sha256:" + d:             false,
		d + d:                     false,
		"":                        false,
		d + "  a.osm.pbf extra\n": false,
	} {
		got, err := ParseMarker([]byte(content), "a.osm.pbf")
		if ok != (err == nil) || (ok && got != d) {
			t.Errorf("%q: %q %v (want ok=%v)", content, got, err, ok)
		}
	}
}

func TestStageCopiesVerifiesAndCleansUp(t *testing.T) {
	dir, staging := t.TempDir(), t.TempDir()
	data := []byte("snapshot bytes")
	put(t, dir, "a.osm.pbf", data)
	put(t, dir, "a.osm.pbf.provenance.json", []byte(`{"k":1}`))
	put(t, dir, "a.osm.pbf.ready", []byte(digest(data)+"  a.osm.pbf\n"))
	st, err := Stage(dir, scanOne(t, dir, "a"), staging, "job1", opts)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(st.SnapshotPath)
	side, _ := os.ReadFile(st.SidecarPath)
	if string(got) != string(data) || string(side) != `{"k":1}` || st.SHA256 != digest(data) || st.Size != int64(len(data)) {
		t.Fatalf("staged %+v", st)
	}
	fi, _ := os.Stat(st.Dir)
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("staging dir mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(st.SnapshotPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("staged file mode %v", fi.Mode().Perm())
	}
	// The same job id cannot be staged twice (no reuse of leftovers).
	if _, err := Stage(dir, scanOne(t, dir, "a"), staging, "job1", opts); err == nil {
		t.Fatal("staging over an existing job directory succeeded")
	}
	removed, err := CleanStaging(staging, nil)
	if err != nil || len(removed) != 1 {
		t.Fatalf("clean %v %v", removed, err)
	}
}

func TestStageRejections(t *testing.T) {
	data := []byte("snapshot bytes")
	cases := map[string]struct {
		setup func(t *testing.T, dir string) Entry
		opts  Options
		code  string
	}{
		"marker digest differs": {func(t *testing.T, dir string) Entry {
			put(t, dir, "a.osm.pbf", data)
			put(t, dir, "a.osm.pbf.ready", []byte(digest([]byte("other"))))
			return scanOne(t, dir, "a")
		}, opts, CodeDigestMismatch},
		"marker is not a digest": {func(t *testing.T, dir string) Entry {
			put(t, dir, "a.osm.pbf", data)
			put(t, dir, "a.osm.pbf.ready", []byte("done\n"))
			return scanOne(t, dir, "a")
		}, opts, CodeInvalidMarker},
		"snapshot grew after listing": {func(t *testing.T, dir string) Entry {
			put(t, dir, "a.osm.pbf", data)
			put(t, dir, "a.osm.pbf.ready", []byte(digest(data)))
			e := scanOne(t, dir, "a")
			f, _ := os.OpenFile(filepath.Join(dir, "a.osm.pbf"), os.O_APPEND|os.O_WRONLY, 0)
			f.Write([]byte("more"))
			f.Close()
			return e
		}, opts, CodeChanged},
		"snapshot replaced by a symlink after listing": {func(t *testing.T, dir string) Entry {
			put(t, dir, "a.osm.pbf", data)
			put(t, dir, "a.osm.pbf.ready", []byte(digest(data)))
			e := scanOne(t, dir, "a")
			other := filepath.Join(t.TempDir(), "x")
			os.WriteFile(other, data, 0o644)
			os.Remove(filepath.Join(dir, "a.osm.pbf"))
			os.Symlink(other, filepath.Join(dir, "a.osm.pbf"))
			return e
		}, opts, CodeSymlink},
		"snapshot replaced by another file after listing": {func(t *testing.T, dir string) Entry {
			put(t, dir, "a.osm.pbf", data)
			put(t, dir, "a.osm.pbf.ready", []byte(digest(data)))
			e := scanOne(t, dir, "a")
			os.Rename(filepath.Join(dir, "a.osm.pbf"), filepath.Join(dir, "old"))
			put(t, dir, "a.osm.pbf", data) // same bytes, new inode
			return e
		}, opts, CodeChanged},
		"too large": {func(t *testing.T, dir string) Entry {
			put(t, dir, "a.osm.pbf", data)
			put(t, dir, "a.osm.pbf.ready", []byte(digest(data)))
			return scanOne(t, dir, "a")
		}, Options{MaxSnapshotBytes: 4}, CodeTooLarge},
		"empty": {func(t *testing.T, dir string) Entry {
			put(t, dir, "a.osm.pbf", nil)
			put(t, dir, "a.osm.pbf.ready", []byte(digest(nil)))
			return scanOne(t, dir, "a")
		}, opts, CodeEmpty},
		"no room in staging": {func(t *testing.T, dir string) Entry {
			put(t, dir, "a.osm.pbf", data)
			put(t, dir, "a.osm.pbf.ready", []byte(digest(data)))
			return scanOne(t, dir, "a")
		}, Options{MaxSnapshotBytes: 1 << 20, ReserveBytes: 1 << 62}, CodeInsufficientSpace},
		"oversized marker": {func(t *testing.T, dir string) Entry {
			put(t, dir, "a.osm.pbf", data)
			put(t, dir, "a.osm.pbf.ready", []byte(strings.Repeat(" ", MaxMarkerBytes+1)))
			return scanOne(t, dir, "a")
		}, opts, CodeInvalidMarker},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir, staging := t.TempDir(), t.TempDir()
			e := c.setup(t, dir)
			_, err := Stage(dir, e, staging, "job", c.opts)
			if CodeOf(err) != c.code {
				t.Fatalf("err %v, want code %s", err, c.code)
			}
			if left, _ := os.ReadDir(staging); len(left) != 0 {
				t.Errorf("staging not cleaned after rejection: %v", left)
			}
		})
	}
}

func TestStageFile(t *testing.T) {
	dir, staging := t.TempDir(), t.TempDir()
	put(t, dir, "x.osm", []byte("<osm/>"))
	put(t, dir, "x.osm.provenance.json", []byte("{}"))
	st, err := StageFile(filepath.Join(dir, "x.osm"), filepath.Join(dir, "x.osm.provenance.json"), staging, "cli1", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(st.SnapshotPath, "snapshot.osm") || st.SidecarPath != st.SnapshotPath+".provenance.json" || st.SHA256 != digest([]byte("<osm/>")) {
		t.Fatalf("%+v", st)
	}
	if err := os.Symlink(filepath.Join(dir, "x.osm"), filepath.Join(dir, "y.osm.pbf")); err != nil {
		t.Fatal(err)
	}
	if _, err := StageFile(filepath.Join(dir, "y.osm.pbf"), "", staging, "cli2", opts); CodeOf(err) != CodeSymlink {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := StageFile(filepath.Join(dir, "x.txt"), "", staging, "cli3", opts); CodeOf(err) != CodeInvalidName {
		t.Fatalf("name: %v", err)
	}
}

func TestSignedManifestsOnlyInOnlineScans(t *testing.T) {
	dir := t.TempDir()
	snap := []byte("snapshot bytes")
	put(t, dir, "d.osm.pbf", snap)
	put(t, dir, "d.osm.pbf.manifest.json", []byte(`{"signed":true}`))
	put(t, dir, "d.osm.pbf.ready", []byte(digest(snap)))
	// The manual inbox ignores manifest files: the entry and its fingerprint
	// are exactly what Stage 2 recorded (Manifest nil).
	manual := scanOne(t, dir, "d")
	if manual.Manifest != nil {
		t.Fatal("the manual inbox listed a manifest")
	}
	h := sha256.New()
	h.Write([]byte("karta-inbox/1\x00d\x00" + manual.Snapshot.key() + "\x00-\x00" + manual.Marker.key()))
	if manual.Fingerprint() != hex.EncodeToString(h.Sum(nil)) {
		t.Fatal("the inbox fingerprint format changed")
	}
	entries, err := ScanWith(dir, 100, ScanOptions{Manifests: true})
	if err != nil || len(entries) != 1 || entries[0].Manifest == nil {
		t.Fatalf("online scan: %v %+v", err, entries)
	}
	if entries[0].Fingerprint() == manual.Fingerprint() {
		t.Error("the manifest is not part of an online delivery's fingerprint")
	}
	st, err := Stage(dir, entries[0], t.TempDir(), "job", Options{MaxSnapshotBytes: 1 << 20, RequireManifest: true})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(st.ManifestPath); string(b) != `{"signed":true}` {
		t.Errorf("staged manifest %q", b)
	}
	// Without a manifest, an online delivery is refused before anything is read.
	put(t, dir, "e.osm.pbf", snap)
	put(t, dir, "e.osm.pbf.ready", []byte(digest(snap)))
	entries, _ = ScanWith(dir, 100, ScanOptions{Manifests: true})
	for _, e := range entries {
		if e.Name != "e" {
			continue
		}
		if _, err := Stage(dir, e, t.TempDir(), "job2", Options{MaxSnapshotBytes: 1 << 20, RequireManifest: true}); CodeOf(err) != CodeManifestMissing {
			t.Errorf("no manifest: %v", err)
		}
	}
	// An oversized manifest is refused from its listed size.
	put(t, dir, "d.osm.pbf.manifest.json", []byte(strings.Repeat(" ", MaxManifestBytes+1)))
	entries, _ = ScanWith(dir, 100, ScanOptions{Manifests: true})
	if _, err := Stage(dir, entries[0], t.TempDir(), "job3", Options{MaxSnapshotBytes: 1 << 20, RequireManifest: true}); CodeOf(err) != CodeTooLarge {
		t.Errorf("oversized manifest: %v", err)
	}
}

func TestStageStopsWhenTheContextEnds(t *testing.T) {
	dir, staging := t.TempDir(), t.TempDir()
	data := []byte("snapshot bytes")
	put(t, dir, "a.osm.pbf", data)
	put(t, dir, "a.osm.pbf.ready", []byte(digest(data)+"  a.osm.pbf\n"))
	stop := errors.New("publication deadline")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(stop)
	if _, err := StageContext(ctx, dir, scanOne(t, dir, "a"), staging, "job1", opts); !errors.Is(err, stop) || CodeOf(err) != "" {
		t.Fatalf("staging after the context ended: %v (code %q)", err, CodeOf(err))
	}
	if entries, _ := os.ReadDir(staging); len(entries) != 0 {
		t.Errorf("the stopped staging left %v", entries)
	}
	if _, err := StageFileContext(ctx, filepath.Join(dir, "a.osm.pbf"), "", staging, "cli1", opts); !errors.Is(err, stop) {
		t.Fatalf("command-line staging after the context ended: %v", err)
	}
}
