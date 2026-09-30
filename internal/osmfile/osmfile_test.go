package osmfile

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/pouriya-sedaghat/karta/internal/pbfwrite"
)

func TestFixtureXMLHeader(t *testing.T) {
	info, err := Inspect("../../testdata/fixture/karta-fixture.osm", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != FormatXML || info.BBox == nil || *info.BBox != [4]float64{0, 0, 0.02, 0.015} {
		t.Fatalf("info %+v", info)
	}
	if info.Timestamp == nil || !info.Timestamp.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) || len(info.SHA256) != 64 {
		t.Fatalf("timestamp %v sha %q", info.Timestamp, info.SHA256)
	}
}

// The real Chitgar extract, when present (it is not committed).
func TestChitgarPBFHeader(t *testing.T) {
	path := "../../data/local/tehran-chitgar.osm.pbf"
	if _, err := os.Stat(path); err != nil {
		t.Skip("Chitgar extract not present in data/local")
	}
	info, err := Inspect(path, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if info.SHA256 != "7d0e69a2d5e1ad184ee48637626882bb8bcb7acd8e95123e05d0697ce216191e" {
		t.Fatalf("sha %s", info.SHA256)
	}
	if info.BBox == nil || *info.BBox != [4]float64{51.175, 35.705, 51.285, 35.785} {
		t.Fatalf("bbox %v", info.BBox)
	}
	// The full scan counts every object; docs/development-data.md records
	// 125,907 nodes, 18,619 ways and 267 relations for this file.
	if info.Nodes != 125907 || info.Ways != 18619 || info.Relations != 267 {
		t.Fatalf("counts %d/%d/%d", info.Nodes, info.Ways, info.Relations)
	}
}

// The committed publication snapshots scan completely.
func TestFixtureSnapshots(t *testing.T) {
	for name, want := range map[string]string{
		"karta-fixture-a.osm.pbf": "2026-01-01T00:00:00Z",
		"karta-fixture-b.osm.pbf": "2026-02-01T00:00:00Z",
	} {
		info, err := Inspect("../../testdata/fixture/snapshots/"+name, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if info.Timestamp == nil || info.Timestamp.Format(time.RFC3339) != want || *info.BBox != [4]float64{0, 0, 0.02, 0.015} {
			t.Errorf("%s: %+v", name, info)
		}
		if info.Blocks != 4 || info.Nodes == 0 || info.Ways != 15 || info.Relations != 3 {
			t.Errorf("%s: blocks %d nodes %d ways %d relations %d", name, info.Blocks, info.Nodes, info.Ways, info.Relations)
		}
	}
}

// snapshot is a small valid PBF from pbfwrite, split into its header blob and
// the data blobs that follow it.
func snapshot(t *testing.T, zlibData bool) (full, header, data []byte) {
	t.Helper()
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d := &pbfwrite.Data{
		BBox: &[4]float64{0, 0, 0.02, 0.015}, Timestamp: &ts,
		Nodes: []pbfwrite.Node{{ID: 1, Lat: 10, Lon: 10, Tags: []pbfwrite.Tag{{K: "amenity", V: "cafe"}, {K: "name", V: "کافه"}}}, {ID: 2, Lat: 20, Lon: 20}},
		Ways:  []pbfwrite.Way{{ID: 10, Refs: []int64{1, 2}, Tags: []pbfwrite.Tag{{K: "highway", V: "residential"}}}},
		Relations: []pbfwrite.Relation{{ID: 20, Members: []pbfwrite.Member{{Type: "way", Ref: 10, Role: "outer"}},
			Tags: []pbfwrite.Tag{{K: "type", V: "multipolygon"}}}},
	}
	b, err := pbfwrite.Encode(d, pbfwrite.Options{Zlib: zlibData})
	if err != nil {
		t.Fatal(err)
	}
	n := firstBlobLen(b)
	return b, b[:n], b[n:]
}

func firstBlobLen(b []byte) int {
	hl := int(binary.BigEndian.Uint32(b))
	var size int
	_ = walkVarint(b[4:4+hl], 3, &size)
	return 4 + hl + size
}

func walkVarint(msg []byte, num protowire.Number, out *int) error {
	for len(msg) > 0 {
		n, typ, k := protowire.ConsumeTag(msg)
		msg = msg[k:]
		if typ == protowire.VarintType {
			v, k := protowire.ConsumeVarint(msg)
			if n == num {
				*out = int(v)
			}
			msg = msg[k:]
			continue
		}
		msg = msg[protowire.ConsumeFieldValue(n, typ, msg):]
	}
	return nil
}

func pbfWithHeader(t *testing.T, block []byte, compress bool, headerType string) []byte {
	t.Helper()
	var blob []byte
	if compress {
		var z bytes.Buffer
		w := zlib.NewWriter(&z)
		w.Write(block)
		w.Close()
		blob = protowire.AppendVarint(protowire.AppendTag(blob, 2, protowire.VarintType), uint64(len(block)))
		blob = protowire.AppendBytes(protowire.AppendTag(blob, 3, protowire.BytesType), z.Bytes())
	} else {
		blob = protowire.AppendBytes(protowire.AppendTag(blob, 1, protowire.BytesType), block)
	}
	var hdr []byte
	hdr = protowire.AppendString(protowire.AppendTag(hdr, 1, protowire.BytesType), headerType)
	hdr = protowire.AppendVarint(protowire.AppendTag(hdr, 3, protowire.VarintType), uint64(len(blob)))
	out := binary.BigEndian.AppendUint32(nil, uint32(len(hdr)))
	return append(append(out, hdr...), blob...)
}

func headerBlock(bbox [4]int64, ts uint64) []byte {
	var bb []byte
	for i, v := range bbox { // left, right, top, bottom
		bb = protowire.AppendVarint(protowire.AppendTag(bb, protowire.Number(i+1), protowire.VarintType), protowire.EncodeZigZag(v))
	}
	var b []byte
	b = protowire.AppendBytes(protowire.AppendTag(b, 1, protowire.BytesType), bb)
	b = protowire.AppendString(protowire.AppendTag(b, 4, protowire.BytesType), "OsmSchema-V0.6")
	b = protowire.AppendVarint(protowire.AppendTag(b, 32, protowire.VarintType), ts)
	return b
}

func write(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPBFHeaderParsing(t *testing.T) {
	block := headerBlock([4]int64{51175000000, 51285000000, 35785000000, 35705000000}, 1790540616)
	_, _, data := snapshot(t, false)
	for _, compress := range []bool{false, true} {
		file := append(pbfWithHeader(t, block, compress, "OSMHeader"), data...)
		info, err := Inspect(write(t, "a.osm.pbf", file), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if *info.BBox != [4]float64{51.175, 35.705, 51.285, 35.785} || info.Timestamp.Unix() != 1790540616 {
			t.Fatalf("compress=%v: %+v", compress, info)
		}
	}
}

func TestRejectsBadInput(t *testing.T) {
	good, header, data := snapshot(t, false)
	hdrOnly := pbfWithHeader(t, headerBlock([4]int64{0, 1, 1, 0}, 1), false, "OSMHeader")
	history := append(pbfWithHeader(t, append(headerBlock([4]int64{0, 1, 1, 0}, 1),
		protowire.AppendString(protowire.AppendTag(nil, 4, protowire.BytesType), "HistoricalInformation")...), false, "OSMHeader"), data...)
	unknownFeature := append(pbfWithHeader(t, append(headerBlock([4]int64{0, 1, 1, 0}, 1),
		protowire.AppendString(protowire.AppendTag(nil, 4, protowire.BytesType), "Sort.Geographic.Future")...), false, "OSMHeader"), data...)
	changesets := append(append([]byte(nil), header...), pbfWithHeader(t, primitiveBlock(protowire.AppendBytes(protowire.AppendTag(nil, 5, protowire.BytesType), []byte{})), false, "OSMData")...)
	denseMismatch := append(append([]byte(nil), header...), pbfWithHeader(t, primitiveBlock(
		protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType),
			protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), []byte{2, 2}))), false, "OSMData")...)
	lz4Blob := func() []byte {
		blob := protowire.AppendBytes(protowire.AppendTag(nil, 6, protowire.BytesType), []byte{1, 2, 3})
		hdr := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "OSMData")
		hdr = protowire.AppendVarint(protowire.AppendTag(hdr, 3, protowire.VarintType), uint64(len(blob)))
		return append(append(append(append([]byte(nil), header...), binary.BigEndian.AppendUint32(nil, uint32(len(hdr)))...), hdr...), blob...)
	}()
	cases := map[string]struct {
		name string
		data []byte
		want string
	}{
		"wrong extension":       {"a.pbf", good, "must end in"},
		"empty":                 {"a.osm.pbf", nil, "is empty"},
		"truncated length":      {"a.osm.pbf", []byte{0, 0}, "truncated"},
		"huge header size":      {"a.osm.pbf", binary.BigEndian.AppendUint32(nil, 1<<30), "blob header size"},
		"data block first":      {"a.osm.pbf", pbfWithHeader(t, []byte{}, false, "OSMData"), "want OSMHeader"},
		"truncated blob":        {"a.osm.pbf", good[:len(good)-3], "truncated"},
		"header only":           {"a.osm.pbf", hdrOnly, "no OSMData blocks"},
		"trailing garbage":      {"a.osm.pbf", append(append([]byte(nil), good...), 0, 0), "truncated"},
		"second header":         {"a.osm.pbf", append(append([]byte(nil), good...), header...), "want OSMData"},
		"history file":          {"a.osm.pbf", history, "history file"},
		"unknown feature":       {"a.osm.pbf", unknownFeature, "unsupported feature"},
		"changeset group":       {"a.osm.pbf", changesets, "changesets"},
		"dense arrays differ":   {"a.osm.pbf", denseMismatch, "dense nodes"},
		"lz4 blob":              {"a.osm.pbf", lz4Blob, "unsupported compression"},
		"garbage header":        {"a.osm.pbf", append(binary.BigEndian.AppendUint32(nil, 3), 0xff, 0xff, 0xff), "malformed"},
		"implausible timestamp": {"a.osm.pbf", pbfWithHeader(t, headerBlock([4]int64{0, 1, 1, 0}, 1<<63), false, "OSMHeader"), "implausible"},
		"xml without osm root":  {"a.osm", []byte("<html></html>"), "want <osm>"},
		"xml bad timestamp":     {"a.osm", []byte(`<osm timestamp="yesterday"></osm>`), "timestamp"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Inspect(write(t, c.name, c.data), 1<<20)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
	// A cut inside a blob is a structural error. A cut exactly between two
	// blobs leaves a well-formed, shorter PBF that no scan can tell apart
	// from a smaller snapshot: it is caught by the digest (ready marker,
	// authorized digests, provenance size), never by the scan alone, so the
	// test checks that its digest differs.
	t.Run("truncated at every byte", func(t *testing.T) {
		for _, z := range []bool{false, true} {
			full, _, _ := snapshot(t, z)
			whole, err := Inspect(write(t, "a.osm.pbf", full), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			boundaries := map[int]bool{}
			for off := 0; off < len(full); off += firstBlobLen(full[off:]) {
				boundaries[off] = true
			}
			for cut := 1; cut < len(full); cut++ {
				info, err := Inspect(write(t, "a.osm.pbf", full[:cut]), 1<<20)
				switch {
				case !boundaries[cut] && err == nil:
					t.Fatalf("zlib=%v: file cut inside a blob at %d of %d bytes accepted", z, cut, len(full))
				case boundaries[cut] && err == nil && (info.SHA256 == whole.SHA256 || info.Blocks >= whole.Blocks):
					t.Fatalf("zlib=%v: cut at blob boundary %d not distinguishable: %+v", z, cut, info)
				}
			}
		}
	})
	t.Run("corrupted zlib data", func(t *testing.T) {
		full, _, _ := snapshot(t, true)
		bad := append([]byte(nil), full...)
		bad[len(bad)-6] ^= 0xff // inside the last blob's deflate stream or checksum
		if _, err := Inspect(write(t, "a.osm.pbf", bad), 1<<20); err == nil {
			t.Fatal("corrupted zlib blob accepted")
		}
	})
	t.Run("zlib snapshot counts", func(t *testing.T) {
		full, _, _ := snapshot(t, true)
		info, err := Inspect(write(t, "a.osm.pbf", full), 1<<20)
		if err != nil || info.Nodes != 2 || info.Ways != 1 || info.Relations != 1 {
			t.Fatalf("%+v %v", info, err)
		}
	})
	t.Run("fifo", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "f.osm.pbf")
		if err := mkfifo(p); err != nil {
			t.Skip(err)
		}
		if _, err := Inspect(p, 1<<20); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("size limit", func(t *testing.T) {
		if _, err := Inspect(write(t, "a.osm.pbf", good), 10); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		target := write(t, "a.osm.pbf", good)
		link := filepath.Join(t.TempDir(), "b.osm.pbf")
		if err := os.Symlink(target, link); err != nil {
			t.Skip(err)
		}
		if _, err := Inspect(link, 1<<20); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v", err)
		}
	})
}

func primitiveBlock(group []byte) []byte {
	b := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), nil))
	return protowire.AppendBytes(protowire.AppendTag(b, 2, protowire.BytesType), group)
}

func FuzzInspectPBF(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		p := filepath.Join(t.TempDir(), "f.osm.pbf")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = Inspect(p, 1<<20) // must not panic
	})
}
