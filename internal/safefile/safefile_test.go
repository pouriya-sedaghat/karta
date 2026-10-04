package safefile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestStrictDecode(t *testing.T) {
	type obj struct {
		A string `json:"a"`
		B struct {
			C int `json:"c"`
		} `json:"b"`
	}
	for name, c := range map[string]struct {
		in string
		ok bool
	}{
		"plain":               {`{"a": "x", "b": {"c": 1}}`, true},
		"byte-order mark":     {"\xef\xbb\xbf{\"a\": \"x\"}\r\n", true},
		"unknown field":       {`{"a": "x", "z": 1}`, false},
		"repeated key":        {`{"a": "x", "a": "y"}`, false},
		"repeated nested key": {`{"b": {"c": 1, "c": 2}}`, false},
		"trailing data":       {`{"a": "x"} {"a": "y"}`, false},
		"not an object":       {`["a"]`, false},
		"truncated":           {`{"a": "x"`, false},
	} {
		var v obj
		if err := StrictDecode([]byte(c.in), &v); (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Repeated keys are found at any depth, also inside arrays.
func TestNoDuplicateKeys(t *testing.T) {
	if err := noDuplicateKeys([]byte(`{"l": [{"k": 1}, {"k": 2}], "m": {"k": [1, {"x": 1}]}}`)); err != nil {
		t.Errorf("distinct keys refused: %v", err)
	}
	for _, in := range []string{`{"l": [{"k": 1, "k": 2}]}`, `[[{"a": 1, "a": 1}]]`, `{"m": {"n": {"o": 1, "o": 2}}}`} {
		if err := noDuplicateKeys([]byte(in)); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
}

func TestReadRegularAndOpenNoFollow(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadRegular(f, 5); err != nil || string(b) != "12345" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := ReadRegular(f, 4); err == nil {
		t.Error("a file above the bound was read")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(f, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRegular(link, 5); err == nil {
		t.Error("read through a symlink")
	}
	if fh, err := OpenNoFollow(link); err == nil {
		fh.Close()
		t.Error("opened through a symlink")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRegular(fifo, 5); err == nil {
		t.Error("read a FIFO (and did not block)")
	}
	if _, err := ReadRegular(filepath.Join(dir, "missing"), 5); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
}

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	for _, content := range []string{"one", "two"} {
		if err := WriteAtomic(p, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p)
		fi, _ := os.Stat(p)
		if string(b) != content || fi.Mode().Perm() != 0o640 {
			t.Fatalf("%q %v", b, fi.Mode())
		}
	}
	es, _ := os.ReadDir(dir)
	if len(es) != 1 {
		var names []string
		for _, e := range es {
			names = append(names, e.Name())
		}
		t.Fatalf("temporary files left: %s", strings.Join(names, " "))
	}
}

func TestLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lock")
	a, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := Lock(p); err == nil {
		b.Close()
		t.Fatal("a second holder took the lock")
	}
	a.Close()
	b, err := Lock(p)
	if err != nil {
		t.Fatalf("the released lock: %v", err)
	}
	b.Close()
}

func TestNeverReplace(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			return "missing"
		}
		return string(b)
	}
	// A move to a free name.
	src, dst := write("a", "mine"), filepath.Join(dir, "b")
	if err := RenameNoReplace(src, dst); err != nil || read(dst) != "mine" || read(src) != "missing" {
		t.Fatalf("move: %v %q %q", err, read(dst), read(src))
	}
	// An existing destination is never replaced, and the source stays.
	src, other := write("c", "mine"), write("d", "theirs")
	if err := RenameNoReplace(src, other); !errors.Is(err, ErrExists) || read(other) != "theirs" || read(src) != "mine" {
		t.Fatalf("over an existing file: %v %q %q", err, read(other), read(src))
	}
	// Nor through a symlink at the destination.
	link := filepath.Join(dir, "l")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if err := RenameNoReplace(src, link); !errors.Is(err, ErrExists) || read(other) != "theirs" {
		t.Fatalf("over a symlink: %v %q", err, read(other))
	}
	// A crash between link and unlink: both names are one file, and a
	// repeat completes the move.
	src = write("e", "mine")
	half := filepath.Join(dir, "f")
	if err := os.Link(src, half); err != nil {
		t.Fatal(err)
	}
	if err := RenameNoReplace(src, half); err != nil || read(half) != "mine" || read(src) != "missing" {
		t.Fatalf("repeat after a crash: %v", err)
	}
	// WriteNew writes a new file, and refuses an existing one.
	fresh := filepath.Join(dir, "g")
	if err := WriteNew(fresh, []byte("new"), 0o644); err != nil || read(fresh) != "new" {
		t.Fatalf("new file: %v", err)
	}
	if err := WriteNew(other, []byte("new"), 0o644); !errors.Is(err, ErrExists) || read(other) != "theirs" {
		t.Fatalf("over an existing file: %v %q", err, read(other))
	}
	if _, err := os.Lstat(filepath.Join(dir, ".tmp-d")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file left: %v", err)
	}
}
