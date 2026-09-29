package toolchain

import (
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	rec := map[string]string{"osm2pgsql": "1.11.0", "postgresql": "18.6", "postgis": "3.6.4", "geos": "3.14.1-CAPI-1.20.5",
		"proj": "9.8.1", "pg_trgm": "1.6", "icu": "153.128"}
	run := map[string]string{}
	for k, v := range rec {
		if k != "osm2pgsql" {
			run[k] = v
		}
	}
	if err := Compare(rec, run); err != nil {
		t.Fatalf("identical serving toolchain rejected: %v", err)
	}
	run["geos"] = "3.14.2-CAPI-1.20.6"
	run["icu"] = "154.1"
	err := Compare(rec, run)
	if err == nil || !strings.Contains(err.Error(), "geos 3.14.1-CAPI-1.20.5 at import, 3.14.2-CAPI-1.20.6 now") || !strings.Contains(err.Error(), "icu") {
		t.Fatalf("drift not reported: %v", err)
	}
	delete(rec, "proj")
	if err := Compare(rec, run); err == nil || !strings.Contains(err.Error(), "proj not recorded") {
		t.Fatalf("missing component not reported: %v", err)
	}
}
