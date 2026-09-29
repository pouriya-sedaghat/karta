// Package inbox implements the local publication inbox: a directory an
// operator (or a producer they run) copies OSM snapshots into.
//
// # Completion protocol
//
// A submission named NAME consists of
//
//	NAME.osm.pbf                  the snapshot
//	NAME.osm.pbf.provenance.json  optional provenance sidecar
//	NAME.osm.pbf.ready            completion marker, written last
//
// NAME must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}. Producers copy each file
// under a temporary name that does not end in one of these suffixes (for
// example a leading dot, or ".part"), rename it into place, and create the
// marker last, also by rename. The marker holds the snapshot's SHA-256 as
// 64 hex digits, optionally followed by the file name (the output of
// `sha256sum NAME.osm.pbf`).
//
// Until the marker exists, the files are only listed and lstat'ed, never
// opened. After it appears, all of a submission's files must stay unchanged
// (same size, modification time and inode) for a settle interval. The
// snapshot and sidecar are then copied into a private staging directory
// while being hashed; the copy is used only if the source did not change
// during the copy and its digest equals the marker's. Everything after that
// reads only the staged copy, so the bytes validated are the bytes imported.
//
// Symlinks, directories, devices and FIFOs under a submission name are
// rejected from the lstat result without being opened; files are opened with
// O_NOFOLLOW|O_NONBLOCK and the opened file must be the one listed.
//
// The inbox is never written to: outcomes are recorded in the registry,
// keyed by the submission's fingerprint (names, sizes, modification times and
// inodes of its files), so an unchanged submission is not processed twice,
// including after a restart, and a changed one is a new submission.
package inbox

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

// File suffixes of the protocol.
const (
	SnapshotSuffix = ".osm.pbf"
	SidecarSuffix  = ".osm.pbf.provenance.json"
	MarkerSuffix   = ".osm.pbf.ready"
)

// Limits.
const (
	MaxMarkerBytes  = 512
	MaxSidecarBytes = 1 << 20
)

// NamePattern restricts submission names.
var NamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Rejection codes (also used as registry reason codes).
const (
	CodeInvalidName       = "invalid_name"
	CodeSymlink           = "symlink"
	CodeNotRegular        = "not_regular_file"
	CodeInvalidMarker     = "invalid_marker"
	CodeDigestMismatch    = "marker_digest_mismatch"
	CodeChanged           = "changed_during_copy"
	CodeTooLarge          = "too_large"
	CodeEmpty             = "empty_file"
	CodeInsufficientSpace = "insufficient_storage"
	CodeIO                = "io_error"
)

// Rejection is a submission problem with a stable code.
type Rejection struct {
	Code string
	Msg  string
}

func (r *Rejection) Error() string { return r.Code + ": " + r.Msg }

