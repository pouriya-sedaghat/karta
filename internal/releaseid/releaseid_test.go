package releaseid

import (
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

func TestDeriveGolden(t *testing.T) {
	id := Derive(base())
	if !Valid(id) || len(id) != 25 {
		t.Fatalf("id %q", id)
	}
	// Golden value: the derivation must never change silently, since ids are
	// persisted in the registry and in clients' pinned URLs.
	if id != "re0d81db55037e521861ab1bb" {
		t.Fatalf("Derive changed: got %s\n%s", id, Canonical(base()))
	}
	if !strings.HasPrefix(Canonical(base()), "karta-release-id/2\n") {
		t.Fatal("canonical encoding is not versioned")
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
	want := Derive(base())
	seen := map[string]string{}
	for name, mutate := range mutations {
		in := base()
		mutate(&in)
		got := Derive(in)
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
	if Derive(a) != Derive(b) {
		t.Error("toolchain order changed the id")
	}
	// Field values cannot be spliced into one another.
	c, d := base(), base()
	c.Region.Name, c.Attribution = "x\nattribution=\"y\"", "z"
	d.Region.Name, d.Attribution = "x", "y\"\nattribution=\"z"
	if Derive(c) == Derive(d) {
		t.Error("canonical encoding is ambiguous")
	}
}

func TestValid(t *testing.T) {
	for _, bad := range []string{"", "latest", "r123", "R0123456789abcdef01234567", "r0123456789abcdef0123456g", "r0123456789abcdef01234567 "} {
		if Valid(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
