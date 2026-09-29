package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	fixtureSnapshot = "../../testdata/fixture/karta-fixture.osm"
	fixtureRegion   = "../../config/regions/fixture.json"
)

// runInput runs the import far enough to verify its input. Options carry no
// database and no osm2pgsql, so a run that got past input verification fails
// with a different error instead of importing anything.
func runInput(t *testing.T, snapshot, provenance string) error {
	t.Helper()
	_, err := Run(context.Background(), Options{
		SnapshotPath: snapshot, RegionPath: fixtureRegion, ProvenancePath: provenance, MaxInputBytes: 1 << 30,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("import succeeded without a database")
	}
	return err
}

// A provenance sidecar whose box has NaN or an infinity in any coordinate is
// rejected as input, before any database is touched, instead of being taken
// as matching the region.
func TestNonFiniteProvenanceBoxIsRejectedAsInput(t *testing.T) {
	b, err := os.ReadFile(fixtureSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	for i, name := range []string{"west", "south", "east", "north"} {
		for _, bad := range []string{"NaN", "Inf", "-Infinity"} {
			box := []string{"0", "0", "0.02", "0.015"}
			box[i] = bad
			p := filepath.Join(t.TempDir(), "fixture.provenance.json")
			doc := fmt.Sprintf(`{"output_sha256":%q,"output_fileinfo":{"file":{"size":%d}},"bbox_wgs84":%q}`,
				hex.EncodeToString(sum[:]), len(b), strings.Join(box, ","))
			if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			err := runInput(t, fixtureSnapshot, p)
			if !errors.Is(err, ErrInput) || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "not a finite number") {
				t.Errorf("bbox_wgs84 %s = %s: %v; want an input error naming %s", name, bad, err, name)
			}
		}
	}
}

// The same holds for a snapshot header box (OSM XML <bounds> goes through
// strconv.ParseFloat too): a non-finite coordinate never matches the region.
func TestNonFiniteHeaderBoxIsRejectedAsInput(t *testing.T) {
	for _, attr := range []string{"minlon", "minlat", "maxlon", "maxlat"} {
		bounds := map[string]string{"minlon": "0", "minlat": "0", "maxlon": "0.02", "maxlat": "0.015"}
		bounds[attr] = "NaN"
		p := filepath.Join(t.TempDir(), "header.osm")
		doc := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<osm version="0.6" generator="test" timestamp="2026-01-01T00:00:00Z">
  <bounds minlat=%q minlon=%q maxlat=%q maxlon=%q/>
  <node id="1" lat="0.001" lon="0.001" version="1"/>
</osm>
`, bounds["minlat"], bounds["minlon"], bounds["maxlat"], bounds["maxlon"])
		if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		err := runInput(t, p, "")
		if !errors.Is(err, ErrInput) || !strings.Contains(err.Error(), "header box") {
			t.Errorf("<bounds %s=NaN>: %v; want an input error for the header box", attr, err)
		}
	}
}
