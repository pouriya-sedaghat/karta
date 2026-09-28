// Package mvt reads the layer structure of a Mapbox Vector Tile: layer
// names, extents, feature counts and property keys. The importer uses it to
// verify tiles before a release is accepted; geometry is not decoded.
package mvt

import (
	"errors"
	"math"
	"sort"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/pouriya-sedaghat/karta/internal/pb"
)

// Layer summarises one tile layer.
type Layer struct {
	Name     string
	Version  uint64
	Extent   uint64
	Features int
	Keys     []string
	// GeometryTypes counts features by type: 1 point, 2 line, 3 polygon.
	GeometryTypes map[uint64]int
}

// Layers decodes the layers of an uncompressed tile, sorted by name.
func Layers(tile []byte) ([]Layer, error) {
	var out []Layer
	err := pb.Walk(tile, func(f pb.Field) error {
		if f.Num != 3 || f.Type != protowire.BytesType {
			return nil
		}
		l := Layer{Version: 1, Extent: 4096, GeometryTypes: map[uint64]int{}}
		if err := pb.Walk(f.Bytes, func(lf pb.Field) error {
			switch {
			case lf.Num == 1 && lf.Type == protowire.BytesType:
				l.Name = string(lf.Bytes)
			case lf.Num == 2 && lf.Type == protowire.BytesType:
				l.Features++
				return pb.Walk(lf.Bytes, func(ff pb.Field) error {
					if ff.Num == 3 && ff.Type == protowire.VarintType {
						l.GeometryTypes[ff.Varint]++
					}
					return nil
				})
			case lf.Num == 3 && lf.Type == protowire.BytesType:
				l.Keys = append(l.Keys, string(lf.Bytes))
			case lf.Num == 5 && lf.Type == protowire.VarintType:
				l.Extent = lf.Varint
			case lf.Num == 15 && lf.Type == protowire.VarintType:
				l.Version = lf.Varint
			}
			return nil
		}); err != nil {
			return err
		}
		if l.Name == "" {
			return errors.New("layer without a name")
		}
		out = append(out, l)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}

// TileXY returns the Web Mercator tile containing a WGS84 point at zoom z.
func TileXY(lon, lat float64, z int) (x, y int) {
	n := math.Exp2(float64(z))
	x = int(math.Floor((lon + 180) / 360 * n))
	latRad := lat * math.Pi / 180
	y = int(math.Floor((1 - math.Asinh(math.Tan(latRad))/math.Pi) / 2 * n))
	maxIdx := int(n) - 1
	return min(max(x, 0), maxIdx), min(max(y, 0), maxIdx)
}

// TileBounds returns the WGS84 west, south, east, north of tile z/x/y.
func TileBounds(z, x, y int) [4]float64 {
	n := math.Exp2(float64(z))
	lon := func(x int) float64 { return float64(x)/n*360 - 180 }
	lat := func(y int) float64 { return math.Atan(math.Sinh(math.Pi*(1-2*float64(y)/n))) * 180 / math.Pi }
	return [4]float64{lon(x), lat(y + 1), lon(x + 1), lat(y)}
}
