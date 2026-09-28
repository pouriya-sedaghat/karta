package region

import (
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
