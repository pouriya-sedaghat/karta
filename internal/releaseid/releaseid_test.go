package releaseid

import (
	"errors"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"
)

func base() Inputs {
	return Inputs{
		SourceSHA256:        "7d0e69a2d5e1ad184ee48637626882bb8bcb7acd8e95123e05d0697ce216191e",
		ProvenanceSHA256:    "960feef8b3f2934983935d5e0309d7d1162ac526cb49b2b676640e4e1125286e",
		DataTimestamp:       time.Date(2026, 9, 27, 20, 23, 36, 0, time.UTC),
		DataTimestampSource: "provenance.source_fileinfo.header",
		Region: Region{
			ID: "tehran-chitgar", Name: "Chitgar Lake area, Tehran (development sample)",
			BBox: [4]float64{51.175, 35.705, 51.285, 35.785}, Center: [2]float64{51.215, 35.745}, Zoom: 13,
		},
		SchemaRevision: "s1-0000000000000000",
		StyleRevision:  "st1-0000000000000000",
		Attribution:    "© OpenStreetMap contributors",
		License:        "ODbL-1.0",
		LicenseURL:     "https://www.openstreetmap.org/copyright",
		Toolchain: map[string]string{
			"osm2pgsql": "1.11.0", "postgresql": "18.6", "postgis": "3.6.4",
			"geos": "3.14.1-CAPI-1.20.5", "proj": "9.8.1", "icu": "153.128", "pg_trgm": "1.6",
		},
	}
}

func derive(t *testing.T, in Inputs) string {
	t.Helper()
	id, canonical, err := Derive(in)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := Canonical(in); err != nil || c != canonical {
		t.Fatalf("Canonical disagrees with Derive: %v", err)
	}
	return id
}

func TestDeriveGolden(t *testing.T) {
	id, canonical, err := Derive(base())
	if err != nil || !Valid(id) || len(id) != 25 {
		t.Fatalf("id %q, err %v", id, err)
	}
	// Golden value: the derivation must never change silently, since ids are
	// persisted in the registry and in clients' pinned URLs.
	if id != "r89297836c5af940e06074fa0" {
		t.Fatalf("Derive changed: got %s\n%s", id, canonical)
	}
	if !strings.HasPrefix(canonical, "karta-release-id/3\n") {
		t.Fatal("canonical encoding is not versioned")
	}
	for _, want := range []string{`region.bbox="51.175,35.705,51.285,35.785"`, `region.view.center="51.215,35.745"`, `region.view.zoom="13"`} {
		if !strings.Contains(canonical, want) {
			t.Errorf("canonical lacks %s:\n%s", want, canonical)
		}
	}
	if DatabaseName(id) != "karta_"+id {
		t.Error(DatabaseName(id))
	}
}

// Every input that can change served bytes must change the identifier, so
// immutable release URLs are never reused for different content.
func TestEveryOutputAffectingInputChangesTheID(t *testing.T) {
	mutations := map[string]func(*Inputs){
		"source digest":         func(in *Inputs) { in.SourceSHA256 = "0" + in.SourceSHA256[1:] },
		"provenance digest":     func(in *Inputs) { in.ProvenanceSHA256 = "" },
		"data timestamp":        func(in *Inputs) { in.DataTimestamp = in.DataTimestamp.Add(time.Second) },
		"data timestamp source": func(in *Inputs) { in.DataTimestampSource = "pbf_header" },
		"region id":             func(in *Inputs) { in.Region.ID = "tehran" },
		"region name":           func(in *Inputs) { in.Region.Name += "!" },
		"region bbox":           func(in *Inputs) { in.Region.BBox[0] = 51.1750001 },
		"default view center":   func(in *Inputs) { in.Region.Center[1] = 35.7450001 },
		"default view zoom":     func(in *Inputs) { in.Region.Zoom = 14 },
		"schema revision":       func(in *Inputs) { in.SchemaRevision = "s1-0000000000000001" },
		"style revision":        func(in *Inputs) { in.StyleRevision = "st1-0000000000000001" },
		"attribution":           func(in *Inputs) { in.Attribution = "OSM" },
		"license":               func(in *Inputs) { in.License = "CC0" },
		"license url":           func(in *Inputs) { in.LicenseURL = "https://example.org" },
		"osm2pgsql version":     func(in *Inputs) { in.Toolchain["osm2pgsql"] = "2.0.0" },
		"postgresql version":    func(in *Inputs) { in.Toolchain["postgresql"] = "18.7" },
		"geos version":          func(in *Inputs) { in.Toolchain["geos"] = "3.14.2-CAPI-1.20.6" },
		"icu version":           func(in *Inputs) { in.Toolchain["icu"] = "154.1" },
		"new toolchain entry":   func(in *Inputs) { in.Toolchain["wagyu"] = "0.5.0" },
	}
	want := derive(t, base())
	seen := map[string]string{}
	for name, mutate := range mutations {
		in := base()
		mutate(&in)
		got := derive(t, in)
		if got == want {
			t.Errorf("changing %s did not change the release id", name)
		}
		if other, dup := seen[got]; dup {
			t.Errorf("%s and %s produced the same id", name, other)
		}
		seen[got] = name
	}
	// Map iteration order must not matter.
	a, b := base(), base()
	b.Toolchain = map[string]string{}
	for _, k := range []string{"pg_trgm", "icu", "proj", "geos", "postgis", "postgresql", "osm2pgsql"} {
		b.Toolchain[k] = a.Toolchain[k]
	}
	if derive(t, a) != derive(t, b) {
		t.Error("toolchain order changed the id")
	}
	// Field values cannot be spliced into one another.
	c, d := base(), base()
	c.Region.Name, c.Attribution = "x\nattribution=\"y\"", "z"
	d.Region.Name, d.Attribution = "x", "y\"\nattribution=\"z"
	if derive(t, c) == derive(t, d) {
		t.Error("canonical encoding is ambiguous")
	}
}