func reject(code, format string, args ...any) error {
	return &Rejection{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// CodeOf returns the rejection code of err, or "" if it is not a Rejection.
func CodeOf(err error) string {
	var r *Rejection
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

// FileState is what lstat reports about one file of a submission.
type FileState struct {
	Size    int64
	ModTime time.Time
	Dev     uint64
	Ino     uint64
	Mode    fs.FileMode
}

func (f *FileState) key() string {
	if f == nil {
		return "-"
	}
	return fmt.Sprintf("%d:%d:%d:%d:%o", f.Size, f.ModTime.UnixNano(), f.Dev, f.Ino, uint32(f.Mode))
}

// Entry is one submission as listed.
type Entry struct {
	Name     string
	Snapshot *FileState
	Sidecar  *FileState
	Marker   *FileState
	// Problem is a rejection code for a submission that is structurally
	// invalid (symlink, not a regular file, bad name); ProblemDetail says why.
	Problem       string
	ProblemDetail string
}

// Complete reports whether the producer has declared the submission complete.
func (e Entry) Complete() bool { return e.Marker != nil }

// Fingerprint identifies this exact set of files.
func (e Entry) Fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "karta-inbox/1\x00%s\x00%s\x00%s\x00%s", e.Name, e.Snapshot.key(), e.Sidecar.key(), e.Marker.key())
	return hex.EncodeToString(h.Sum(nil))
}

// Waiting describes why an incomplete entry is not processed yet.
func (e Entry) Waiting() string {
	switch {
	case e.Marker == nil:
		return "waiting_for_ready_marker"
	case e.Snapshot == nil:
		return "waiting_for_snapshot"
	}
	return ""
}

func stateOf(fi fs.FileInfo) *FileState {
	fs := &FileState{Size: fi.Size(), ModTime: fi.ModTime(), Mode: fi.Mode()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		fs.Dev, fs.Ino = uint64(st.Dev), st.Ino // #nosec G115 -- device numbers are non-negative
	}
	return fs
}

// Scan lists the submissions in dir, in name order, without opening any
// file. At most limit directory entries are considered; more is an error so
// a flooded inbox is reported rather than scanned partially.
func Scan(dir string, limit int) ([]Entry, error) {
	d, err := os.Open(dir) // #nosec G304 -- the configured inbox directory
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(limit + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(names) > limit {
		return nil, fmt.Errorf("inbox holds more than %d entries; remove processed submissions", limit)
	}
	byName := map[string]*Entry{}
	for _, n := range names {
		if strings.HasPrefix(n, ".") {
			continue // hidden and temporary files are the producer's business
		}
		var base string
		var slot func(*Entry) **FileState
		switch {
		case strings.HasSuffix(n, MarkerSuffix):
			base, slot = strings.TrimSuffix(n, MarkerSuffix), func(e *Entry) **FileState { return &e.Marker }
		case strings.HasSuffix(n, SidecarSuffix):
			base, slot = strings.TrimSuffix(n, SidecarSuffix), func(e *Entry) **FileState { return &e.Sidecar }
		case strings.HasSuffix(n, SnapshotSuffix):
			base, slot = strings.TrimSuffix(n, SnapshotSuffix), func(e *Entry) **FileState { return &e.Snapshot }
		default:
			continue
		}
		e := byName[base]
		if e == nil {
			e = &Entry{Name: base}
			byName[base] = e
		}
		fi, err := os.Lstat(filepath.Join(dir, n))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // renamed or removed since the listing
			}
			return nil, err
		}
		st := stateOf(fi)
		*slot(e) = st
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			e.setProblem(CodeSymlink, n+" is a symbolic link")
		case !fi.Mode().IsRegular():
			e.setProblem(CodeNotRegular, n+" is not a regular file ("+fi.Mode().Type().String()+")")
		}
	}
	out := make([]Entry, 0, len(byName))
	for _, e := range byName {
		if !NamePattern.MatchString(e.Name) {
			e.setProblem(CodeInvalidName, fmt.Sprintf("submission name %q must match %s", e.Name, NamePattern))
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (e *Entry) setProblem(code, detail string) {
	if e.Problem == "" {
		e.Problem, e.ProblemDetail = code, detail
	}
}

// Settler tracks when each submission's fingerprint last changed.
type Settler struct {
	seen map[string]settle
}

type settle struct {
	fingerprint string
	since       time.Time
}

// NewSettler returns an empty tracker.
func NewSettler() *Settler { return &Settler{seen: map[string]settle{}} }

// Stable reports whether e has kept its fingerprint for at least d, and
// records it. Entries no longer listed are forgotten by Forget.
func (s *Settler) Stable(e Entry, now time.Time, d time.Duration) bool {
	fp := e.Fingerprint()
	cur, ok := s.seen[e.Name]
	if !ok || cur.fingerprint != fp {
		s.seen[e.Name] = settle{fingerprint: fp, since: now}
		return d <= 0
	}
	return now.Sub(cur.since) >= d
}

// Forget drops entries that are not in the current listing.
func (s *Settler) Forget(current []Entry) {
	keep := map[string]bool{}
	for _, e := range current {
		keep[e.Name] = true
	}
	for n := range s.seen {
		if !keep[n] {
			delete(s.seen, n)
		}
	}
}

var markerPattern = regexp.MustCompile(`^([0-9a-fA-F]{64})(?:[ \t]+\*?([^\s]+))?\s*$`)

// ParseMarker returns the digest in marker content for snapshot file name.
func ParseMarker(b []byte, snapshotName string) (string, error) {
	m := markerPattern.FindSubmatch(b)
	if m == nil {
		return "", reject(CodeInvalidMarker, "the ready marker must hold the snapshot's SHA-256 (64 hex digits), optionally followed by its file name")
	}
	if len(m[2]) > 0 && string(m[2]) != snapshotName {
		return "", reject(CodeInvalidMarker, "the ready marker names %q, not %q", m[2], snapshotName)
	}
	return strings.ToLower(string(m[1])), nil
}

// openListed opens name in dir without following symlinks or blocking, and
// checks it is the regular file the listing recorded.
func openListed(dir, name string, want *FileState) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // #nosec G304 -- validated name inside the configured inbox
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, reject(CodeSymlink, "%s became a symbolic link", name)
		}
		return nil, reject(CodeChanged, "%s could not be opened: %v", name, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, reject(CodeIO, "%s: %v", name, err)
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, reject(CodeNotRegular, "%s is not a regular file", name)
	}
	if got := stateOf(fi); got.key() != want.key() {
		f.Close()
		return nil, reject(CodeChanged, "%s changed after it was listed", name)
	}
	return f, nil
}

