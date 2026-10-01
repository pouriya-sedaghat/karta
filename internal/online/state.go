package online

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Fetcher directories inside its outbox (hidden: the publisher's scan
// ignores names starting with a dot).
const (
	StateDirName   = ".fetcher"
	StateFileName  = "state.json"
	PartialDirName = ".partial"
	// MaxStateBytes bounds the state file the publisher reads.
	MaxStateBytes = 64 << 10
	stateVersion  = 1
)

// State is the fetcher's persisted state. The publisher reads it for
// operator status and metrics; it describes the fetcher's view (checks,
// errors, schedule, downloads), while the publisher's own verification of
// delivered manifests is recorded in the registry.
type State struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	// Source is the manifest URL without anything sensitive (URLs carry no
	// credentials or query).
	Source string `json:"source"`
	// LastCheckAt is when the last check started; LastSuccessAt when a
	// check last completed: the manifest fetched and verified, and the
	// snapshot it names delivered (or already delivered).
	LastCheckAt         *time.Time  `json:"last_check_at"`
	LastSuccessAt       *time.Time  `json:"last_success_at"`
	LastError           *StateError `json:"last_error"`
	ConsecutiveFailures int         `json:"consecutive_failures"`
	NextAttemptAt       *time.Time  `json:"next_attempt_at"`
	// HighestSerial is the highest manifest serial verified; a lower one is
	// refused as a replay.
	HighestSerial int64            `json:"highest_serial"`
	Current       *ManifestSummary `json:"current"`
	Delivered     *Delivery        `json:"delivered"`
	Download      *DownloadState   `json:"download"`
}

// StateError is the last failure.
type StateError struct {
	At      time.Time `json:"at"`
	Code    string    `json:"code"`
	Message string    `json:"message"`
}

// ManifestSummary is a verified manifest.
type ManifestSummary struct {
	Serial         int64     `json:"serial"`
	EnvelopeSHA256 string    `json:"envelope_sha256"`
	SnapshotSHA256 string    `json:"snapshot_sha256"`
	SizeBytes      int64     `json:"size_bytes"`
	DataTimestamp  time.Time `json:"data_timestamp"`
	IssuedAt       time.Time `json:"issued_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	KeyID          string    `json:"key_id"`
	VerifiedAt     time.Time `json:"verified_at"`
}

// Delivery is the last snapshot handed to the publisher.
type Delivery struct {
	Name           string    `json:"name"`
	Serial         int64     `json:"serial"`
	SnapshotSHA256 string    `json:"snapshot_sha256"`
	SizeBytes      int64     `json:"size_bytes"`
	DataTimestamp  time.Time `json:"data_timestamp"`
	DeliveredAt    time.Time `json:"delivered_at"`
}

// DownloadState is the snapshot being downloaded.
type DownloadState struct {
	SnapshotSHA256 string     `json:"snapshot_sha256"`
	SizeBytes      int64      `json:"size_bytes"`
	Bytes          int64      `json:"bytes"`
	StartedAt      time.Time  `json:"started_at"`
	Attempts       int        `json:"attempts"`
	AbandonedUntil *time.Time `json:"abandoned_until"`
}

func summary(v *Verified, at time.Time) *ManifestSummary {
	m := v.Manifest
	return &ManifestSummary{Serial: m.Serial, EnvelopeSHA256: v.EnvelopeSHA256, SnapshotSHA256: m.Snapshot.SHA256,
		SizeBytes: m.Snapshot.SizeBytes, DataTimestamp: m.Snapshot.DataTimestamp.UTC(), IssuedAt: m.IssuedAt.UTC(),
		ExpiresAt: m.ExpiresAt.UTC(), KeyID: v.KeyID, VerifiedAt: at.UTC()}
}

// ReadState reads a fetcher state file (never through a symlink, bounded).
// A missing file is (nil, nil).
func ReadState(dir string) (*State, error) {
	b, err := readRegular(filepath.Join(dir, StateDirName, StateFileName), MaxStateBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := strictDecode(b, &s); err != nil {
		return nil, err
	}
	if s.Version != stateVersion {
		return nil, errors.New("unsupported fetcher state version")
	}
	return &s, nil
}
