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
