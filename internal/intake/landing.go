package intake

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// NamePattern restricts delivery names (as the inbox).
var NamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Entry refusal and waiting codes.
const (
	CodeInvalidName       = "invalid_name"
	CodeSymlink           = "symlink"
	CodeNotRegular        = "not_regular_file"
	CodeHardLink          = "hard_link"
	CodeWrongOwner        = "wrong_owner"
	CodeUnsafeMode        = "unsafe_mode"
	CodeTooLarge          = "too_large"
	CodeEmpty             = "empty_file"
	CodeInvalidCompletion = "invalid_completion"
	CodeStaleCompletion   = "completion_stale"
	CodeSizeMismatch      = "size_mismatch"
	CodeDigestMismatch    = "digest_mismatch"
	CodeChanged           = "changed_during_copy"
	CodeRegionMismatch    = "region_mismatch"
	CodeMalformed         = "malformed_snapshot"
	CodeTimestampMissing  = "timestamp_missing"
	CodeStorage           = "insufficient_storage"

	WaitingForCompletion = "waiting_for_completion_marker"
	WaitingSettling      = "settling"
	WaitingQueueFull     = "queue_full"
	StateRefused         = "refused"
	StateDelivered       = "delivered"
)

// maxLandingEntries bounds a landing scan; more is reported, not scanned
// partially.
const maxLandingEntries = 1000

// fileState is what lstat reports about one landing file.
type fileState struct {
	Size    int64
	ModTime time.Time
	Dev     uint64
	Ino     uint64
	Mode    fs.FileMode
	UID     uint32
	Nlink   uint64
}

func (f *fileState) key() string {
	if f == nil {
		return "-"
	}
	return fmt.Sprintf("%d:%d:%d:%d:%o:%d:%d", f.Size, f.ModTime.UnixNano(), f.Dev, f.Ino, uint32(f.Mode), f.UID, f.Nlink)
}

func stateOf(fi fs.FileInfo) *fileState {
	s := &fileState{Size: fi.Size(), ModTime: fi.ModTime(), Mode: fi.Mode()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		s.Dev, s.Ino, s.UID, s.Nlink = uint64(st.Dev), st.Ino, st.Uid, uint64(st.Nlink) // #nosec G115 -- device and link counts are non-negative
	}
	return s
}

// landingEntry is one delivery in the landing area.
type landingEntry struct {
	Name       string
	Snapshot   *fileState
	Sidecar    *fileState
	Completion *fileState
	// Problem is a refusal from the listing alone (never opened).
	Problem, Detail string
}

func (e landingEntry) snapshotName() string { return e.Name + SnapshotSuffix }

// fingerprint identifies this exact set of files (owner and link count
// included, so fixing a refusal by chown or unlink makes a new one).
func (e landingEntry) fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "karta-landing/1\x00%s\x00%s\x00%s\x00%s", e.Name, e.Snapshot.key(), e.Sidecar.key(), e.Completion.key())
	return hex.EncodeToString(h.Sum(nil))
}

func (e *landingEntry) problem(code, format string, args ...any) {
	if e.Problem == "" {
		e.Problem, e.Detail = code, fmt.Sprintf(format, args...)
	}
}

// temporaryName reports names producers use while copying, which are the
// producer's business (sftp clients, WinSCP, rsync, cp scripts).
func temporaryName(n string) bool {
	return strings.HasPrefix(n, ".") || strings.HasSuffix(n, ".part") || strings.HasSuffix(n, ".filepart") ||
		strings.HasSuffix(n, ".tmp") || strings.HasSuffix(n, "~")
}

