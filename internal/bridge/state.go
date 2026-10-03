package bridge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/safefile"
)

// Hidden directories (the spool scan ignores names starting with a dot).
const (
	AcquireDirName = ".acquire"
	StatusDirName  = ".status"
	partialDirName = ".partial"
	stagingDirName = ".staging"
	stateFileName  = "state.json"
	maxStateBytes  = 512 << 10
	stateVersion   = 1
	// MaxSerial keeps the fetcher's %012d delivery names in serial order.
	MaxSerial = 999_999_999_999
)

// StateError is a recorded failure.
type StateError struct {
	At      time.Time `json:"at"`
	Code    string    `json:"code"`
	Message string    `json:"message"`
}

// Hint is what the distributor says about its current file without sending
// it: advisory only.
type Hint struct {
	ETag          string    `json:"etag,omitempty"`
	LastModified  string    `json:"last_modified,omitempty"`
	ContentLength int64     `json:"content_length"`
	MD5           string    `json:"md5,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
}

// same reports whether two hints agree on every validator both have. A hint
// with no validators at all agrees with nothing being known: the
// re-verification schedule then decides.
func (h *Hint) same(o *Hint) bool {
	if h == nil || o == nil {
		return false
	}
	return h.ETag == o.ETag && h.LastModified == o.LastModified && h.ContentLength == o.ContentLength && h.MD5 == o.MD5
}

// Acquisition is one complete download.
type Acquisition struct {
	SHA256     string    `json:"sha256"`
	SizeBytes  int64     `json:"size_bytes"`
	MD5        string    `json:"md5"`
	Source     string    `json:"source"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// VerifiedAt is the last time a full download produced these bytes.
	VerifiedAt time.Time `json:"verified_at"`
	Hint       *Hint     `json:"hint"`
	// MD5Matched is whether the distributor's .md5 (fetched before and after
	// the download, unchanged) matched the bytes; nil without an md5_url.
	MD5Matched *bool  `json:"md5_matched"`
	UserAgent  string `json:"user_agent"`
	Version    string `json:"acquire_version"`
}

// DownloadProgress is the download in progress (or interrupted).
type DownloadProgress struct {
	Bytes     int64     `json:"bytes"`
	SizeBytes int64     `json:"size_bytes"`
	ETag      string    `json:"etag,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Attempts  int       `json:"attempts"`
}

// AcquireState is the acquire process's persisted state.
type AcquireState struct {
	Version             int               `json:"version"`
	UpdatedAt           time.Time         `json:"updated_at"`
	Source              string            `json:"source"`
	LastCheckAt         *time.Time        `json:"last_check_at"`
	LastSuccessAt       *time.Time        `json:"last_success_at"`
	LastError           *StateError       `json:"last_error"`
	ConsecutiveFailures int               `json:"consecutive_failures"`
	NextAttemptAt       *time.Time        `json:"next_attempt_at"`
	LastOutcome         string            `json:"last_outcome"`
	Hint                *Hint             `json:"hint"`
	Last                *Acquisition      `json:"last"`
	Download            *DownloadProgress `json:"download"`
}

// Signed is a manifest the signer issued.
type Signed struct {
	Serial         int64     `json:"serial"`
	SHA256         string    `json:"sha256"`
	SizeBytes      int64     `json:"size_bytes"`
	DataTimestamp  time.Time `json:"data_timestamp"`
	IssuedAt       time.Time `json:"issued_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	EnvelopeSHA256 string    `json:"envelope_sha256"`
	KeyIDs         []string  `json:"key_ids"`
	// Envelope is the exact signed manifest, persisted before it is made
	// visible: after a crash the same serial is promoted with the same
	// bytes, never re-signed differently.
	Envelope string `json:"envelope"`
	Promoted bool   `json:"promoted"`
	Reason   string `json:"reason"`
}

// AssetRef is a published asset and how long a manifest may still name it.
type AssetRef struct {
	SHA256    string    `json:"sha256"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Held is a newer download the signer did not sign.
type Held struct {
	SHA256        string    `json:"sha256"`
	DataTimestamp time.Time `json:"data_timestamp"`
	Code          string    `json:"code"`
	Reason        string    `json:"reason"`
	At            time.Time `json:"at"`
}

// Raise is an operator's high-water raise after a state restore or loss.
type Raise struct {
	From   int64     `json:"from"`
	To     int64     `json:"to"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// SignerState is the sign process's persisted state.
type SignerState struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	BridgeID  string    `json:"bridge_id"`
	// HighWater is the highest serial ever allocated.
	HighWater int64       `json:"high_water"`
	Current   *Signed     `json:"current"`
	Assets    []AssetRef  `json:"assets"`
	Processed []string    `json:"processed"`
	Held      *Held       `json:"held"`
	LastError *StateError `json:"last_error"`
	History   []Signed    `json:"history"`
	Raises    []Raise     `json:"raises"`
}

func readJSONState(path string, v any) (bool, error) {
	b, err := safefile.ReadRegular(path, maxStateBytes)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := safefile.StrictDecode(b, v); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

// ReadAcquireState reads the acquire state of a spool directory (nil if
// none).
func ReadAcquireState(spool string) (*AcquireState, error) {
	var s AcquireState
	ok, err := readJSONState(filepath.Join(spool, AcquireDirName, stateFileName), &s)
	if err != nil || !ok {
		return nil, err
	}
	if s.Version != stateVersion {
		return nil, errors.New("unsupported acquire state version")
	}
	return &s, nil
}

// ReadSignerState reads a signer state (nil if none).
func ReadSignerState(dir string) (*SignerState, error) {
	var s SignerState
	ok, err := readJSONState(filepath.Join(dir, stateFileName), &s)
	if err != nil || !ok {
		return nil, err
	}
	if s.Version != stateVersion {
		return nil, errors.New("unsupported signer state version")
	}
	return &s, nil
}

// ReadSignerStatus reads the status the signer mirrors into the publish
// directory for the serve process (its state without envelopes).
func ReadSignerStatus(publish string) (*SignerState, error) {
	var s SignerState
	ok, err := readJSONState(filepath.Join(publish, StatusDirName, "signer.json"), &s)
	if err != nil || !ok {
		return nil, err
	}
	return &s, nil
}

func writeJSONState(path string, v any) error {
	b, err := safefile.MarshalIndent(v)
	if err != nil {
		return err
	}
	return safefile.WriteAtomic(path, b, 0o644)
}
