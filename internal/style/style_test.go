package style

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pouriya-sedaghat/karta/internal/schema"
)

var fonts = []string{"Vazirmatn Bold", "Vazirmatn Regular"}

func TestTemplateMatchesCatalog(t *testing.T) {
	if err := Validate(Template(), schema.Layers(), fonts); err != nil {
		t.Fatal(err)
	}
	got, err := Fontstacks(Template())
	if err != nil || strings.Join(got, ",") != strings.Join(fonts, ",") {
		t.Fatalf("fontstacks %v %v", got, err)
	}
}

func mutate(t *testing.T, fn func(doc map[string]any)) json.RawMessage {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(Template(), &doc); err != nil {
		t.Fatal(err)
	}
	fn(doc)
	b, _ := json.Marshal(doc)
	return b
}

func layer(doc map[string]any, id string) map[string]any {
	for _, l := range doc["layers"].([]any) {
		if m := l.(map[string]any); m["id"] == id {
			return m
		}
	}
	panic(id)
}

func TestValidateRejectsMismatches(t *testing.T) {
	cases := map[string]struct {
		fn   func(doc map[string]any)
		want string
	}{
		"missing tile layer": {func(d map[string]any) { layer(d, "water")["source-layer"] = "ocean" }, `tile layer "ocean"`},
		"missing field": {func(d map[string]any) {
			layer(d, "water")["filter"] = []any{"==", []any{"get", "depth"}, 3}
		}, `field "depth"`},
		"unserved font": {func(d map[string]any) {
			layer(d, "poi-labels")["layout"].(map[string]any)["text-font"] = []any{"Noto Sans Regular"}
		}, `fontstack "Noto Sans Regular"`},
		"font expression": {func(d map[string]any) {
			layer(d, "poi-labels")["layout"].(map[string]any)["text-font"] = []any{"literal", []any{"Vazirmatn Regular"}}
		}, "literal array"},
		"external source": {func(d map[string]any) {
			d["sources"].(map[string]any)["osm"] = map[string]any{"type": "raster", "tiles": []any{"https://tile.openstreetmap.org/{z}/{x}/{y}.png"}}
		}, `unexpected source "osm"`},
		"duplicate id": {func(d map[string]any) {
			d["layers"] = append(d["layers"].([]any), layer(d, "water"))
		}, `duplicate layer id "water"`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := Validate(mutate(t, c.fn), schema.Layers(), fonts)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestRender(t *testing.T) {
	p := Params{
		ReleaseID: "r0123456789abcdef01234567", BaseURL: "http://localhost:8080",
		Bounds: [4]float64{51.175, 35.705, 51.285, 35.785}, Center: [2]float64{51.215, 35.745},
		Zoom: 13, MinZoom: 0, MaxZoom: 16, Attribution: "© OpenStreetMap contributors",
	}
	a, err := Render(Template(), p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Render(Template(), p)
	if !bytes.Equal(a, b) {
		t.Fatal("render is not deterministic")
	}
	var doc struct {
		Glyphs  string `json:"glyphs"`
		Sources map[string]struct {
			Tiles   []string `json:"tiles"`
			MaxZoom int      `json:"maxzoom"`
		} `json:"sources"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(a, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Glyphs != "http://localhost:8080/v1/fonts/{fontstack}/{range}.pbf" ||
		doc.Sources["karta"].Tiles[0] != "http://localhost:8080/v1/releases/r0123456789abcdef01234567/tiles/{z}/{x}/{y}.pbf" ||
		doc.Sources["karta"].MaxZoom != 16 || doc.Metadata["karta:release_id"] != p.ReleaseID {
		t.Fatalf("rendered %s", a)
	}
	// The attribution HTML is kept verbatim, not JSON-escaped (\u003c...).
	p.Attribution = `<a href="https://www.openstreetmap.org/copyright">© OpenStreetMap contributors</a>`
	html, _ := Render(Template(), p)
	if !bytes.Contains(html, []byte(`"attribution":"<a href=`)) || bytes.Contains(html, []byte("\\"+"u003c")) {
		t.Errorf("attribution escaped: %s", html)
	}
}