// scanLanding lists the deliveries of the landing area in name order without
// opening any file. Files with other names (a .sha256 or .md5 checksum, for
// example) are ignored: they are not completion signals.
func scanLanding(dir string, ownerUID uint32, maxSnapshot int64) ([]landingEntry, error) {
	d, err := os.Open(dir) // #nosec G304 -- the configured landing directory
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(maxLandingEntries + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(names) > maxLandingEntries {
		return nil, fmt.Errorf("the landing area holds more than %d entries; remove delivered files", maxLandingEntries)
	}
	by := map[string]*landingEntry{}
	for _, n := range names {
		if temporaryName(n) {
			continue
		}
		var base string
		var slot func(*landingEntry) **fileState
		switch {
		case strings.HasSuffix(n, CompletionSuffix):
			base, slot = strings.TrimSuffix(n, CompletionSuffix), func(e *landingEntry) **fileState { return &e.Completion }
		case strings.HasSuffix(n, SidecarSuffix):
			base, slot = strings.TrimSuffix(n, SidecarSuffix), func(e *landingEntry) **fileState { return &e.Sidecar }
		case strings.HasSuffix(n, SnapshotSuffix):
			base, slot = strings.TrimSuffix(n, SnapshotSuffix), func(e *landingEntry) **fileState { return &e.Snapshot }
		default:
			continue
		}
		fi, err := os.Lstat(filepath.Join(dir, n))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		e := by[base]
		if e == nil {
			e = &landingEntry{Name: base}
			by[base] = e
		}
		st := stateOf(fi)
		*slot(e) = st
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			e.problem(CodeSymlink, "%s is a symbolic link", n)
		case !fi.Mode().IsRegular():
			e.problem(CodeNotRegular, "%s is not a regular file (%s)", n, fi.Mode().Type())
		case st.Nlink != 1:
			e.problem(CodeHardLink, "%s has %d hard links; another path to the same file may be writable outside the landing area", n, st.Nlink)
		case st.UID != ownerUID:
			e.problem(CodeWrongOwner, "%s is owned by UID %d, not the landing owner %d", n, st.UID, ownerUID)
		case fi.Mode().Perm()&0o022 != 0:
			// Anyone else who can write the file could change it in place,
			// keeping its owner and inode.
			e.problem(CodeUnsafeMode, "%s is writable by its group or by others (mode %04o); deliveries must be writable by the landing owner "+
				"only (umask 022)", n, fi.Mode().Perm())
		}
	}
	out := make([]landingEntry, 0, len(by))
	for _, e := range by {
		if !NamePattern.MatchString(e.Name) {
			e.problem(CodeInvalidName, "delivery name %q must match %s", e.Name, NamePattern)
		}
		switch {
		case e.Snapshot != nil && e.Snapshot.Size == 0:
			e.problem(CodeEmpty, "%s is empty", e.snapshotName())
		case e.Snapshot != nil && e.Snapshot.Size > maxSnapshot:
			e.problem(CodeTooLarge, "%s is %d bytes, above the limit of %d", e.snapshotName(), e.Snapshot.Size, maxSnapshot)
		case e.Completion != nil && e.Completion.Size > MaxCompletionBytes:
			e.problem(CodeInvalidCompletion, "the completion marker is larger than %d bytes", MaxCompletionBytes)
		case e.Sidecar != nil && e.Sidecar.Size > MaxSidecarBytes:
			e.problem(CodeTooLarge, "the provenance sidecar is larger than %d bytes", MaxSidecarBytes)
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// openListed opens a landing file without following symlinks or blocking,
// and checks it is the regular file the listing recorded.
func openListed(path string, want *fileState) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // #nosec G304 -- a listed landing file
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, refuse(CodeSymlink, "%s became a symbolic link", filepath.Base(path))
		}
		return nil, refuse(CodeChanged, "%s could not be opened: %v", filepath.Base(path), err)
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || stateOf(fi).key() != want.key() {
		_ = f.Close()
		return nil, refuse(CodeChanged, "%s changed after it was listed", filepath.Base(path))
	}
	return f, nil
}

// readSmall reads a small listed landing file completely.
func readSmall(path string, want *fileState, max int64) ([]byte, error) {
	f, err := openListed(path, want)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, refuse(CodeChanged, "%s: %v", filepath.Base(path), err)
	}
	if int64(len(b)) != want.Size {
		return nil, refuse(CodeChanged, "%s changed while it was read", filepath.Base(path))
	}
	return b, nil
}

// Refusal is a delivery the intake refuses, with a stable code.
type Refusal struct {
	Code string
	Msg  string
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Msg }

func refuse(code, format string, args ...any) error {
	return &Refusal{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// RefusalCode returns the code of a refusal, or "".
func RefusalCode(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}
