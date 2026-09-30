package importer

import (
	"strings"
	"testing"
)

func TestRelativeChecks(t *testing.T) {
	half := 0.5
	active := map[string]int64{"roads": 4, "pois": 16, "waterways": 0}

	if c := RelativeChecks(nil, "rA", active, map[string]int64{}); c != nil {
		t.Errorf("gate off: %+v", c)
	}
	if c := RelativeChecks(&half, "", nil, map[string]int64{"roads": 1}); c != nil {
		t.Errorf("no active release: %+v", c)
	}
	// An active release whose counts are unknown fails the gate; it is
	// never skipped.
	for _, unknown := range []map[string]int64{nil, {}} {
		c := RelativeChecks(&half, "rA", unknown, map[string]int64{"roads": 4})
		if len(c) != 1 || c[0].Passed || c[0].Name != "relative_counts_available" || !strings.Contains(c[0].Detail, "rA") {
			t.Errorf("unknown active counts %v: %+v", unknown, c)
		}
	}

	byName := func(c []Check) map[string]Check {
		m := map[string]Check{}
		for _, x := range c {
			m[x.Name] = x
		}
		return m
	}
	// At the bound: ceil(4 * 0.5) = 2 roads and 8 POIs pass.
	c := byName(RelativeChecks(&half, "rA", active, map[string]int64{"roads": 2, "pois": 8}))
	if len(c) != 2 || !c["relative_count_roads"].Passed || !c["relative_count_pois"].Passed {
		t.Errorf("at the bound: %+v", c)
	}
	if _, ok := c["relative_count_waterways"]; ok {
		t.Error("a table the active release has no rows in is checked")
	}
	// Below it, or missing from the candidate, fails.
	c = byName(RelativeChecks(&half, "rA", active, map[string]int64{"roads": 1}))
	if c["relative_count_roads"].Passed || c["relative_count_pois"].Passed {
		t.Errorf("below the bound: %+v", c)
	}
	if d := c["relative_count_pois"].Detail; !strings.Contains(d, "0 rows") || !strings.Contains(d, "minimum 8") || !strings.Contains(d, "rA has 16") {
		t.Errorf("detail %q", d)
	}
}
