package mvt

import (
	"math"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func TestTileMath(t *testing.T) {
	x, y := TileXY(51.2149, 35.7456, 14)
	if x != 10522 || y != 6448 {
		t.Fatalf("lake tile %d/%d", x, y)
	}
	b := TileBounds(14, x, y)
	if !(b[0] <= 51.2149 && 51.2149 <= b[2] && b[1] <= 35.7456 && 35.7456 <= b[3]) {
		t.Fatalf("bounds %v do not contain the point", b)
	}
	if w := TileBounds(0, 0, 0); math.Abs(w[0]+180) > 1e-9 || math.Abs(w[3]-85.0511287798) > 1e-6 {
		t.Fatalf("world %v", w)
	}
	if x, y := TileXY(200, 89, 3); x != 7 || y != 0 {
		t.Fatalf("clamped %d/%d", x, y)
	}
}

func TestLayers(t *testing.T) {
	var feat []byte
	feat = protowire.AppendVarint(protowire.AppendTag(feat, 3, protowire.VarintType), 1)
	var layer []byte
	layer = protowire.AppendString(protowire.AppendTag(layer, 1, protowire.BytesType), "pois")
	layer = protowire.AppendBytes(protowire.AppendTag(layer, 2, protowire.BytesType), feat)
	layer = protowire.AppendString(protowire.AppendTag(layer, 3, protowire.BytesType), "name")
	layer = protowire.AppendVarint(protowire.AppendTag(layer, 5, protowire.VarintType), 4096)
	layer = protowire.AppendVarint(protowire.AppendTag(layer, 15, protowire.VarintType), 2)
	tile := protowire.AppendBytes(protowire.AppendTag(nil, 3, protowire.BytesType), layer)
	ls, err := Layers(tile)
	if err != nil || len(ls) != 1 || ls[0].Name != "pois" || ls[0].Features != 1 || ls[0].Extent != 4096 || ls[0].Version != 2 || ls[0].GeometryTypes[1] != 1 || ls[0].Keys[0] != "name" {
		t.Fatalf("%+v %v", ls, err)
	}
	if _, err := Layers([]byte{0x1a, 0xff}); err == nil {
		t.Error("truncated tile accepted")
	}
	if ls, err := Layers(nil); err != nil || len(ls) != 0 {
		t.Errorf("empty tile: %v %v", ls, err)
	}
}