// Staged is a submission copied into private staging.
type Staged struct {
	Dir          string
	SnapshotPath string
	// SidecarPath is "" when the submission has no sidecar.
	SidecarPath  string
	SHA256       string
	Size         int64
	MarkerSHA256 string
}

// Options bound a staging copy.
type Options struct {
	MaxSnapshotBytes int64
	// ReserveBytes must remain free on the staging filesystem after the copy.
	ReserveBytes int64
}

// Stage copies a complete submission into a new directory under stagingDir
// (created 0700, files 0600) and verifies it. On any error the staging
// directory is removed.
func Stage(dir string, e Entry, stagingDir, id string, opts Options) (st *Staged, err error) {
	if e.Problem != "" {
		return nil, reject(e.Problem, "%s", e.ProblemDetail)
	}
	if e.Marker == nil || e.Snapshot == nil {
		return nil, fmt.Errorf("submission %s is not complete", e.Name)
	}
	snapName := e.Name + SnapshotSuffix
	if e.Snapshot.Size == 0 {
		return nil, reject(CodeEmpty, "%s is empty", snapName)
	}
	if e.Snapshot.Size > opts.MaxSnapshotBytes {
		return nil, reject(CodeTooLarge, "%s is %d bytes, above the limit of %d", snapName, e.Snapshot.Size, opts.MaxSnapshotBytes)
	}
	if e.Marker.Size > MaxMarkerBytes {
		return nil, reject(CodeInvalidMarker, "the ready marker is larger than %d bytes", MaxMarkerBytes)
	}
	if e.Sidecar != nil && e.Sidecar.Size > MaxSidecarBytes {
		return nil, reject(CodeTooLarge, "the provenance sidecar is larger than %d bytes", MaxSidecarBytes)
	}
	need := e.Snapshot.Size + opts.ReserveBytes
	if e.Sidecar != nil {
		need += e.Sidecar.Size
	}
	if free, ferr := FreeBytes(stagingDir); ferr == nil && free < need {
		return nil, reject(CodeInsufficientSpace, "staging has %d bytes free; the submission needs %d plus a reserve of %d",
			free, need-opts.ReserveBytes, opts.ReserveBytes)
	}

	marker, err := readSmall(dir, e.Name+MarkerSuffix, e.Marker, MaxMarkerBytes)
	if err != nil {
		return nil, err
	}
	markerDigest, err := ParseMarker(marker, snapName)
	if err != nil {
		return nil, err
	}

	jobDir := filepath.Join(stagingDir, id)
	if err := os.Mkdir(jobDir, 0o700); err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(jobDir)
		}
	}()
	st = &Staged{Dir: jobDir, SnapshotPath: filepath.Join(jobDir, "snapshot"+SnapshotSuffix), MarkerSHA256: markerDigest}
	if e.Sidecar != nil {
		b, err := readSmall(dir, e.Name+SidecarSuffix, e.Sidecar, MaxSidecarBytes)
		if err != nil {
			return nil, err
		}
		st.SidecarPath = filepath.Join(jobDir, "snapshot"+SidecarSuffix)
		if err := writeNew(st.SidecarPath, b); err != nil {
			return nil, err
		}
	}
	st.SHA256, st.Size, err = copyVerified(dir, snapName, e.Snapshot, st.SnapshotPath, opts.MaxSnapshotBytes)
	if err != nil {
		return nil, err
	}
	if st.SHA256 != markerDigest {
		return nil, reject(CodeDigestMismatch, "%s has SHA-256 %s but its ready marker says %s", snapName, st.SHA256, markerDigest)
	}
	return st, nil
}

func readSmall(dir, name string, want *FileState, limit int64) ([]byte, error) {
	f, err := openListed(dir, name, want)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, reject(CodeIO, "%s: %v", name, err)
	}
	if int64(len(b)) != want.Size {
		return nil, reject(CodeChanged, "%s changed while it was read", name)
	}
	return b, nil
}

func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600) // #nosec G304 -- inside our staging directory
	if err != nil {
		return classifyWrite(err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return classifyWrite(err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return classifyWrite(err)
	}
	return classifyWrite(f.Close())
}

func classifyWrite(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return reject(CodeInsufficientSpace, "staging filesystem full: %v", err)
	}
	return fmt.Errorf("write staging copy: %w", err)
}

