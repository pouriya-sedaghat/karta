// Package pbfwrite writes small OSM PBF files deterministically. It exists
// for Karta's committed test snapshots (testdata/fixture/snapshots) and for
// tests that need malformed or altered variants of them; it is not used on
// any serving or import path.
//
// The output depends only on the input: entities are sorted by type and id,
// tags by key, the string table is built in encounter order, and blobs are
// stored uncompressed ("raw") by default, so the bytes do not depend on a
// compressor implementation. Zlib blobs can be requested for tests.
package pbfwrite

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// Tag is one OSM key/value pair.
type Tag struct{ K, V string }

// Node is an OSM node; Lat and Lon are in units of 1e-7 degrees.
type Node struct {
	ID       int64
	Lat, Lon int64
	Tags     []Tag
}

// Way is an OSM way.
type Way struct {
	ID   int64
	Refs []int64
	Tags []Tag
}

// Member is a relation member; Type is node, way or relation.
type Member struct {
	Type string
	Ref  int64
	Role string
}

// Relation is an OSM relation.
type Relation struct {
	ID      int64
	Members []Member
	Tags    []Tag
}

// Data is a complete snapshot.
type Data struct {
	// BBox is west, south, east, north in degrees, or nil.
	BBox *[4]float64
	// Timestamp is written as osmosis_replication_timestamp, or omitted if nil.
	Timestamp      *time.Time
	WritingProgram string
	Nodes          []Node
	Ways           []Way
	Relations      []Relation
}

// Options control encoding.
type Options struct {
	// Zlib compresses data blobs (the header blob stays raw).
	Zlib bool
}

// ParseXML reads an OSM XML document (nodes, ways, relations, <bounds> and
// the <osm timestamp> attribute).
func ParseXML(r io.Reader) (*Data, error) {
	var doc struct {
		Timestamp string `xml:"timestamp,attr"`
		Generator string `xml:"generator,attr"`
		Bounds    *struct {
			MinLat string `xml:"minlat,attr"`
			MinLon string `xml:"minlon,attr"`
			MaxLat string `xml:"maxlat,attr"`
			MaxLon string `xml:"maxlon,attr"`
		} `xml:"bounds"`
		Nodes []struct {
			ID   int64    `xml:"id,attr"`
			Lat  string   `xml:"lat,attr"`
			Lon  string   `xml:"lon,attr"`
			Tags []xmlTag `xml:"tag"`
		} `xml:"node"`
		Ways []struct {
			ID   int64    `xml:"id,attr"`
			Nds  []xmlRef `xml:"nd"`
			Tags []xmlTag `xml:"tag"`
		} `xml:"way"`
		Relations []struct {
			ID      int64 `xml:"id,attr"`
			Members []struct {
				Type string `xml:"type,attr"`
				Ref  int64  `xml:"ref,attr"`
				Role string `xml:"role,attr"`
			} `xml:"member"`
			Tags []xmlTag `xml:"tag"`
		} `xml:"relation"`
	}
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, err
	}
	d := &Data{WritingProgram: doc.Generator}
	if doc.Timestamp != "" {
		t, err := time.Parse(time.RFC3339, doc.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("osm timestamp: %w", err)
		}
		t = t.UTC()
		d.Timestamp = &t
	}
	if b := doc.Bounds; b != nil {
		var box [4]float64
		for i, s := range []string{b.MinLon, b.MinLat, b.MaxLon, b.MaxLat} {
			v, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return nil, fmt.Errorf("bounds: %w", err)
			}
			box[i] = v
		}
		d.BBox = &box
	}
	for _, n := range doc.Nodes {
		lat, err := fixed(n.Lat)
		if err != nil {
			return nil, fmt.Errorf("node %d lat: %w", n.ID, err)
		}
		lon, err := fixed(n.Lon)
		if err != nil {
			return nil, fmt.Errorf("node %d lon: %w", n.ID, err)
		}
		d.Nodes = append(d.Nodes, Node{ID: n.ID, Lat: lat, Lon: lon, Tags: tags(n.Tags)})
	}
	for _, w := range doc.Ways {
		way := Way{ID: w.ID, Tags: tags(w.Tags)}
		for _, nd := range w.Nds {
			way.Refs = append(way.Refs, nd.Ref)
		}
		d.Ways = append(d.Ways, way)
	}
	for _, r := range doc.Relations {
		rel := Relation{ID: r.ID, Tags: tags(r.Tags)}
		for _, m := range r.Members {
			if m.Type != "node" && m.Type != "way" && m.Type != "relation" {
				return nil, fmt.Errorf("relation %d: member type %q", r.ID, m.Type)
			}
			rel.Members = append(rel.Members, Member{Type: m.Type, Ref: m.Ref, Role: m.Role})
		}
		d.Relations = append(d.Relations, rel)
	}
	return d, nil
}

type xmlTag struct {
	K string `xml:"k,attr"`
	V string `xml:"v,attr"`
}

