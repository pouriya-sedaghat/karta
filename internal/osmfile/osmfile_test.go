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
	for _, compress := range []bool{false, true} {
		info, err := Inspect(write(t, "a.osm.pbf", pbfWithHeader(t, block, compress, "OSMHeader")), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if *info.BBox != [4]float64{51.175, 35.705, 51.285, 35.785} || info.Timestamp.Unix() != 1790540616 {
			t.Fatalf("compress=%v: %+v", compress, info)
		}
	}
}

func TestRejectsBadInput(t *testing.T) {
	good := pbfWithHeader(t, headerBlock([4]int64{0, 1, 1, 0}, 1), false, "OSMHeader")
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