// copyVerified copies the listed snapshot while hashing it, and checks that
// it did not change during the copy.
func copyVerified(dir, name string, want *FileState, dst string, limit int64) (string, int64, error) {
	src, err := openListed(dir, name, want)
	if err != nil {
		return "", 0, err
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600) // #nosec G304 -- inside our staging directory
	if err != nil {
		return "", 0, classifyWrite(err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(src, limit+1))
	if err != nil {
		out.Close()
		var pe *fs.PathError
		if errors.As(err, &pe) && pe.Path == src.Name() {
			return "", 0, reject(CodeIO, "read %s: %v", name, err)
		}
		return "", 0, classifyWrite(err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return "", 0, classifyWrite(err)
	}
	if err := out.Close(); err != nil {
		return "", 0, classifyWrite(err)
	}
	if n != want.Size {
		return "", 0, reject(CodeChanged, "%s was %d bytes when listed but %d were copied", name, want.Size, n)
	}
	// The same file (inode), still the same size and modification time, and
	// the name still refers to it.
	after, err := src.Stat()
	if err != nil || stateOf(after).key() != want.key() {
		return "", 0, reject(CodeChanged, "%s changed while it was copied", name)
	}
	if fi, err := os.Lstat(filepath.Join(dir, name)); err != nil || stateOf(fi).key() != want.key() {
		return "", 0, reject(CodeChanged, "%s was replaced while it was copied", name)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// FreeBytes returns the space available to unprivileged users on the
// filesystem holding path.
func FreeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil // #nosec G115 -- block counts fit in int64
}

// CleanStaging removes every entry in stagingDir except keep; staging
// directories left by an interrupted process are never reused.
func CleanStaging(stagingDir string, keep map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		if keep[e.Name()] || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(stagingDir, e.Name())); err != nil {
			return removed, err
		}
		removed = append(removed, e.Name())
	}
	return removed, nil
}

// StageFile copies an operator-named snapshot (and optional sidecar) into a
// new staging directory for the command-line import. There is no ready
// marker (the operator names a complete file); otherwise it applies the
// same checks as Stage: regular files only, never through a symlink, and
// unchanged while copied. The staged snapshot keeps its .osm.pbf or .osm
// suffix.
func StageFile(snapshotPath, sidecarPath, stagingDir, id string, opts Options) (st *Staged, err error) {
	dir, name := filepath.Split(snapshotPath)
	if dir == "" {
		dir = "."
	}
	suffix := SnapshotSuffix
	if !strings.HasSuffix(name, SnapshotSuffix) {
		if !strings.HasSuffix(name, ".osm") {
			return nil, reject(CodeInvalidName, "snapshot file name must end in .osm.pbf or .osm")
		}
		suffix = ".osm"
	}
	snap, err := lstatRegular(snapshotPath)
	if err != nil {
		return nil, err
	}
	if snap.Size == 0 {
		return nil, reject(CodeEmpty, "%s is empty", name)
	}
	if snap.Size > opts.MaxSnapshotBytes {
		return nil, reject(CodeTooLarge, "%s is %d bytes, above the limit of %d", name, snap.Size, opts.MaxSnapshotBytes)
	}
	var side *FileState
	if sidecarPath != "" {
		if side, err = lstatRegular(sidecarPath); err != nil {
			return nil, err
		}
		if side.Size > MaxSidecarBytes {
			return nil, reject(CodeTooLarge, "the provenance sidecar is larger than %d bytes", MaxSidecarBytes)
		}
	}
	if free, ferr := FreeBytes(stagingDir); ferr == nil && free < snap.Size+opts.ReserveBytes {
		return nil, reject(CodeInsufficientSpace, "staging has %d bytes free; the snapshot needs %d plus a reserve of %d", free, snap.Size, opts.ReserveBytes)
	}
	jobDir := filepath.Join(stagingDir, id)
	if err := os.Mkdir(jobDir, 0o700); err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(jobDir)
		}
	}()
	st = &Staged{Dir: jobDir, SnapshotPath: filepath.Join(jobDir, "snapshot"+suffix)}
	if side != nil {
		sdir, sname := filepath.Split(sidecarPath)
		if sdir == "" {
			sdir = "."
		}
		b, err := readSmall(sdir, sname, side, MaxSidecarBytes)
		if err != nil {
			return nil, err
		}
		st.SidecarPath = st.SnapshotPath + ".provenance.json"
		if err := writeNew(st.SidecarPath, b); err != nil {
			return nil, err
		}
	}
	st.SHA256, st.Size, err = copyVerified(dir, name, snap, st.SnapshotPath, opts.MaxSnapshotBytes)
	if err != nil {
		return nil, err
	}
	return st, nil
}

func lstatRegular(path string) (*FileState, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, reject(CodeIO, "%v", err)
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return nil, reject(CodeSymlink, "%s is a symbolic link", path)
	case !fi.Mode().IsRegular():
		return nil, reject(CodeNotRegular, "%s is not a regular file", path)
	}
	return stateOf(fi), nil
}
