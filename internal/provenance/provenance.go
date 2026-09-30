// Package provenance reads the sidecar written by scripts/extract_tehran.py
// (<extract>.osm.pbf.provenance.json) and checks it against the extract.
//
// What a sidecar can prove is limited: its output digest, size and box are
// checked against the file and the region, and its own timestamps must be
// consistent. Its claims about the source file (source path and
// source_sha256) cannot be verified without that source; they are recorded
// with the release as claims, never reported as verified.
package provenance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Sidecar is the subset of the provenance file Karta relies on. The raw
// document is kept verbatim in the release metadata.
type Sidecar struct {
	Source         string          `json:"source"`
	SourceSHA256   string          `json:"source_sha256"`
	OutputSHA256   string          `json:"output_sha256"`
	BBoxWGS84      string          `json:"bbox_wgs84"`
	Strategy       string          `json:"strategy"`
	OsmiumVersion  string          `json:"osmium_version"`
	SourceFileinfo fileinfo        `json:"source_fileinfo"`
	OutputFileinfo fileinfo        `json:"output_fileinfo"`
	License        string          `json:"license"`
	Attribution    string          `json:"attribution"`
	LicenseURL     string          `json:"license_url"`
	Raw            json.RawMessage `json:"-"`
}

type fileinfo struct {
	File struct {
		Size int64 `json:"size"`
	} `json:"file"`
	Header struct {
		Boxes  [][]float64       `json:"boxes"`
		Option map[string]string `json:"option"`
	} `json:"header"`
	Data struct {
		Timestamp struct {
			First string `json:"first"`
			Last  string `json:"last"`
		} `json:"timestamp"`
	} `json:"data"`
}

// maxSidecarSize bounds the sidecar; the real one is about 5 KiB.
const maxSidecarSize = 1 << 20

// Load reads a sidecar file.
func Load(path string) (Sidecar, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return Sidecar{}, err
	}
	if !st.Mode().IsRegular() {
		return Sidecar{}, fmt.Errorf("%s is not a regular file", path)
	}
	if st.Size() > maxSidecarSize {
		return Sidecar{}, fmt.Errorf("%s is larger than %d bytes", path, maxSidecarSize)
	}
	b, err := os.ReadFile(path) // #nosec G304 -- sidecar next to the operator-supplied snapshot
	if err != nil {
		return Sidecar{}, err
	}
	return Parse(b)
}

// Parse decodes sidecar bytes. The document must be one JSON object without
// duplicate top-level keys (a duplicate could make two readers see different
// values).
func Parse(b []byte) (Sidecar, error) {
	if err := noDuplicateKeys(b); err != nil {
		return Sidecar{}, err
	}
	var s Sidecar
	if err := json.Unmarshal(b, &s); err != nil {
		return Sidecar{}, err
	}
	s.Raw = b
	return s, nil
}

func noDuplicateKeys(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("provenance sidecar is not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		if seen[key] {
			return fmt.Errorf("provenance sidecar repeats key %q", key)
		}
		seen[key] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("provenance sidecar has data after its JSON object")
	}
	return nil
}

// bboxNames are the bbox_wgs84 coordinates in order.
var bboxNames = [4]string{"west", "south", "east", "north"}

// BBox parses bbox_wgs84 ("west,south,east,north"). strconv.ParseFloat also
// accepts NaN and infinities; those are not coordinates and are rejected.
func (s Sidecar) BBox() ([4]float64, error) {
	var out [4]float64
	parts := strings.Split(s.BBoxWGS84, ",")
	if len(parts) != 4 {
		return out, fmt.Errorf("bbox_wgs84 %q is not west,south,east,north", s.BBoxWGS84)
	}
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return out, fmt.Errorf("bbox_wgs84: %w", err)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return out, fmt.Errorf("bbox_wgs84 %s %q is not a finite number", bboxNames[i], strings.TrimSpace(p))
		}
		out[i] = v
	}
	return out, nil
}

