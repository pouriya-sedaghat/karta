// Package osmfile inspects an OSM snapshot before import: it opens only
// regular files (no symlinks), streams a SHA-256 digest and reads the header
// of a PBF (OSMHeader block) or OSM XML (<osm> and <bounds>) file.
package osmfile

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/pouriya-sedaghat/karta/internal/pb"
)

// Format of a snapshot file.
type Format string

const (
	FormatPBF Format = "pbf"
	FormatXML Format = "xml"
)

// Limits from the OSM PBF specification.
const (
	maxBlobHeaderSize = 64 * 1024
	maxBlobSize       = 32 * 1024 * 1024
)

// Info describes a snapshot file.
type Info struct {
	Path   string
	Format Format
	Size   int64
	SHA256 string
	// BBox is west, south, east, north from the file header, if present.
	BBox *[4]float64
	// Timestamp is the replication/snapshot timestamp from the header, if present.
	Timestamp        *time.Time
	WritingProgram   string
	RequiredFeatures []string
}

// DetectFormat returns the format implied by the file name.
func DetectFormat(path string) (Format, error) {
	switch {
	case strings.HasSuffix(path, ".osm.pbf"):
		return FormatPBF, nil
	case strings.HasSuffix(path, ".osm"):
		return FormatXML, nil
	}
	return "", errors.New("snapshot file name must end in .osm.pbf or .osm")
}

// Inspect validates the path, digests the file and parses its header.
// maxSize bounds the accepted file size in bytes.
func Inspect(path string, maxSize int64) (Info, error) {
	format, err := DetectFormat(path)
	if err != nil {
		return Info{}, err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return Info{}, err
	}
	if !st.Mode().IsRegular() {
		return Info{}, fmt.Errorf("%s is not a regular file (symlinks and devices are rejected)", path)
	}
	if st.Size() == 0 {
		return Info{}, fmt.Errorf("%s is empty", path)
	}
	if st.Size() > maxSize {
		return Info{}, fmt.Errorf("%s is %d bytes, above the configured limit of %d", path, st.Size(), maxSize)
	}
	f, err := os.Open(path) // #nosec G304 -- operator-supplied snapshot path, checked above
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	info := Info{Path: path, Format: format, Size: st.Size()}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return Info{}, err
	}
	info.SHA256 = hex.EncodeToString(h.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Info{}, err
	}
	switch format {
	case FormatPBF:
		err = readPBFHeader(f, &info)
	case FormatXML:
		err = readXMLHeader(f, &info)
	}
	if err != nil {
		return Info{}, fmt.Errorf("%s: %w", path, err)
	}
	return info, nil
}

