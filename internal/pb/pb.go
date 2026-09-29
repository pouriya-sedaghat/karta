// Package pb walks protobuf messages with google.golang.org/protobuf's
// protowire, which rejects truncated or malformed input safely. It is used
// for the untrusted OSM PBF header and for inspecting vector tiles.
package pb

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
)

// Field is one decoded field: Varint for varint fields, Bytes for
// length-delimited fields (nil otherwise).
type Field struct {
	Num    protowire.Number
	Type   protowire.Type
	Varint uint64
	Bytes  []byte
}

// ErrMalformed reports a truncated or invalid message.
var ErrMalformed = errors.New("malformed protobuf message")

// Walk calls fn for every field of the message in b.
func Walk(b []byte, fn func(Field) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return fmt.Errorf("%w: %v", ErrMalformed, protowire.ParseError(n))
		}
		b = b[n:]
		f := Field{Num: num, Type: typ}
		switch typ {
		case protowire.VarintType:
			f.Varint, n = protowire.ConsumeVarint(b)
		case protowire.BytesType:
			f.Bytes, n = protowire.ConsumeBytes(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return fmt.Errorf("%w: %v", ErrMalformed, protowire.ParseError(n))
		}
		b = b[n:]
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}
