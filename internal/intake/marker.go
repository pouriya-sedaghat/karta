package intake

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/pouriya-sedaghat/karta/internal/safefile"
)

// Landing file suffixes.
const (
	SnapshotSuffix   = ".osm.pbf"
	SidecarSuffix    = ".osm.pbf.provenance.json"
	CompletionSuffix = ".osm.pbf.complete"
	// DeliveryFormat is the completion marker's format.
	DeliveryFormat = "karta-delivery/1"
	// MaxCompletionBytes bounds a completion marker.
	MaxCompletionBytes = 4096
	// MaxSidecarBytes bounds a provenance sidecar (as the inbox).
	MaxSidecarBytes = 1 << 20
)

// Completion is the producer's completion marker: the file it completed and
// the digest and size it computed from its source copy before the transfer.
type Completion struct {
	Format    string `json:"format"`
	File      string `json:"file"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

var hexDigest = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// ParseCompletion decodes a completion marker for snapshot file name. Upper
// case hex, a UTF-8 byte-order mark and CRLF line ends (as Windows tools
// write them) are accepted; unknown fields and repeated keys are not.
func ParseCompletion(b []byte, snapshotName string) (Completion, error) {
	var c Completion
	if len(b) > MaxCompletionBytes {
		return c, fmt.Errorf("the completion marker is larger than %d bytes", MaxCompletionBytes)
	}
	if err := safefile.StrictDecode(b, &c); err != nil {
		return c, fmt.Errorf("the completion marker is not a %s JSON object: %v", DeliveryFormat, err)
	}
	switch {
	case c.Format != DeliveryFormat:
		return c, fmt.Errorf("the completion marker's format is %q, not %q", c.Format, DeliveryFormat)
	case c.File != snapshotName:
		return c, fmt.Errorf("the completion marker names %q, not %q", c.File, snapshotName)
	case !hexDigest.MatchString(c.SHA256):
		return c, errors.New("the completion marker's sha256 must be 64 hex digits")
	case c.SizeBytes < 1:
		return c, errors.New("the completion marker's size_bytes must be positive")
	}
	c.SHA256 = strings.ToLower(c.SHA256)
	return c, nil
}
