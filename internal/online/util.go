package online

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"unicode/utf8"
)

// maxJSONDepth bounds the nesting of decoded documents.
const maxJSONDepth = 16

// strictDecode decodes one JSON value into v: valid UTF-8, no repeated
// object keys at any depth (two readers could otherwise see different
// values), bounded nesting, no unknown fields and nothing after the value.
func strictDecode(b []byte, v any) error {
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
	return walkValue(dec, 0)
}

func walkValue(dec *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return fmt.Errorf("nested deeper than %d levels", maxJSONDepth)
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
			if err := walkValue(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := walkValue(dec, depth+1); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token() // closing delimiter
	return err
}

// openNoFollow opens path for reading without following a final symlink
// and without blocking on a FIFO.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // #nosec G304 -- configured paths and our own directories
}

func limitReader(r io.Reader, n int64) io.Reader { return io.LimitReader(r, n) }

func decodeB64(s string) ([]byte, error) { return base64.StdEncoding.Strict().DecodeString(s) }

func marshalIndent(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