type xmlRef struct {
	Ref int64 `xml:"ref,attr"`
}

func tags(in []xmlTag) []Tag {
	out := make([]Tag, len(in))
	for i, t := range in {
		out[i] = Tag{K: t.K, V: t.V}
	}
	return out
}

// fixed converts a decimal degree string to units of 1e-7 degrees.
func fixed(s string) (int64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v < -180 || v > 180 {
		return 0, fmt.Errorf("coordinate %q out of range", s)
	}
	return int64(math.Round(v * 1e7)), nil
}

// Encode writes d as an OSM PBF file.
func Encode(d *Data, opts Options) ([]byte, error) {
	var out bytes.Buffer
	if err := writeBlob(&out, "OSMHeader", headerBlock(d), false); err != nil {
		return nil, err
	}
	nodes := append([]Node(nil), d.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	ways := append([]Way(nil), d.Ways...)
	sort.Slice(ways, func(i, j int) bool { return ways[i].ID < ways[j].ID })
	rels := append([]Relation(nil), d.Relations...)
	sort.Slice(rels, func(i, j int) bool { return rels[i].ID < rels[j].ID })
	if len(nodes)+len(ways)+len(rels) == 0 {
		return nil, errors.New("snapshot has no objects")
	}
	if len(nodes) > 0 {
		if err := writeBlob(&out, "OSMData", nodeBlock(nodes), opts.Zlib); err != nil {
			return nil, err
		}
	}
	if len(ways) > 0 {
		if err := writeBlob(&out, "OSMData", wayBlock(ways), opts.Zlib); err != nil {
			return nil, err
		}
	}
	if len(rels) > 0 {
		if err := writeBlob(&out, "OSMData", relationBlock(rels), opts.Zlib); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

func headerBlock(d *Data) []byte {
	var b []byte
	if d.BBox != nil {
		var box []byte
		nano := func(v float64) uint64 { return protowire.EncodeZigZag(int64(math.Round(v * 1e9))) }
		box = protowire.AppendTag(box, 1, protowire.VarintType) // left
		box = protowire.AppendVarint(box, nano(d.BBox[0]))
		box = protowire.AppendTag(box, 2, protowire.VarintType) // right
		box = protowire.AppendVarint(box, nano(d.BBox[2]))
		box = protowire.AppendTag(box, 3, protowire.VarintType) // top
		box = protowire.AppendVarint(box, nano(d.BBox[3]))
		box = protowire.AppendTag(box, 4, protowire.VarintType) // bottom
		box = protowire.AppendVarint(box, nano(d.BBox[1]))
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendBytes(b, box)
	}
	for _, f := range []string{"OsmSchema-V0.6", "DenseNodes"} {
		b = protowire.AppendTag(b, 4, protowire.BytesType)
		b = protowire.AppendString(b, f)
	}
	b = protowire.AppendTag(b, 5, protowire.BytesType)
	b = protowire.AppendString(b, "Sort.Type_then_ID")
	prog := d.WritingProgram
	if prog == "" {
		prog = "karta-pbfwrite/1"
	}
	b = protowire.AppendTag(b, 16, protowire.BytesType)
	b = protowire.AppendString(b, prog)
	if d.Timestamp != nil {
		b = protowire.AppendTag(b, 32, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(d.Timestamp.Unix())) // #nosec G115 -- fixture timestamps are after 1970
	}
	return b
}

// stringTable collects strings in encounter order; index 0 is "".
type stringTable struct {
	list  []string
	index map[string]uint32
}

func newStringTable() *stringTable {
	return &stringTable{list: []string{""}, index: map[string]uint32{"": 0}}
}

func (s *stringTable) id(v string) uint32 {
	if i, ok := s.index[v]; ok {
		return i
	}
	i := uint32(len(s.list)) // #nosec G115 -- fixture string tables are tiny
	s.list = append(s.list, v)
	s.index[v] = i
	return i
}

func (s *stringTable) encode() []byte {
	var t []byte
	for _, v := range s.list {
		t = protowire.AppendTag(t, 1, protowire.BytesType)
		t = protowire.AppendString(t, v)
	}
	return t
}

func sortedTags(in []Tag) []Tag {
	out := append([]Tag(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].K < out[j].K })
	return out
}

func primitiveBlock(st *stringTable, group []byte) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendBytes(b, st.encode())
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	return protowire.AppendBytes(b, group)
}

func packedSint(vals []int64) []byte {
	var b []byte
	for _, v := range vals {
		b = protowire.AppendVarint(b, protowire.EncodeZigZag(v))
	}
	return b
}

func packedUint(vals []uint64) []byte {
	var b []byte
	for _, v := range vals {
		b = protowire.AppendVarint(b, v)
	}
	return b
}

func deltas(vals []int64) []int64 {
	out := make([]int64, len(vals))
	var prev int64
	for i, v := range vals {
		out[i] = v - prev
		prev = v
	}
	return out
}

func nodeBlock(nodes []Node) []byte {
	st := newStringTable()
	ids := make([]int64, len(nodes))
	lats := make([]int64, len(nodes))
	lons := make([]int64, len(nodes))
	var kv []uint64
	for i, n := range nodes {
		ids[i], lats[i], lons[i] = n.ID, n.Lat, n.Lon
		for _, t := range sortedTags(n.Tags) {
			kv = append(kv, uint64(st.id(t.K)), uint64(st.id(t.V)))
		}
		kv = append(kv, 0)
	}
	var dense []byte
	dense = protowire.AppendTag(dense, 1, protowire.BytesType)
	dense = protowire.AppendBytes(dense, packedSint(deltas(ids)))
	dense = protowire.AppendTag(dense, 8, protowire.BytesType)
	dense = protowire.AppendBytes(dense, packedSint(deltas(lats)))
	dense = protowire.AppendTag(dense, 9, protowire.BytesType)
	dense = protowire.AppendBytes(dense, packedSint(deltas(lons)))
	dense = protowire.AppendTag(dense, 10, protowire.BytesType)
	dense = protowire.AppendBytes(dense, packedUint(kv))
	var group []byte
	group = protowire.AppendTag(group, 2, protowire.BytesType)
	group = protowire.AppendBytes(group, dense)
	return primitiveBlock(st, group)
}

func appendKeysVals(b []byte, st *stringTable, in []Tag) []byte {
	ts := sortedTags(in)
	if len(ts) == 0 {
		return b
	}
	keys := make([]uint64, len(ts))
	vals := make([]uint64, len(ts))
	for i, t := range ts {
		keys[i], vals[i] = uint64(st.id(t.K)), uint64(st.id(t.V))
	}
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendBytes(b, packedUint(keys))
	b = protowire.AppendTag(b, 3, protowire.BytesType)
	return protowire.AppendBytes(b, packedUint(vals))
}

func wayBlock(ways []Way) []byte {
	st := newStringTable()
	var group []byte
	for _, w := range ways {
		var b []byte
		b = protowire.AppendTag(b, 1, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(w.ID)) // #nosec G115 -- OSM ids are positive
		b = appendKeysVals(b, st, w.Tags)
		b = protowire.AppendTag(b, 8, protowire.BytesType)
		b = protowire.AppendBytes(b, packedSint(deltas(w.Refs)))
		group = protowire.AppendTag(group, 3, protowire.BytesType)
		group = protowire.AppendBytes(group, b)
	}
	return primitiveBlock(st, group)
}

var memberTypes = map[string]uint64{"node": 0, "way": 1, "relation": 2}

func relationBlock(rels []Relation) []byte {
	st := newStringTable()
	var group []byte
	for _, r := range rels {
		var b []byte
		b = protowire.AppendTag(b, 1, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(r.ID)) // #nosec G115 -- OSM ids are positive
		b = appendKeysVals(b, st, r.Tags)
		roles := make([]uint64, len(r.Members))
		ids := make([]int64, len(r.Members))
		types := make([]uint64, len(r.Members))
		for i, m := range r.Members {
			roles[i], ids[i], types[i] = uint64(st.id(m.Role)), m.Ref, memberTypes[m.Type]
		}
		b = protowire.AppendTag(b, 8, protowire.BytesType)
		b = protowire.AppendBytes(b, packedUint(roles))
		b = protowire.AppendTag(b, 9, protowire.BytesType)
		b = protowire.AppendBytes(b, packedSint(deltas(ids)))
		b = protowire.AppendTag(b, 10, protowire.BytesType)
		b = protowire.AppendBytes(b, packedUint(types))
		group = protowire.AppendTag(group, 4, protowire.BytesType)
		group = protowire.AppendBytes(group, b)
	}
	return primitiveBlock(st, group)
}

// writeBlob appends one length-prefixed BlobHeader and Blob.
func writeBlob(w *bytes.Buffer, typ string, block []byte, compress bool) error {
	var blob []byte
	if compress {
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		if _, err := zw.Write(block); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		blob = protowire.AppendTag(blob, 2, protowire.VarintType)
		blob = protowire.AppendVarint(blob, uint64(len(block)))
		blob = protowire.AppendTag(blob, 3, protowire.BytesType)
		blob = protowire.AppendBytes(blob, z.Bytes())
	} else {
		blob = protowire.AppendTag(blob, 1, protowire.BytesType)
		blob = protowire.AppendBytes(blob, block)
	}
	var hdr []byte
	hdr = protowire.AppendTag(hdr, 1, protowire.BytesType)
	hdr = protowire.AppendString(hdr, typ)
	hdr = protowire.AppendTag(hdr, 3, protowire.VarintType)
	hdr = protowire.AppendVarint(hdr, uint64(len(blob)))
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(hdr))) // #nosec G115 -- a blob header is a few bytes
	w.Write(n[:])
	w.Write(hdr)
	w.Write(blob)
	return nil
}