// SourceTimestamp is the snapshot timestamp of the source file the extract
// was cut from (the replication timestamp in its header). The extract's own
// header does not carry one, so this is the data timestamp of the release.
//
// If the header records both osmosis_replication_timestamp and timestamp,
// they must be equal: two different snapshot times are not trustworthy.
func (s Sidecar) SourceTimestamp() (time.Time, error) {
	opt := s.SourceFileinfo.Header.Option
	var found *time.Time
	for _, key := range []string{"osmosis_replication_timestamp", "timestamp"} {
		v := opt[key]
		if v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, fmt.Errorf("source header %s: %w", key, err)
		}
		t = t.UTC()
		if found != nil && !found.Equal(t) {
			return time.Time{}, fmt.Errorf("source header timestamps disagree (%s and %s)", found.Format(time.RFC3339), t.Format(time.RFC3339))
		}
		found = &t
	}
	if found == nil {
		return time.Time{}, errors.New("sidecar has no source header timestamp")
	}
	return *found, nil
}

var hexDigest = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// Check verifies the sidecar describes the given extract: its output digest
// and size are present and equal the file's, and the license is ODbL 1.0.
func (s Sidecar) Check(sha256 string, size int64) error {
	var errs []error
	switch {
	case !hexDigest.MatchString(s.OutputSHA256):
		errs = append(errs, fmt.Errorf("sidecar output_sha256 %q is not a SHA-256 digest", s.OutputSHA256))
	case !strings.EqualFold(s.OutputSHA256, sha256):
		errs = append(errs, fmt.Errorf("sidecar output_sha256 %s does not match the file (%s)", s.OutputSHA256, sha256))
	}
	if s.OutputFileinfo.File.Size != size {
		errs = append(errs, fmt.Errorf("sidecar output size %d does not match the file (%d bytes)", s.OutputFileinfo.File.Size, size))
	}
	if s.License != "ODbL 1.0" {
		errs = append(errs, fmt.Errorf("sidecar license %q, want ODbL 1.0", s.License))
	}
	if s.SourceSHA256 != "" && !hexDigest.MatchString(s.SourceSHA256) {
		errs = append(errs, fmt.Errorf("sidecar source_sha256 %q is not a SHA-256 digest", s.SourceSHA256))
	}
	return errors.Join(errs...)
}

// Verify runs Check and the sidecar's internal consistency checks: the
// extract's recorded header box equals bbox_wgs84, the source snapshot
// timestamp is present, and no object in the source or the extract is newer
// than the snapshot it is said to come from. It returns the parsed box and
// the source timestamp.
func (s Sidecar) Verify(sha256 string, size int64) ([4]float64, time.Time, error) {
	var errs []error
	if err := s.Check(sha256, size); err != nil {
		errs = append(errs, err)
	}
	box, err := s.BBox()
	if err != nil {
		errs = append(errs, err)
	}
	if boxes := s.OutputFileinfo.Header.Boxes; err == nil && len(boxes) > 0 {
		if len(boxes) != 1 || len(boxes[0]) != 4 {
			errs = append(errs, errors.New("sidecar output header must record exactly one box of four numbers"))
		} else {
			for i, v := range boxes[0] {
				if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v-box[i]) > 1e-7 {
					errs = append(errs, fmt.Errorf("sidecar output header box %v differs from bbox_wgs84 %q", boxes[0], s.BBoxWGS84))
					break
				}
			}
		}
	}
	ts, err := s.SourceTimestamp()
	if err != nil {
		errs = append(errs, err)
	} else {
		for _, c := range []struct{ name, value string }{
			{"source_fileinfo.data.timestamp.last", s.SourceFileinfo.Data.Timestamp.Last},
			{"output_fileinfo.data.timestamp.last", s.OutputFileinfo.Data.Timestamp.Last},
		} {
			if c.value == "" {
				continue
			}
			last, err := time.Parse(time.RFC3339, c.value)
			if err != nil {
				errs = append(errs, fmt.Errorf("sidecar %s: %w", c.name, err))
			} else if last.After(ts) {
				errs = append(errs, fmt.Errorf("sidecar %s %s is after the source snapshot timestamp %s",
					c.name, last.UTC().Format(time.RFC3339), ts.Format(time.RFC3339)))
			}
		}
	}
	switch s.Strategy {
	case "", "simple", "complete_ways", "smart":
	default:
		errs = append(errs, fmt.Errorf("sidecar strategy %q is not an osmium extract strategy", s.Strategy))
	}
	return box, ts, errors.Join(errs...)
}
