package provenance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	digestA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

const sidecar = `{"source":"/x/iran.osm.pbf","source_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","output_sha256":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","bbox_wgs84":"51.175,35.705,51.285,35.785",
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
	if err := s.Check(digestA, 42); err != nil {
		t.Errorf("matching file rejected: %v", err)
	}
	if err := s.Check(digestC, 42); err == nil || !strings.Contains(err.Error(), "output_sha256") {
		t.Errorf("digest mismatch: %v", err)
	}
	if err := s.Check(digestA, 43); err == nil || !strings.Contains(err.Error(), "size") {
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
	box, ts, err := s.Verify("7d0e69a2d5e1ad184ee48637626882bb8bcb7acd8e95123e05d0697ce216191e", 1149950)
	if err != nil {
		t.Fatal(err)
	}
	if box != [4]float64{51.175, 35.705, 51.285, 35.785} || ts.Format(time.RFC3339) != "2026-09-27T20:23:36Z" {
		t.Fatalf("box %v ts %v", box, ts)
	}
	if ts, err := s.SourceTimestamp(); err != nil || ts.Format(time.RFC3339) != "2026-09-27T20:23:36Z" {
		t.Fatalf("timestamp %v %v", ts, err)
	}
}

// strconv.ParseFloat accepts NaN and infinities; a sidecar box with one in any
// coordinate must be rejected, not passed on to the box comparison.
func TestBBoxRejectsNonFinite(t *testing.T) {
	good := [4]string{"51.175", "35.705", "51.285", "35.785"}
	for i, name := range bboxNames {
		for _, bad := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "infinity", "-Infinity"} {
			parts := good
			parts[i] = bad
			s := Sidecar{BBoxWGS84: strings.Join(parts[:], ",")}
			if b, err := s.BBox(); err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "finite") {
				t.Errorf("%s = %s: bbox %v, err %v; want an error naming %s", name, bad, b, err, name)
			}
		}
	}
	if _, err := (Sidecar{BBoxWGS84: strings.Join(good[:], ",")}).BBox(); err != nil {
		t.Errorf("finite box rejected: %v", err)
	}
}

// Every consistency rule Verify applies, one broken at a time.
func TestVerifyRejectsInconsistentSidecars(t *testing.T) {
	base := map[string]any{
		"source_sha256": strings.Repeat("a", 64), "output_sha256": digestA,
		"bbox_wgs84": "51.175,35.705,51.285,35.785", "strategy": "smart", "license": "ODbL 1.0",
		"source_fileinfo": map[string]any{
			"header": map[string]any{"option": map[string]any{"osmosis_replication_timestamp": "2026-09-27T20:23:36Z", "timestamp": "2026-09-27T20:23:36Z"}},
			"data":   map[string]any{"timestamp": map[string]any{"last": "2026-09-27T20:17:59Z"}},
		},
		"output_fileinfo": map[string]any{
			"file":   map[string]any{"size": 42},
			"header": map[string]any{"boxes": [][]float64{{51.175, 35.705, 51.285, 35.785}}},
			"data":   map[string]any{"timestamp": map[string]any{"last": "2026-09-27T14:34:58Z"}},
		},
	}
	encode := func(m map[string]any) []byte { b, _ := json.Marshal(m); return b }
	s, err := Parse(encode(base))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Verify(digestA, 42); err != nil {
		t.Fatalf("consistent sidecar rejected: %v", err)
	}
	clone := func() map[string]any { var m map[string]any; _ = json.Unmarshal(encode(base), &m); return m }
	cases := map[string]struct {
		edit func(m map[string]any)
		want string
	}{
		"missing size":        {func(m map[string]any) { delete(m["output_fileinfo"].(map[string]any), "file") }, "size"},
		"missing license":     {func(m map[string]any) { delete(m, "license") }, "license"},
		"other license":       {func(m map[string]any) { m["license"] = "CC-BY" }, "license"},
		"short output digest": {func(m map[string]any) { m["output_sha256"] = "abc" }, "not a SHA-256"},
		"bad source digest":   {func(m map[string]any) { m["source_sha256"] = "zz" }, "source_sha256"},
		"header box differs": {func(m map[string]any) {
			m["output_fileinfo"].(map[string]any)["header"] = map[string]any{"boxes": [][]float64{{51, 35, 52, 36}}}
		}, "header box"},
		"two header boxes": {func(m map[string]any) {
			m["output_fileinfo"].(map[string]any)["header"] = map[string]any{"boxes": [][]float64{{1, 2, 3, 4}, {1, 2, 3, 4}}}
		}, "exactly one box"},
		"no source timestamp": {func(m map[string]any) { m["source_fileinfo"].(map[string]any)["header"] = map[string]any{} }, "no source header timestamp"},
		"timestamps disagree": {func(m map[string]any) {
			m["source_fileinfo"].(map[string]any)["header"].(map[string]any)["option"].(map[string]any)["timestamp"] = "2026-09-28T00:00:00Z"
		}, "disagree"},
		"extract newer": {func(m map[string]any) {
			m["output_fileinfo"].(map[string]any)["data"] = map[string]any{"timestamp": map[string]any{"last": "2026-09-28T00:00:00Z"}}
		}, "after the source snapshot"},
		"source object newer": {func(m map[string]any) {
			m["source_fileinfo"].(map[string]any)["data"] = map[string]any{"timestamp": map[string]any{"last": "2026-10-01T00:00:00Z"}}
		}, "after the source snapshot"},
		"unknown strategy": {func(m map[string]any) { m["strategy"] = "magic" }, "strategy"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := clone()
			c.edit(m)
			s, err := Parse(encode(m))
			if err == nil {
				_, _, err = s.Verify(digestA, 42)
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestParseRejectsDuplicateKeysAndTrailingData(t *testing.T) {
	for doc, want := range map[string]string{
		`{"output_sha256":"a","output_sha256":"b"}`: "repeats key",
		`{"license":"ODbL 1.0"} {}`:                 "after its JSON object",
		`["not","an","object"]`:                     "not a JSON object",
	} {
		if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", doc, err, want)
		}
	}
}