// floatFields are the region floats that the manifest and style publish.
var floatFields = map[string]func(*Region) *float64{
	"bbox west":  func(r *Region) *float64 { return &r.BBox[0] },
	"bbox south": func(r *Region) *float64 { return &r.BBox[1] },
	"bbox east":  func(r *Region) *float64 { return &r.BBox[2] },
	"bbox north": func(r *Region) *float64 { return &r.BBox[3] },
	"center lon": func(r *Region) *float64 { return &r.Center[0] },
	"center lat": func(r *Region) *float64 { return &r.Center[1] },
	"view zoom":  func(r *Region) *float64 { return &r.Zoom },
}

// Changes far below 1e-7 must change the id: the manifest and style publish
// these floats at full precision (the API test compares the served bytes),
// and version 2 of the encoding rounded them to 7 decimals.
func TestChangesBelow1e7ChangeTheID(t *testing.T) {
	steps := map[string]func(float64) float64{
		"+1e-8":         func(f float64) float64 { return f + 1e-8 },
		"-1e-8":         func(f float64) float64 { return f - 1e-8 },
		"next float up": func(f float64) float64 { return math.Nextafter(f, math.Inf(1)) },
		"next float down": func(f float64) float64 {
			return math.Nextafter(f, math.Inf(-1))
		},
	}
	want := derive(t, base())
	seen := map[string]string{want: "base"}
	for field, ptr := range floatFields {
		for step, apply := range steps {
			name := field + " " + step
			in := base()
			p := ptr(&in.Region)
			before := *p
			*p = apply(before)
			if d := math.Abs(*p - before); d == 0 || d >= 1e-7 {
				t.Fatalf("%s: change %g is not in (0, 1e-7)", name, d)
			}
			if strconv.FormatFloat(*p, 'f', 7, 64) != strconv.FormatFloat(before, 'f', 7, 64) {
				t.Fatalf("%s: not below the old 7-decimal rounding", name)
			}
			got := derive(t, in)
			if other, dup := seen[got]; dup {
				t.Errorf("%s gives the same id as %s", name, other)
			}
			seen[got] = name
		}
	}
	// The example from review: one release-pinned style URL for two zooms.
	a, b := base(), base()
	a.Region.Zoom, b.Region.Zoom = 13.00000001, 13.00000002
	if derive(t, a) == derive(t, b) {
		t.Error("zoom 13.00000001 and 13.00000002 share an id")
	}
	// encoding/json writes -0 and 0 differently, so they must not share an id.
	c, d := base(), base()
	c.Region.BBox[0], c.Region.Center[0] = -1, 0
	d.Region.BBox[0], d.Region.Center[0] = -1, math.Copysign(0, -1)
	if derive(t, c) == derive(t, d) {
		t.Error("center 0 and -0 share an id")
	}
}

func TestNonFiniteValuesAreRejected(t *testing.T) {
	for field, ptr := range floatFields {
		for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			in := base()
			*ptr(&in.Region) = v
			if _, _, err := Derive(in); !errors.Is(err, ErrNotFinite) {
				t.Errorf("%s = %v: err %v, want ErrNotFinite", field, v, err)
			}
			if _, err := Canonical(in); !errors.Is(err, ErrNotFinite) {
				t.Errorf("%s = %v: Canonical err %v", field, v, err)
			}
		}
	}
}

// The float encoding parses back to the identical float64 for every finite
// value, so distinct values never share an encoding.
func TestFloatEncodingIsLossless(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	check := func(f float64) {
		enc := nums([]float64{f})
		back, err := strconv.ParseFloat(enc, 64)
		if err != nil || math.Float64bits(back) != math.Float64bits(f) {
			t.Fatalf("%v (bits %x) encodes as %q, parses back as %v", f, math.Float64bits(f), enc, back)
		}
	}
	for _, f := range []float64{0, math.Copysign(0, -1), 1e-300, math.SmallestNonzeroFloat64, math.MaxFloat64, 13.00000001, 51.175, -85.05112878} {
		check(f)
	}
	for range 100000 {
		if f := math.Float64frombits(rng.Uint64()); !math.IsNaN(f) && !math.IsInf(f, 0) {
			check(f)
		}
		check(rng.Float64()*360 - 180)
		check(rng.Float64() * 22)
	}
}

func TestValid(t *testing.T) {
	for _, bad := range []string{"", "latest", "r123", "R0123456789abcdef01234567", "r0123456789abcdef0123456g", "r0123456789abcdef01234567 "} {
		if Valid(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
