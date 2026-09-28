package provenance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sidecar = `{"source":"/x/iran.osm.pbf","source_sha256":"aa","output_sha256":"BB","bbox_wgs84":"51.175,35.705,51.285,35.785",
 "strategy":"smart","source_fileinfo":{"header":{"option":{"osmosis_replication_timestamp":"2026-09-27T20:23:36Z"}}},
 "output_fileinfo":{"file":{"size":42}},"license":"ODbL 1.0"}`

func TestLoadAndCheck(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.osm.pbf.provenance.json")
	if err := os.WriteFile(p, []byte(sidecar), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Check("bb", 42); err != nil {
		t.Errorf("matching file rejected: %v", err)
	}
	if err := s.Check("cc", 42); err == nil || !strings.Contains(err.Error(), "output_sha256") {
		t.Errorf("digest mismatch: %v", err)
	}
	if err := s.Check("bb", 43); err == nil || !strings.Contains(err.Error(), "size") {
		t.Errorf("size mismatch: %v", err)
	}
	b, err := s.BBox()
	if err != nil || b != [4]float64{51.175, 35.705, 51.285, 35.785} {
		t.Errorf("bbox %v %v", b, err)
	}
	ts, err := s.SourceTimestamp()
	if err != nil || !ts.Equal(time.Date(2026, 9, 27, 20, 23, 36, 0, time.UTC)) {
		t.Errorf("timestamp %v %v", ts, err)
	}
}

// The supplied Chitgar sidecar, when present.
func TestChitgarSidecar(t *testing.T) {
	s, err := Load("../../data/local/tehran-chitgar.osm.pbf.provenance.json")
	if err != nil {
		t.Skip("sidecar not present in data/local")
	}
	if err := s.Check("7d0e69a2d5e1ad184ee48637626882bb8bcb7acd8e95123e05d0697ce216191e", 1149950); err != nil {
		t.Fatal(err)
	}
	if ts, err := s.SourceTimestamp(); err != nil || ts.Format(time.RFC3339) != "2026-09-27T20:23:36Z" {
		t.Fatalf("timestamp %v %v", ts, err)
	}
}
