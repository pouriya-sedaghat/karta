package importer

import (
	"strings"
	"testing"

	"github.com/pouriya-sedaghat/karta/internal/region"
)

func TestPolicyOf(t *testing.T) {
	base, err := region.Load("../../config/regions/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Validation.MinCounts) == 0 || len(base.Validation.Search) == 0 || len(base.Validation.Tiles) == 0 {
		t.Fatal("the fixture region has no checks to vary")
	}
	want := PolicyOf(base)
	if len(want.SHA256) != 64 || !strings.Contains(string(want.Canonical), `"revision":"`+ValidationRevision+`"`) ||
		!strings.Contains(string(want.Canonical), `"region":{"id":"fixture","name":`) {
		t.Fatalf("policy %s %s", want.SHA256, want.Canonical)
	}
	clone := func() region.Config {
		c := base
		c.Validation.MinCounts = map[string]int64{}
		for k, v := range base.Validation.MinCounts {
			c.Validation.MinCounts[k] = v
		}
		c.Validation.Search = append([]region.SearchCheck(nil), base.Validation.Search...)
		c.Validation.Tiles = append([]region.TileCheck(nil), base.Validation.Tiles...)
		return c
	}

	// Admission and the relative gate are not the content policy: neither
	// changes it.
	same := map[string]func(*region.Config){
		"pinned digests": func(c *region.Config) {
			c.Source.AllowedSHA256 = append(c.Source.AllowedSHA256, strings.Repeat("0", 64))
		},
		"max_drop_fraction": func(c *region.Config) { f := 0.99; c.Validation.MaxDropFraction = &f },
	}
	for name, change := range same {
		c := clone()
		change(&c)
		if got := PolicyOf(c); got.SHA256 != want.SHA256 {
			t.Errorf("%s changed the content policy", name)
		}
	}

	// Every content check is part of it, and so is the region identity the
	// checks are evaluated for, exactly: a pass stands for that identity only.
	differ := map[string]func(*region.Config){
		"the region id":          func(c *region.Config) { c.ID = "other" },
		"the region name":        func(c *region.Config) { c.Name = "Another name" },
		"the box":                func(c *region.Config) { c.BBox[0] -= 1e-9 },
		"the view centre":        func(c *region.Config) { c.View.Center[1] += 1e-12 },
		"the view zoom":          func(c *region.Config) { c.View.Zoom += 0.001 },
		"a min_count":            func(c *region.Config) { c.Validation.MinCounts["roads"]++ },
		"a new min_count":        func(c *region.Config) { c.Validation.MinCounts["places_extra"] = 1 },
		"a search position":      func(c *region.Config) { c.Validation.Search[0].MaxPosition++ },
		"a search language":      func(c *region.Config) { c.Validation.Search[0].Lang = "xx" },
		"a removed search":       func(c *region.Config) { c.Validation.Search = c.Validation.Search[1:] },
		"a tile's layers":        func(c *region.Config) { c.Validation.Tiles[0].Layers = append(c.Validation.Tiles[0].Layers, "water") },
		"a tile's zoom":          func(c *region.Config) { c.Validation.Tiles[0].Zoom++ },
		"no checks at all":       func(c *region.Config) { c.Validation = region.Validation{} },
		"search order (kept)":    func(c *region.Config) { c.Validation.Search = append(c.Validation.Search, c.Validation.Search[0]) },
		"an empty min_counts":    func(c *region.Config) { c.Validation.MinCounts = map[string]int64{} },
		"a tile's position":      func(c *region.Config) { c.Validation.Tiles[0].Lon += 1e-9 },
		"a search's expectation": func(c *region.Config) { c.Validation.Search[0].OSMID++ },
	}
	seen := map[string]string{want.SHA256: "base"}
	for name, change := range differ {
		c := clone()
		change(&c)
		got := PolicyOf(c).SHA256
		if prev, dup := seen[got]; dup {
			t.Errorf("%s gives the same policy as %s", name, prev)
		}
		seen[got] = name
	}

	// Absent and empty checks are one policy (the drafted Iran region has
	// none), different from any policy with checks.
	empty := PolicyOf(region.Config{})
	if e2 := PolicyOf(region.Config{Validation: region.Validation{MinCounts: map[string]int64{}, Search: []region.SearchCheck{},
		Tiles: []region.TileCheck{}}}); e2.SHA256 != empty.SHA256 {
		t.Error("absent and empty checks differ")
	}
	if empty.SHA256 == want.SHA256 {
		t.Error("the empty policy equals the fixture's")
	}
}
