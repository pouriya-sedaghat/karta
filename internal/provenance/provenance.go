// Package provenance reads the sidecar written by scripts/extract_tehran.py
// (<extract>.osm.pbf.provenance.json) and checks it against the extract.
package provenance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
		Option map[string]string `json:"option"`
	} `json:"header"`
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
	var s Sidecar
	if err := json.Unmarshal(b, &s); err != nil {
		return Sidecar{}, fmt.Errorf("%s: %w", path, err)
	}
	s.Raw = b
	return s, nil
}

// BBox parses bbox_wgs84 ("west,south,east,north").
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
		out[i] = v
	}
	return out, nil
}

// SourceTimestamp is the snapshot timestamp of the source file the extract
// was cut from (the replication timestamp in its header). The extract's own
// header does not carry one, so this is the data timestamp of the release.
func (s Sidecar) SourceTimestamp() (time.Time, error) {
	opt := s.SourceFileinfo.Header.Option
	for _, key := range []string{"osmosis_replication_timestamp", "timestamp"} {
		if v := opt[key]; v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return time.Time{}, fmt.Errorf("source header %s: %w", key, err)
			}
			return t.UTC(), nil
		}
	}
	return time.Time{}, errors.New("sidecar has no source header timestamp")
}

// Check verifies the sidecar describes the given extract.
func (s Sidecar) Check(sha256 string, size int64) error {
	var errs []error
	if !strings.EqualFold(s.OutputSHA256, sha256) {
		errs = append(errs, fmt.Errorf("sidecar output_sha256 %s does not match the file (%s)", s.OutputSHA256, sha256))
	}
	if s.OutputFileinfo.File.Size != 0 && s.OutputFileinfo.File.Size != size {
		errs = append(errs, fmt.Errorf("sidecar output size %d does not match the file (%d bytes)", s.OutputFileinfo.File.Size, size))
	}
	if s.License != "" && s.License != "ODbL 1.0" {
		errs = append(errs, fmt.Errorf("sidecar license %q, want ODbL 1.0", s.License))
	}
	return errors.Join(errs...)
}
