package glyphs

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/pouriya-sedaghat/karta/internal/pb"
)

// decoded mirrors MapLibre's glyph PBF reader (readFontstack/readGlyph).
type decoded struct {
	name, rng string
	glyphs    map[int]Glyph
}

func decode(t *testing.T, b []byte) decoded {
	t.Helper()
	out := decoded{glyphs: map[int]Glyph{}}
	err := pb.Walk(b, func(top pb.Field) error {
		if top.Num != 1 {
			return fmt.Errorf("top-level field %d", top.Num)
		}
		return pb.Walk(top.Bytes, func(f pb.Field) error {
			switch f.Num {
			case 1:
				out.name = string(f.Bytes)
			case 2:
				out.rng = string(f.Bytes)
			case 3:
				var g Glyph
				if err := pb.Walk(f.Bytes, func(gf pb.Field) error {
					switch gf.Num {
					case 1:
						g.ID = uint32(gf.Varint)
					case 2:
						g.Bitmap = gf.Bytes
					case 3:
						g.Width = uint32(gf.Varint)
					case 4:
						g.Height = uint32(gf.Varint)
					case 5:
						g.Left = int32(protowire.DecodeZigZag(gf.Varint))
					case 6:
						g.Top = int32(protowire.DecodeZigZag(gf.Varint))
					case 7:
						g.Advance = uint32(gf.Varint)
					}
					return nil
				}); err != nil {
					return err
				}
				out.glyphs[int(g.ID)] = g
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLatinRange(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Range("Vazirmatn Regular", 0, 255)
	if err != nil {
		t.Fatal(err)
	}
	d := decode(t, b)
	if d.name != "Vazirmatn Regular" || d.rng != "0-255" {
		t.Fatalf("header = %q %q", d.name, d.rng)
	}
	space, ok := d.glyphs[' ']
	if !ok || space.Bitmap != nil || space.Advance <= 0 {
		t.Fatalf("space glyph = %+v", space)
	}
	h, ok := d.glyphs['H']
	if !ok {
		t.Fatal("no glyph for H")
	}
	if len(h.Bitmap) != int((h.Width+6)*(h.Height+6)) {
		t.Fatalf("H bitmap %d bytes for %dx%d", len(h.Bitmap), h.Width, h.Height)
	}
	// Cap height of a 24 px em is roughly 15-19 px; the glyph sits above the
	// baseline, so its top is a few pixels below the line origin (negative).
	if h.Height < 14 || h.Height > 20 || h.Top >= 0 || h.Advance < 12 || h.Advance > 20 {
		t.Fatalf("implausible H metrics %+v", h)
	}
	// Signed distance: the centre of the left stem is inside (>191); the
	// border corner, about 4 px from the outline, is clearly outside.
	w := int(h.Width) + 6
	stem := h.Bitmap[(int(h.Height)/2+3)*w+3+1]
	if stem <= 191 {
		t.Errorf("stem pixel %d, want inside (>191)", stem)
	}
	if h.Bitmap[0] >= 100 {
		t.Errorf("corner pixel %d, want outside (<100)", h.Bitmap[0])
	}
}

func TestPersianPresentationForms(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	// Persian-specific letters: peh, tcheh, keheh, gaf, farsi yeh (Forms-A);
	// alef, beh, heh and others (Forms-B); lam-alef ligature.
	cases := map[int][]int{
		0xFB00: {0xFB56, 0xFB57, 0xFB58, 0xFB59, 0xFB7A, 0xFB7D, 0xFB8E, 0xFB92, 0xFB95, 0xFBFC, 0xFBFF},
		0xFE00: {0xFE8D, 0xFE8F, 0xFE91, 0xFEE9, 0xFEEC, 0xFEFB},
		0x0600: {0x0627, 0x067E, 0x0686, 0x06A9, 0x06AF, 0x06CC, 0x06F5},
		0x2000: {0x200C},
	}
	for _, stack := range s.Fontstacks() {
		for start, cps := range cases {
			b, err := s.Range(stack, start, start+255)
			if err != nil {
				t.Fatal(err)
			}
			d := decode(t, b)
			for _, cp := range cps {
				g, ok := d.glyphs[cp]
				if !ok {
					t.Errorf("%s: missing U+%04X", stack, cp)
					continue
				}
				if cp != 0x200C && (g.Bitmap == nil || len(g.Bitmap) != int((g.Width+6)*(g.Height+6))) {
					t.Errorf("%s: U+%04X bitmap %d for %dx%d", stack, cp, len(g.Bitmap), g.Width, g.Height)
				}
			}
		}
	}
}

func TestEmptyAndInvalidRanges(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Range("Vazirmatn Regular", 0x4E00, 0x4EFF) // CJK: not in the font
	if err != nil {
		t.Fatal(err)
	}
	if d := decode(t, b); len(d.glyphs) != 0 || d.rng != "19968-20223" {
		t.Fatalf("CJK range = %d glyphs, %q", len(d.glyphs), d.rng)
	}
	for _, r := range [][2]int{{1, 256}, {0, 254}, {65536, 65791}, {-256, -1}} {
		if _, err := s.Range("Vazirmatn Regular", r[0], r[1]); !errors.Is(err, ErrInvalidRange) {
			t.Errorf("range %v: err = %v", r, err)
		}
	}
	if _, err := s.Range("Arial Unicode MS Regular", 0, 255); !errors.Is(err, ErrUnknownFontstack) {
		t.Errorf("unknown fontstack err = %v", err)
	}
}

func TestDeterministic(t *testing.T) {
	a, _ := New()
	b, _ := New()
	ra, _ := a.Range("Vazirmatn Bold", 0xFE00, 0xFEFF)
	rb, _ := b.Range("Vazirmatn Bold", 0xFE00, 0xFEFF)
	if string(ra) != string(rb) {
		t.Fatal("glyph range output is not deterministic")
	}
}

// ascii renders a glyph for eyeballing with `go test -run Visual -v`.
func ascii(g Glyph) string {
	var sb strings.Builder
	w := int(g.Width) + 6
	for j := 0; j < int(g.Height)+6; j++ {
		for i := 0; i < w; i++ {
			v := g.Bitmap[j*w+i]
			switch {
			case v > 191:
				sb.WriteByte('#')
			case v > 150:
				sb.WriteByte('+')
			case v > 60:
				sb.WriteByte('.')
			default:
				sb.WriteByte(' ')
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func TestVisualSample(t *testing.T) {
	if !testing.Verbose() {
		t.Skip("run with -v to print sample glyphs")
	}
	s, _ := New()
	b, _ := s.Range("Vazirmatn Regular", 0xFB00, 0xFBFF)
	d := decode(t, b)
	for _, cp := range []int{0xFB58, 0xFB92} { // initial peh, isolated gaf
		g := d.glyphs[cp]
		t.Logf("U+%04X %+v\n%s", cp, Glyph{Width: g.Width, Height: g.Height, Left: g.Left, Top: g.Top, Advance: g.Advance}, ascii(g))
	}
}
