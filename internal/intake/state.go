package intake

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/safefile"
)

// Hidden directory of the intake inside its handoff directory (the
// publisher's scan ignores names starting with a dot): the watcher's state
// file and the lock that serializes handoffs.
const (
	StateDirName  = ".intake"
	StateFileName = "state.json"
	LockFileName  = "lock"
	// MaxStateBytes bounds the state file the publisher reads.
	MaxStateBytes = 256 << 10
	stateVersion  = 1
	// maxConsumed bounds the landing fingerprints the watcher remembers.
	maxConsumed = 200
	// maxListed bounds the landing entries and handoffs reported.
	maxListed = 50
)

// State is the watcher's persisted state. The publisher reads it for
// operator status and metrics; it is the watcher's own report (the
// publisher's record of submissions and authorizations is the registry).
type State struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	// Landing is the landing directory as the watcher sees it.
	Landing    string     `json:"landing"`
	LastScanAt *time.Time `json:"last_scan_at"`
	// StoppedAt is set when the watcher stopped cleanly (a deliberate stop
	// or shutdown) and cleared by its next scan: a stopped watcher is not
	// overdue, a crashed or hung one is.
	StoppedAt *time.Time `json:"stopped_at"`
	Preflight Preflight  `json:"preflight"`
	// Entries are the landing deliveries not yet handed off (or refused).
	Entries []LandingEntry `json:"entries"`
	// Handoffs are this watcher's deliveries the publisher has or will
	// have, with their latest known outcome.
	Handoffs    []Handoff   `json:"handoffs"`
	LastOutcome *Handoff    `json:"last_outcome"`
	LastError   *StateError `json:"last_error"`
	// Consumed are the landing deliveries already handed off or refused, by
	// fingerprint, so the same files are not processed again.
	Consumed []Consumed `json:"consumed"`
}

// Consumed is one landing delivery the watcher is done with.
type Consumed struct {
	Fingerprint string    `json:"fingerprint"`
	Name        string    `json:"name"`
	Outcome     string    `json:"outcome"`
	Code        string    `json:"code,omitempty"`
	At          time.Time `json:"at"`
}

// Preflight is the result of the landing checks.
type Preflight struct {
	OK        bool      `json:"ok"`
	CheckedAt time.Time `json:"checked_at"`
	// FSType names the landing filesystem.
	FSType   string    `json:"fs_type"`
	Problems []Problem `json:"problems"`
}

// Problem is one preflight or entry refusal.
type Problem struct {
	Code   string `json:"code"`
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

// LandingEntry is one delivery in the landing area as the watcher last saw
// it.
type LandingEntry struct {
	Name string `json:"name"`
	// State is waiting_for_completion_marker, settling, queue_full,
	// refused or delivered.
	State     string `json:"state"`
	Code      string `json:"code,omitempty"`
	Detail    string `json:"detail,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
}

// Handoff is one delivery handed to the publisher.
type Handoff struct {
	Name            string    `json:"name"`
	From            string    `json:"from"`
	SHA256          string    `json:"sha256"`
	SizeBytes       int64     `json:"size_bytes"`
	AuthorizationID int64     `json:"authorization_id"`
	Channel         string    `json:"channel"`
	HandedOffAt     time.Time `json:"handed_off_at"`
	// LandingFingerprint identifies the landing files it came from (crash
	// recovery completes a handoff only while they are unchanged).
	LandingFingerprint string `json:"landing_fingerprint,omitempty"`
	// SubmissionState is the publisher's state of the submission ("" until
	// it has one), with its reason code and release.
	SubmissionState string     `json:"submission_state"`
	ReasonCode      string     `json:"reason_code,omitempty"`
	ReleaseID       string     `json:"release_id,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

// StateError is the last failure of the watcher itself.
type StateError struct {
	At      time.Time `json:"at"`
	Code    string    `json:"code"`
	Message string    `json:"message"`
}

// ReadState reads the watcher state in a handoff directory (bounded, never
// through a symlink). A missing file is (nil, nil).
func ReadState(handoffDir string) (*State, error) {
	b, err := safefile.ReadRegular(filepath.Join(handoffDir, StateDirName, StateFileName), MaxStateBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := safefile.StrictDecode(b, &s); err != nil {
		return nil, fmt.Errorf("intake state: %w", err)
	}
	if s.Version != stateVersion {
		return nil, errors.New("unsupported intake state version")
	}
	return &s, nil
}

func writeState(handoffDir string, s *State) error {
	s.Version = stateVersion
	if len(s.Consumed) > maxConsumed {
		s.Consumed = s.Consumed[len(s.Consumed)-maxConsumed:]
	}
	if len(s.Entries) > maxListed {
		s.Entries = s.Entries[:maxListed]
	}
	if len(s.Handoffs) > maxListed {
		s.Handoffs = s.Handoffs[len(s.Handoffs)-maxListed:]
	}
	b, err := safefile.MarshalIndent(s)
	if err != nil {
		return err
	}
	return safefile.WriteAtomic(filepath.Join(handoffDir, StateDirName, StateFileName), b, 0o644)
}
