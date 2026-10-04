// Package safefile holds the small file-handling rules the Stage 5 intake
// and bridge processes share: reading bounded regular files without
// following symlinks or blocking on FIFOs, durable atomic replacement
// (temporary file, fsync, rename, directory fsync), moves and writes that
// never replace an existing file, and strict JSON
// decoding (no unknown fields, no repeated keys, nothing after the value).
package safefile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"unicode/utf8"
)

// OpenNoFollow opens path for reading without following a final symlink and
// without blocking on a FIFO.
func OpenNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // #nosec G304 -- callers pass configured paths and their own directories
}

// ReadRegular reads a regular file of at most max bytes, never through a
// symlink.
func ReadRegular(path string, max int64) ([]byte, error) {
	f, err := OpenNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if st.Size() > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(io.LimitReader(f, max+1)); err != nil {
		return nil, err
	}
	if int64(buf.Len()) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	return buf.Bytes(), nil
}

// SyncDir flushes a directory's entries (after a rename into it).
func SyncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- the caller's own directory
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// WriteAtomic durably replaces path with b: a temporary file in the same
// directory is written, synced and given mode, renamed over path, and the
// directory is synced. A reader sees the old or the new content, never a
// part.
func WriteAtomic(path string, b []byte, mode os.FileMode) error {
	tmp, err := writeTemp(path, b, mode)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// ErrExists is a destination that already exists: RenameNoReplace and
// WriteNew never replace a file.
var ErrExists = errors.New("the destination already exists")

// WriteNew durably writes b to path like WriteAtomic, but path must not exist
// yet (ErrExists): the temporary file is linked into place, never renamed
// over an existing one.
func WriteNew(path string, b []byte, mode os.FileMode) error {
	tmp, err := writeTemp(path, b, mode)
	if err != nil {
		return err
	}
	if err := os.Link(tmp, path); err != nil {
		_ = os.Remove(tmp)
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s: %w", path, ErrExists)
		}
		return err
	}
	if err := os.Remove(tmp); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// RenameNoReplace moves src to dst in the same filesystem and fails with
// ErrExists if dst exists: dst is linked to src, then src is removed, so an
// existing dst is never replaced. A crash between the two leaves both names
// on one file; a repeat finds that dst is src and only removes src.
func RenameNoReplace(src, dst string) error {
	if err := os.Link(src, dst); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		a, aerr := os.Lstat(src)
		b, berr := os.Lstat(dst)
		if aerr != nil || berr != nil || !os.SameFile(a, b) {
			return fmt.Errorf("%s: %w", dst, ErrExists)
		}
	}
	return os.Remove(src)
}

// writeTemp writes b to a synced temporary file next to path, with mode.
func writeTemp(path string, b []byte, mode os.FileMode) (string, error) {
	tmp := filepath.Join(filepath.Dir(path), ".tmp-"+filepath.Base(path))
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode) // #nosec G304 -- the caller's own directory
	if err != nil {
		return "", err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Chmod(tmp, mode); err != nil { // independent of the umask
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// MaxJSONDepth bounds the nesting of strictly decoded documents.
const MaxJSONDepth = 16

// StrictDecode decodes one JSON value into v: valid UTF-8, no repeated
// object keys at any depth, bounded nesting, no unknown fields and nothing
// after the value. A leading UTF-8 byte-order mark (as Windows tools write)
// is ignored.
func StrictDecode(b []byte, v any) error {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(b) {
		return errors.New("not valid UTF-8")
	}
	if err := noDuplicateKeys(b); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("data after the JSON value")
	}
	return nil
}

func noDuplicateKeys(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return walk(dec, 0)
}

func walk(dec *json.Decoder, depth int) error {
	if depth > MaxJSONDepth {
		return fmt.Errorf("nested deeper than %d levels", MaxJSONDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			k, _ := kt.(string)
			if seen[k] {
				return fmt.Errorf("repeated key %q", k)
			}
			seen[k] = true
			if err := walk(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := walk(dec, depth+1); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token()
	return err
}

// MarshalIndent encodes v as indented JSON with a final newline.
func MarshalIndent(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// SHA256Hex is the hex SHA-256 of b.
func SHA256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Lock takes an exclusive, non-blocking flock on path (created 0600) for the
// life of the returned file.
func Lock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600) // #nosec G304 -- the caller's own directory
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { // #nosec G115 -- file descriptors are small
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// IsNoSpace reports a full filesystem or quota.
func IsNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
