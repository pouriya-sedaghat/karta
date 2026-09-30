package pbfwrite

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The committed snapshots are exactly the conversion of their XML sources,
// so CI imports bytes anyone can regenerate and review.
func TestCommittedSnapshotsAreReproducible(t *testing.T) {
	for _, s := range []struct{ src, out string }{
		{"karta-fixture.osm", "snapshots/karta-fixture-a.osm.pbf"},
		{"karta-fixture-b.osm", "snapshots/karta-fixture-b.osm.pbf"},
	} {
		f, err := os.Open(filepath.Join("../../testdata/fixture", s.src))
		if err != nil {
			t.Fatal(err)
		}
		d, err := ParseXML(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		got, err := Encode(d, Options{})
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join("../../testdata/fixture", s.out))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is not the conversion of %s; run go run ./cmd/karta-fixture", s.out, s.src)
		}
		again, _ := Encode(d, Options{})
		if !bytes.Equal(got, again) {
			t.Errorf("%s: encoding is not deterministic", s.src)
		}
	}
}

func TestEncodeRejectsEmptySnapshot(t *testing.T) {
	if _, err := Encode(&Data{}, Options{}); err == nil {
		t.Fatal("empty snapshot encoded")
	}
}
