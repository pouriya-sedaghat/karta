package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/osmfile"
	"github.com/pouriya-sedaghat/karta/internal/pbfwrite"
	"github.com/pouriya-sedaghat/karta/internal/provenance"
	"github.com/pouriya-sedaghat/karta/internal/region"
)

const (
	fixtureSnapshot = "../../testdata/fixture/karta-fixture.osm"
	fixtureRegion   = "../../config/regions/fixture.json"
)

func fixtureConfig(t *testing.T) region.Config {
	t.Helper()
	cfg, err := region.Load(fixtureRegion)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// runInput verifies an input against the fixture region, without any
// database, and returns the rejection.
func runInput(t *testing.T, snapshot, provenance string) error {
	t.Helper()
	_, err := Verify(context.Background(), VerifyOptions{
		SnapshotPath: snapshot, Region: fixtureConfig(t), ProvenancePath: provenance, MaxInputBytes: 1 << 30,
		MaxFutureSkew: time.Hour,
		Authorize:     func(context.Context, string, string, int64) (string, error) { return "test", nil },
	})
	if err == nil {
		t.Fatal("input accepted")
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

func TestCommittedSnapshotsVerify(t *testing.T) {
	for _, f := range []string{fixtureSnapshot, "../../testdata/fixture/snapshots/karta-fixture-a.osm.pbf", "../../testdata/fixture/snapshots/karta-fixture-b.osm.pbf"} {
		v, err := Verify(context.Background(), VerifyOptions{SnapshotPath: f, Region: fixtureConfig(t), MaxInputBytes: 1 << 20, MaxFutureSkew: time.Minute})
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if v.Source.AuthorizedBy != "region configuration" || v.Source.DataTimestampSource == "" {
			t.Errorf("%s: %+v", f, v.Source)
		}
	}
}

// A digest that is neither pinned nor authorized is refused, and the
// authorizer is consulted with the region, digest and size.
func TestUnpinnedDigestNeedsAnOperatorAuthorization(t *testing.T) {
	ts := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	b, err := pbfwrite.Encode(&pbfwrite.Data{BBox: &[4]float64{0, 0, 0.02, 0.015}, Timestamp: &ts,
		Nodes: []pbfwrite.Node{{ID: 1, Lat: 10, Lon: 10}}}, pbfwrite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "c.osm.pbf")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:])
	var asked []string
	opts := VerifyOptions{SnapshotPath: p, Region: fixtureConfig(t), MaxInputBytes: 1 << 20, MaxFutureSkew: time.Minute,
		Authorize: func(_ context.Context, region, d string, size int64) (string, error) {
			asked = append(asked, fmt.Sprintf("%s/%s/%d", region, d, size))
			return "", nil
		}}
	_, err = Verify(context.Background(), opts)
	if InputCode(err) != CodeUnauthorizedDigest || !strings.Contains(err.Error(), digest) {
		t.Fatalf("err %v", err)
	}
	if len(asked) != 1 || asked[0] != fmt.Sprintf("fixture/%s/%d", digest, len(b)) {
		t.Fatalf("authorizer asked %v", asked)
	}
	opts.Authorize = func(context.Context, string, string, int64) (string, error) { return "operator authorization 7", nil }
	v, err := Verify(context.Background(), opts)
	if err != nil || v.Source.AuthorizedBy != "operator authorization 7" {
		t.Fatalf("authorized digest: %v %+v", err, v)
	}
	opts.Authorize = nil
	if _, err := Verify(context.Background(), opts); InputCode(err) != CodeUnauthorizedDigest {
		t.Fatalf("no authorizer: %v", err)
	}
}

func TestTrustedTimestamp(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	at := func(s string) *time.Time { v, _ := time.Parse(time.RFC3339, s); return &v }
	prov := &provenance.Sidecar{}
	cases := []struct {
		name       string
		header     *time.Time
		prov       *provenance.Sidecar
		provTS     *time.Time
		code, from string
	}{
		{"pbf header", at("2026-09-27T20:23:36Z"), nil, nil, "", "pbf_header"},
		{"provenance wins, header absent", nil, prov, at("2026-09-27T20:23:36Z"), "", "provenance.source_fileinfo.header"},
		{"provenance and header agree", at("2026-09-27T20:23:36Z"), prov, at("2026-09-27T20:23:36Z"), "", "provenance.source_fileinfo.header"},
		{"provenance and header disagree", at("2026-09-20T00:00:00Z"), prov, at("2026-09-27T20:23:36Z"), CodeTimestampUntrusted, ""},
		{"no timestamp", nil, nil, nil, CodeTimestampMissing, ""},
		{"future", at("2026-09-29T13:00:00Z"), nil, nil, CodeTimestampUntrusted, ""},
		{"within skew", at("2026-09-29T12:04:00Z"), nil, nil, "", "pbf_header"},
		{"before OSM", at("1999-01-01T00:00:00Z"), nil, nil, CodeTimestampUntrusted, ""},
	}
	for _, c := range cases {
		info := osmfile.Info{Format: osmfile.FormatPBF, Timestamp: c.header}
		var pts time.Time
		if c.provTS != nil {
			pts = *c.provTS
		}
		_, from, err := trustedTimestamp(info, c.prov, pts, now, 5*time.Minute)
		if InputCode(err) != c.code || (c.code == "" && from != c.from) {
			t.Errorf("%s: from %q err %v; want code %q from %q", c.name, from, err, c.code, c.from)
		}
	}
}

// A snapshot of another extract is refused even if its digest is authorized.
func TestRegionMismatch(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.BBox = [4]float64{0, 0, 0.03, 0.02}
	cfg.View.Center = [2]float64{0.01, 0.01}
	_, err := Verify(context.Background(), VerifyOptions{SnapshotPath: "../../testdata/fixture/snapshots/karta-fixture-a.osm.pbf",
		Region: cfg, MaxInputBytes: 1 << 20, MaxFutureSkew: time.Minute})
	if InputCode(err) != CodeRegionMismatch {
		t.Fatalf("err %v", err)
	}
}

// Truncated and malformed files are malformed_snapshot, before any digest check.
func TestMalformedSnapshot(t *testing.T) {
	b, err := os.ReadFile("../../testdata/fixture/snapshots/karta-fixture-b.osm.pbf")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"truncated": b[:len(b)-100], "garbage": []byte("not a pbf at all"), "padded": append(append([]byte(nil), b...), 1)} {
		p := filepath.Join(t.TempDir(), "x.osm.pbf")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runInput(t, p, ""); InputCode(err) != CodeMalformed {
			t.Errorf("%s: %v", name, err)
		}
	}
}
