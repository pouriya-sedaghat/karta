package region

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDraft(t *testing.T) {
	// The box the Chitgar sidecar records for the Iran source file
	// (a claim, used here only as a realistic shape).
	box := [4]float64{44.023033, 24.039475, 63.35413, 39.790447}
	digest := strings.Repeat("ab", 32)
	c, err := Draft("iran", "Iran", box, digest, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.BBox != box || !SameBBox(c.BBox, box) || c.Source.ExpectedSHA256 != digest || c.Source.RequireProvenance {
		t.Fatalf("%+v", c)
	}
	if c.View.Center[0] < box[0] || c.View.Center[0] > box[2] || c.View.Zoom != 4 {
		t.Errorf("view %+v", c.View)
	}
	b, _ := json.Marshal(c)
	if !strings.Contains(string(b), `"validation":{"min_counts":{},"search":[],"tiles":[]}`) {
		t.Errorf("validation is not left empty: %s", b)
	}
	// The draft round-trips through the strict loader.
	var back Config
	if err := json.Unmarshal(b, &back); err != nil || back.Validate() != nil {
		t.Fatalf("%v %v", err, back.Validate())
	}
	// A small box gets a closer view; invalid input is refused.
	if c, _ := Draft("chitgar", "Chitgar", [4]float64{51.175, 35.705, 51.285, 35.785}, digest, true); c.View.Zoom != 11 {
		t.Errorf("chitgar zoom %v", c.View.Zoom)
	}
	if _, err := Draft("Iran!", "Iran", box, digest, false); err == nil {
		t.Error("bad id accepted")
	}
	if _, err := Draft("iran", "Iran", [4]float64{10, 10, 5, 5}, digest, false); err == nil {
		t.Error("bad box accepted")
	}
	if _, err := Draft("iran", "Iran", box, "", false); err == nil {
		t.Error("missing digest accepted")
	}
}
