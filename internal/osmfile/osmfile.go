// Package osmfile inspects an OSM snapshot before import: it opens only
// regular files (no symlinks), streams a SHA-256 digest and reads the header
// of a PBF (OSMHeader block) or OSM XML (<osm> and <bounds>) file.
//
// A PBF file is checked completely, not just its header: every blob must be
// framed within the PBF size limits, decode (raw or zlib, to exactly its
// declared size), and hold a well-formed OSMHeader (first) or OSMData
// (after it) block, and the last blob must end exactly at the end of the
// file. A truncated, padded or corrupted file is therefore rejected before
// osm2pgsql reads it. The digest is computed over the same bytes in the same
// pass.
package osmfile

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
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
	"syscall"
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
	// Counts from the full scan of a PBF file (zero for XML).
	Blocks    int64
	Nodes     int64
	Ways      int64
	Relations int64
}

// SupportedFeatures are the PBF required features Karta accepts; a snapshot
// needing anything else (for example HistoricalInformation, which marks a
// history file rather than a snapshot) is rejected.
var SupportedFeatures = map[string]bool{"OsmSchema-V0.6": true, "DenseNodes": true}

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
	return InspectContext(context.Background(), path, maxSize)
}

// InspectContext is Inspect, stopped when ctx ends: reading the file then
// fails with context.Cause(ctx) (a publication deadline or a shutdown).
func InspectContext(ctx context.Context, path string, maxSize int64) (Info, error) {
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
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // #nosec G304 -- operator-supplied snapshot path, checked above
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	// The file opened must be the regular file inspected above, not one
	// swapped in between (a symlink, FIFO or another file).
	if fst, err := f.Stat(); err != nil || !fst.Mode().IsRegular() || !os.SameFile(st, fst) {
		return Info{}, fmt.Errorf("%s changed while it was being opened", path)
	}
	info := Info{Path: path, Format: format, Size: st.Size()}
	h := sha256.New()
	switch format {
	case FormatPBF:
		counter := &countingReader{r: io.TeeReader(ctxReader{ctx, f}, h)}
		err = scanPBF(bufio.NewReaderSize(counter, 1<<20), &info, false)
		if err == nil && counter.n != st.Size() {
			err = fmt.Errorf("read %d bytes but the file is %d bytes; it changed while being read", counter.n, st.Size())
		}
	case FormatXML:
		if _, err = io.Copy(h, ctxReader{ctx, f}); err != nil {
			return Info{}, err
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return Info{}, err
		}
		err = readXMLHeader(f, &info)
	}
	if err != nil {
		return Info{}, fmt.Errorf("%s: %w", path, err)
	}
	if fst, err := f.Stat(); err != nil || fst.Size() != st.Size() || !fst.ModTime().Equal(st.ModTime()) {
		return Info{}, fmt.Errorf("%s changed while it was being read", path)
	}
	info.SHA256 = hex.EncodeToString(h.Sum(nil))
	return info, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Digest returns the SHA-256 of a file, for re-checking after import.
func Digest(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // #nosec G304 -- same operator-supplied path as Inspect
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

// Header reads only the OSMHeader block of a PBF file (its box, replication
// timestamp and required features), without hashing or scanning the rest.
// It is the local intake's cheap advisory check before it authorizes a
// digest; the publisher still scans the whole staged file.
func Header(path string) (Info, error) {
	if f, err := DetectFormat(path); err != nil || f != FormatPBF {
		return Info{}, errors.New("not a .osm.pbf file")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // #nosec G304 -- the caller's own copy
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Info{}, err
	}
	if !st.Mode().IsRegular() {
		return Info{}, fmt.Errorf("%s is not a regular file", path)
	}
	info := Info{Path: path, Format: FormatPBF, Size: st.Size()}
	if err := scanPBF(bufio.NewReaderSize(f, 1<<16), &info, true); err != nil {
		return Info{}, fmt.Errorf("%s: %w", path, err)
	}
	return info, nil
}

// scanPBF reads every blob of a PBF file (only the first with headerOnly).
// The first must be OSMHeader, all others OSMData; the file must end
// exactly after the last blob.
func scanPBF(r io.Reader, info *Info, headerOnly bool) error {
	var hdrBuf, blobBuf []byte
	var offset int64
	for n := 0; ; n++ {
		var lenBuf [4]byte
		got, err := io.ReadFull(r, lenBuf[:])
		if err == io.EOF && got == 0 {
			if n == 0 {
				return errors.New("truncated PBF: no blocks")
			}
			if n == 1 {
				return errors.New("PBF has an OSMHeader but no OSMData blocks")
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("truncated PBF at byte %d: %w", offset, err)
		}
		hl := binary.BigEndian.Uint32(lenBuf[:])
		if hl == 0 || hl > maxBlobHeaderSize {
			return fmt.Errorf("invalid PBF blob header size %d at byte %d", hl, offset)
		}
		hdrBuf = grow(hdrBuf, int(hl))
		if _, err := io.ReadFull(r, hdrBuf); err != nil {
			return fmt.Errorf("truncated PBF blob header at byte %d: %w", offset, err)
		}
		var typ string
		var dataSize uint64
		var sawType, sawSize bool
		if err := pb.Walk(hdrBuf, func(f pb.Field) error {
			switch {
			case f.Num == 1 && f.Type == protowire.BytesType:
				typ, sawType = string(f.Bytes), true
			case f.Num == 3 && f.Type == protowire.VarintType:
				dataSize, sawSize = f.Varint, true
			}
			return nil
		}); err != nil {
			return fmt.Errorf("PBF blob header at byte %d: %w", offset, err)
		}
		if !sawType || !sawSize {
			return fmt.Errorf("PBF blob header at byte %d lacks type or datasize", offset)
		}
		switch {
		case n == 0 && typ != "OSMHeader":
			return fmt.Errorf("first PBF block is %q, want OSMHeader", typ)
		case n > 0 && typ != "OSMData":
			return fmt.Errorf("PBF block %d at byte %d is %q, want OSMData", n, offset, typ)
		}
		if dataSize == 0 || dataSize > maxBlobSize {
			return fmt.Errorf("invalid PBF blob size %d at byte %d", dataSize, offset)
		}
		blobBuf = grow(blobBuf, int(dataSize)) // #nosec G115 -- bounded by maxBlobSize above
		if _, err := io.ReadFull(r, blobBuf); err != nil {
			return fmt.Errorf("truncated PBF %s blob at byte %d: %w", typ, offset, err)
		}
		block, err := decodeBlob(blobBuf)
		if err != nil {
			return fmt.Errorf("PBF %s blob at byte %d: %w", typ, offset, err)
		}
		if n == 0 {
			err = parseHeaderBlock(block, info)
		} else {
			err = scanDataBlock(block, info)
		}
		if err != nil {
			return fmt.Errorf("PBF %s block at byte %d: %w", typ, offset, err)
		}
		if headerOnly {
			return nil
		}
		info.Blocks++
		offset += 4 + int64(hl) + int64(dataSize) // #nosec G115 -- bounded sizes
	}
}

func grow(b []byte, n int) []byte {
	if cap(b) < n {
		return make([]byte, n)
	}
	return b[:n]
}

// decodeBlob returns the block stored in a Blob message: raw, or zlib data
// that must inflate to exactly raw_size bytes. Other compressions (lzma, lz4,
// zstd) are rejected.
func decodeBlob(blob []byte) ([]byte, error) {
	var raw, zdata []byte
	var rawSize uint64
	var sawRaw, sawRawSize bool
	var other protowire.Number
	if err := pb.Walk(blob, func(f pb.Field) error {
		switch {
		case f.Num == 1 && f.Type == protowire.BytesType:
			raw, sawRaw = f.Bytes, true
		case f.Num == 2 && f.Type == protowire.VarintType:
			rawSize, sawRawSize = f.Varint, true
		case f.Num == 3 && f.Type == protowire.BytesType:
			zdata = f.Bytes
		case f.Num >= 4 && f.Type == protowire.BytesType:
			other = f.Num
		}
		return nil
	}); err != nil {
		return nil, err
	}
	switch {
	case sawRaw:
		if sawRawSize && rawSize != uint64(len(raw)) {
			return nil, fmt.Errorf("raw blob is %d bytes, header says %d", len(raw), rawSize)
		}
		return raw, nil
	case zdata != nil:
		if rawSize == 0 || rawSize > maxBlobSize {
			return nil, fmt.Errorf("invalid raw size %d", rawSize)
		}
		zr, err := zlib.NewReader(bytes.NewReader(zdata))
		if err != nil {
			return nil, err
		}
		// Read at most one byte more than declared to detect a mismatch
		// without trusting the compressed stream's length.
		block, err := io.ReadAll(io.LimitReader(zr, maxBlobSize+1))
		if err != nil {
			return nil, err
		}
		if uint64(len(block)) != rawSize {
			return nil, fmt.Errorf("decompressed to %d bytes, want %d", len(block), rawSize)
		}
		if err := zr.Close(); err != nil {
			return nil, err
		}
		return block, nil
	case other != 0:
		return nil, fmt.Errorf("unsupported compression (blob field %d); only raw and zlib are accepted", other)
	}
	return nil, errors.New("blob has no data")
}

func parseHeaderBlock(block []byte, info *Info) error {
	err := pb.Walk(block, func(f pb.Field) error {
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
	if err != nil {
		return err
	}
	for _, feat := range info.RequiredFeatures {
		if !SupportedFeatures[feat] {
			if feat == "HistoricalInformation" {
				return errors.New("PBF requires HistoricalInformation: a history file is not a snapshot")
			}
			return fmt.Errorf("PBF requires unsupported feature %q", feat)
		}
	}
	return nil
}

// scanDataBlock checks the structure of a PrimitiveBlock (string table and
// primitive groups, every nested message well-formed) and counts objects.
func scanDataBlock(block []byte, info *Info) error {
	var sawStrings bool
	var groups int
	err := pb.Walk(block, func(f pb.Field) error {
		switch f.Num {
		case 1:
			if f.Type != protowire.BytesType {
				return errors.New("string table has the wrong wire type")
			}
			sawStrings = true
			return pb.Walk(f.Bytes, func(pb.Field) error { return nil })
		case 2:
			if f.Type != protowire.BytesType {
				return errors.New("primitive group has the wrong wire type")
			}
			groups++
			return scanGroup(f.Bytes, info)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !sawStrings || groups == 0 {
		return errors.New("primitive block without a string table or groups")
	}
	return nil
}

func scanGroup(group []byte, info *Info) error {
	return pb.Walk(group, func(f pb.Field) error {
		if f.Type != protowire.BytesType {
			return fmt.Errorf("primitive group field %d has the wrong wire type", f.Num)
		}
		switch f.Num {
		case 1: // Node
			info.Nodes++
			return pb.Walk(f.Bytes, func(pb.Field) error { return nil })
		case 2: // DenseNodes: ids, lats and lons are parallel packed arrays
			var ids, lats, lons int64 = -1, -1, -1
			if err := pb.Walk(f.Bytes, func(d pb.Field) error {
				if d.Type != protowire.BytesType {
					return nil
				}
				var err error
				switch d.Num {
				case 1:
					ids, err = countVarints(d.Bytes)
				case 8:
					lats, err = countVarints(d.Bytes)
				case 9:
					lons, err = countVarints(d.Bytes)
				case 5: // DenseInfo
					err = pb.Walk(d.Bytes, func(pb.Field) error { return nil })
				case 10: // keys_vals
					_, err = countVarints(d.Bytes)
				}
				return err
			}); err != nil {
				return fmt.Errorf("dense nodes: %w", err)
			}
			if ids < 0 || ids != lats || ids != lons {
				return fmt.Errorf("dense nodes: %d ids, %d lats, %d lons", ids, lats, lons)
			}
			info.Nodes += ids
		case 3: // Way
			info.Ways++
			return pb.Walk(f.Bytes, func(pb.Field) error { return nil })
		case 4: // Relation
			info.Relations++
			return pb.Walk(f.Bytes, func(pb.Field) error { return nil })
		default:
			return fmt.Errorf("unsupported primitive group field %d (changesets are not snapshot data)", f.Num)
		}
		return nil
	})
}

func countVarints(b []byte) (int64, error) {
	var n int64
	for len(b) > 0 {
		_, k := protowire.ConsumeVarint(b)
		if k < 0 {
			return 0, fmt.Errorf("%w: %v", pb.ErrMalformed, protowire.ParseError(k))
		}
		b = b[k:]
		n++
	}
	return n, nil
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

// ctxReader fails the next read once ctx has ended, so a long scan stops at
// the publication deadline or at shutdown.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if c.ctx.Err() != nil {
		return 0, context.Cause(c.ctx)
	}
	return c.r.Read(p)
}
