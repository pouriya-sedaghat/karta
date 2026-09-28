package releaseid

import "testing"

func TestDerive(t *testing.T) {
	in := Inputs{
		SourceSHA256:   "7d0e69a2d5e1ad184ee48637626882bb8bcb7acd8e95123e05d0697ce216191e",
		RegionID:       "tehran-chitgar",
		RegionBBox:     [4]float64{51.175, 35.705, 51.285, 35.785},
		SchemaRevision: "s1-0000000000000000",
		StyleRevision:  "st1-0000000000000000",
	}
	id := Derive(in)
	if !Valid(id) || len(id) != 25 {
		t.Fatalf("id %q", id)
	}
	// Golden value: the derivation must never change silently, since ids are
	// persisted in the registry and in clients' pinned URLs.
	if id != "rc6a565261a31e1895bedc9c4" {
		t.Fatalf("Derive changed: got %s", id)
	}
	variants := []Inputs{in, in, in, in, in}
	variants[0].SourceSHA256 = "0" + in.SourceSHA256[1:]
	variants[1].RegionID = "tehran"
	variants[2].RegionBBox[0] = 51.1750001
	variants[3].SchemaRevision = "s1-0000000000000001"
	variants[4].StyleRevision = "st1-0000000000000001"
	for i, v := range variants {
		if Derive(v) == id {
			t.Errorf("variant %d produced the same id", i)
		}
	}
	if DatabaseName(id) != "karta_"+id {
		t.Error(DatabaseName(id))
	}
	for _, bad := range []string{"", "latest", "r123", "R0123456789abcdef01234567", "r0123456789abcdef0123456g", "r0123456789abcdef01234567 "} {
		if Valid(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
