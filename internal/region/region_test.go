package region

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommittedRegions(t *testing.T) {
	for _, f := range []string{"../../config/regions/fixture.json", "../../config/regions/tehran-chitgar.json"} {
		c, err := Load(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if len(c.Validation.Search) == 0 || len(c.Validation.Tiles) == 0 || len(c.Validation.MinCounts) == 0 {
			t.Errorf("%s has no acceptance checks", f)
		}
	}
	c, _ := Load("../../config/regions/tehran-chitgar.json")
	if c.Source.ExpectedSHA256 != "7d0e69a2d5e1ad184ee48637626882bb8bcb7acd8e95123e05d0697ce216191e" || !c.Source.RequireProvenance {
		t.Errorf("Tehran region must pin the documented snapshot: %+v", c.Source)
	}
}

func TestRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"unknown field": `{"id":"x","name":"x","bbox":[0,0,1,1],"view":{"center":[0.5,0.5],"zoom":1},"validaton":{}}`,
		"bad bbox":      `{"id":"x","name":"x","bbox":[1,0,0,1],"view":{"center":[0.5,0.5],"zoom":1}}`,
		"bad id":        `{"id":"X Y","name":"x","bbox":[0,0,1,1],"view":{"center":[0.5,0.5],"zoom":1}}`,
		"bad digest":    `{"id":"x","name":"x","bbox":[0,0,1,1],"source":{"expected_sha256":"abc"},"view":{"center":[0.5,0.5],"zoom":1}}`,
		"view outside":  `{"id":"x","name":"x","bbox":[0,0,1,1],"view":{"center":[5,5],"zoom":1}}`,
		"unknown table": `{"id":"x","name":"x","bbox":[0,0,1,1],"view":{"center":[0.5,0.5],"zoom":1},"validation":{"min_counts":{"users":1}}}`,
		"bad search":    `{"id":"x","name":"x","bbox":[0,0,1,1],"view":{"center":[0.5,0.5],"zoom":1},"validation":{"search":[{"q":"a","osm_type":"area","osm_id":1,"max_position":1}]}}`,
		"zoom overflow": `{"id":"x","name":"x","bbox":[0,0,1,1],"view":{"center":[0.5,0.5],"zoom":1e400}}`,
		"bbox overflow": `{"id":"x","name":"x","bbox":[0,0,1e400,1],"view":{"center":[0.5,0.5],"zoom":1}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "r.json")
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestSameBBox(t *testing.T) {
	a := [4]float64{51.175, 35.705, 51.285, 35.785}
	if !SameBBox(a, [4]float64{51.17500001, 35.705, 51.285, 35.785}) || SameBBox(a, [4]float64{51.1751, 35.705, 51.285, 35.785}) {
		t.Error("tolerance")
	}
	if err := ValidBBox([4]float64{0, -86, 1, 1}); err == nil || !strings.Contains(err.Error(), "Mercator") {
		t.Errorf("err %v", err)
	}
}

// SameBBox decides whether a snapshot header or provenance box matches the
// region. A non-finite coordinate on either side, in any position, is no match;
// NaN in particular slips through a plain "difference > tolerance" test.
func TestSameBBoxRejectsNonFinite(t *testing.T) {
	good := [4]float64{51.175, 35.705, 51.285, 35.785}
	for i := range good {
		for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			b := good
			b[i] = bad
			if SameBBox(b, good) || SameBBox(good, b) || SameBBox(b, b) {
				t.Errorf("coordinate %d = %v matched", i, bad)
			}
		}
	}
	if !SameBBox(good, good) {
		t.Error("identical finite boxes do not match")
	}
}

// JSON cannot carry NaN or infinity, but Validate is also the check for
// configurations built in code; every range check must reject them.
func TestNonFiniteValuesRejected(t *testing.T) {
	good := func() Config {
		return Config{ID: "x", Name: "x", BBox: [4]float64{0, 0, 1, 1}, View: View{Center: [2]float64{0.5, 0.5}, Zoom: 1},
			Validation: Validation{Tiles: []TileCheck{{Lon: 0.5, Lat: 0.5, Zoom: 10, Layers: []string{"roads"}}}}}
	}
	if err := good().Validate(); err != nil {
		t.Fatal(err)
	}
	fields := map[string]func(*Config) *float64{
		"bbox west": func(c *Config) *float64 { return &c.BBox[0] }, "bbox north": func(c *Config) *float64 { return &c.BBox[3] },
		"center lon": func(c *Config) *float64 { return &c.View.Center[0] }, "center lat": func(c *Config) *float64 { return &c.View.Center[1] },
		"zoom":     func(c *Config) *float64 { return &c.View.Zoom },
		"tile lon": func(c *Config) *float64 { return &c.Validation.Tiles[0].Lon }, "tile lat": func(c *Config) *float64 { return &c.Validation.Tiles[0].Lat },
	}
	for name, field := range fields {
		for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			c := good()
			*field(&c) = v
			if err := c.Validate(); err == nil {
				t.Errorf("%s = %v accepted", name, v)
			}
		}
	}
}

// Identity carries the exact stored values the manifest and style publish.
func TestIdentityIsExact(t *testing.T) {
	c := Config{ID: "x", Name: "n", BBox: [4]float64{0, 0, 0.02000001, 0.015}, View: View{Center: [2]float64{0.01000001, 0.0075}, Zoom: 15.00000001}}
	r := c.Identity()
	if r.ID != c.ID || r.Name != c.Name || r.BBox != c.BBox || r.Center != c.View.Center || r.Zoom != c.View.Zoom {
		t.Errorf("identity %+v does not match %+v", r, c)
	}
}

// The committed fixture region pins exactly the committed fixture files, so a
// fixture edit that forgets the pin fails here, not in a Docker test.
func TestFixtureRegionPinsCommittedSnapshots(t *testing.T) {
	cfg, err := Load("../../config/regions/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	files := []string{
		"../../testdata/fixture/karta-fixture.osm",
		"../../testdata/fixture/snapshots/karta-fixture-a.osm.pbf",
		"../../testdata/fixture/snapshots/karta-fixture-b.osm.pbf",
	}
	var want []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		want = append(want, hex.EncodeToString(sum[:]))
	}
	got := cfg.Source.PinnedDigests()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("fixture.json pins %v; the committed files are %v", got, want)
	}
	if cfg.Source.Pinned(strings.Repeat("0", 64)) {
		t.Fatal("an unpinned digest is reported as pinned")
	}
}

func TestAllowedDigestsAreValidated(t *testing.T) {
	cfg, err := Load("../../config/regions/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	bad := cfg
	bad.Source.AllowedSHA256 = []string{"ABC"}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "allowed_sha256[0]") {
		t.Errorf("malformed digest: %v", err)
	}
	dup := cfg
	dup.Source.AllowedSHA256 = append([]string{}, cfg.Source.AllowedSHA256[0], cfg.Source.AllowedSHA256[0])
	if err := dup.Validate(); err == nil || !strings.Contains(err.Error(), "repeats") {
		t.Errorf("duplicate digest: %v", err)
	}
	drop := 1.0
	cfg.Validation.MaxDropFraction = &drop
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_drop_fraction") {
		t.Errorf("max_drop_fraction 1: %v", err)
	}
}

// Every region file the integration tests use must load, so a failing
// integration case fails for the reason it tests.
func TestTestdataRegionsLoad(t *testing.T) {
	files, err := filepath.Glob("../../testdata/regions/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no testdata regions: %v", err)
	}
	for _, f := range files {
		if _, err := Load(f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