// Digest returns the SHA-256 of a file, for re-checking after import.
func Digest(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- same operator-supplied path as Inspect
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// maxTimestamp rejects header timestamps after the year 9999 (and values
// that would overflow a signed Unix time).
const maxTimestamp = 253402300799

func readPBFHeader(r io.Reader, info *Info) error {
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return fmt.Errorf("truncated PBF: %w", err)
	}
	if n == 0 || n > maxBlobHeaderSize {
		return fmt.Errorf("invalid PBF blob header size %d", n)
	}
	hdr := make([]byte, n)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return fmt.Errorf("truncated PBF blob header: %w", err)
	}
	var typ string
	var dataSize uint64
	if err := pb.Walk(hdr, func(f pb.Field) error {
		switch {
		case f.Num == 1 && f.Type == protowire.BytesType:
			typ = string(f.Bytes)
		case f.Num == 3 && f.Type == protowire.VarintType:
			dataSize = f.Varint
		}
		return nil
	}); err != nil {
		return fmt.Errorf("PBF blob header: %w", err)
	}
	if typ != "OSMHeader" {
		return fmt.Errorf("first PBF block is %q, want OSMHeader", typ)
	}
	if dataSize == 0 || dataSize > maxBlobSize {
		return fmt.Errorf("invalid PBF header blob size %d", dataSize)
	}
	blob := make([]byte, dataSize)
	if _, err := io.ReadFull(r, blob); err != nil {
		return fmt.Errorf("truncated PBF header blob: %w", err)
	}
	var raw, zdata []byte
	var rawSize uint64
	var other protowire.Number
	if err := pb.Walk(blob, func(f pb.Field) error {
		switch {
		case f.Num == 1 && f.Type == protowire.BytesType:
			raw = f.Bytes
		case f.Num == 2 && f.Type == protowire.VarintType:
			rawSize = f.Varint
		case f.Num == 3 && f.Type == protowire.BytesType:
			zdata = f.Bytes
		case f.Num >= 4 && f.Type == protowire.BytesType:
			other = f.Num
		}
		return nil
	}); err != nil {
		return fmt.Errorf("PBF header blob: %w", err)
	}
	var block []byte
	switch {
	case raw != nil:
		block = raw
	case zdata != nil:
		if rawSize == 0 || rawSize > maxBlobSize {
			return fmt.Errorf("invalid PBF header raw size %d", rawSize)
		}
		zr, err := zlib.NewReader(bytes.NewReader(zdata))
		if err != nil {
			return fmt.Errorf("PBF header: %w", err)
		}
		// Read at most one byte more than declared to detect a mismatch
		// without trusting the compressed stream's length.
		block, err = io.ReadAll(io.LimitReader(zr, maxBlobSize+1))
		if err != nil {
			return fmt.Errorf("PBF header: %w", err)
		}
		if uint64(len(block)) != rawSize {
			return fmt.Errorf("PBF header decompressed to %d bytes, want %d", len(block), rawSize)
		}
	case other != 0:
		return fmt.Errorf("PBF header uses unsupported compression (blob field %d)", other)
	default:
		return errors.New("PBF header blob has no data")
	}
	return pb.Walk(block, func(f pb.Field) error {
		switch {
		case f.Num == 1 && f.Type == protowire.BytesType:
			var nano [4]int64 // left, right, top, bottom in nanodegrees (sint64)
			if err := pb.Walk(f.Bytes, func(c pb.Field) error {
				if c.Num >= 1 && c.Num <= 4 && c.Type == protowire.VarintType {
					nano[c.Num-1] = protowire.DecodeZigZag(c.Varint)
				}
				return nil
			}); err != nil {
				return fmt.Errorf("PBF header bbox: %w", err)
			}
			info.BBox = &[4]float64{float64(nano[0]) / 1e9, float64(nano[3]) / 1e9, float64(nano[1]) / 1e9, float64(nano[2]) / 1e9}
		case f.Num == 4 && f.Type == protowire.BytesType:
			info.RequiredFeatures = append(info.RequiredFeatures, string(f.Bytes))
		case f.Num == 16 && f.Type == protowire.BytesType:
			info.WritingProgram = string(f.Bytes)
		case f.Num == 32 && f.Type == protowire.VarintType:
			if f.Varint > maxTimestamp {
				return fmt.Errorf("implausible PBF replication timestamp %d", f.Varint)
			}
			t := time.Unix(int64(f.Varint), 0).UTC() // #nosec G115 -- bounded by maxTimestamp above
			info.Timestamp = &t
		}
		return nil
	})
}

func readXMLHeader(r io.Reader, info *Info) error {
	dec := xml.NewDecoder(io.LimitReader(r, 1<<20))
	sawRoot := false
	for {
		tok, err := dec.Token()
		if err != nil {
			if !sawRoot {
				return fmt.Errorf("no <osm> root element: %w", err)
			}
			return nil
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if !sawRoot {
			if se.Name.Local != "osm" {
				return fmt.Errorf("root element is <%s>, want <osm>", se.Name.Local)
			}
			sawRoot = true
			for _, a := range se.Attr {
				switch a.Name.Local {
				case "timestamp":
					t, err := time.Parse(time.RFC3339, a.Value)
					if err != nil {
						return fmt.Errorf("invalid <osm timestamp>: %w", err)
					}
					t = t.UTC()
					info.Timestamp = &t
				case "generator":
					info.WritingProgram = a.Value
				}
			}
			continue
		}
		if se.Name.Local != "bounds" {
			return nil // header ends at the first data element
		}
		attrs := map[string]float64{}
		for _, a := range se.Attr {
			v, err := strconv.ParseFloat(a.Value, 64)
			if err != nil {
				return fmt.Errorf("invalid <bounds %s>: %w", a.Name.Local, err)
			}
			attrs[a.Name.Local] = v
		}
		info.BBox = &[4]float64{attrs["minlon"], attrs["minlat"], attrs["maxlon"], attrs["maxlat"]}
		return nil
	}
}
