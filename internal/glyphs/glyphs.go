// Package glyphs serves MapLibre signed-distance-field glyph ranges generated
// from the bundled Vazirmatn fonts (SIL OFL 1.1, see fonts/OFL.txt).
//
// MapLibre shapes Arabic-script text itself and requests glyphs by the
// resulting code points, which for Persian are Arabic Presentation Forms
// (U+FB50-U+FDFF, U+FE70-U+FEFF); Vazirmatn maps all Persian letters there.
// Ranges are generated on first request and cached for the process lifetime,
// so memory use is bounded by the fixed set of 256 ranges per fontstack.
package glyphs

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"google.golang.org/protobuf/encoding/protowire"
)

// Parameters of the MapLibre glyph format: glyphs are rasterised at 24 px per
// em with a 3 px border; the signed distance is scaled by an 8 px radius and
// the outline sits at a quarter of that range (value 191).
const (
	emSize = 24
	border = 3
	radius = 8.0
	cutoff = 0.25
)

//go:embed fonts/Vazirmatn-Regular.ttf fonts/Vazirmatn-Bold.ttf
var fontFiles embed.FS

var fontstackFiles = map[string]string{
	"Vazirmatn Regular": "fonts/Vazirmatn-Regular.ttf",
	"Vazirmatn Bold":    "fonts/Vazirmatn-Bold.ttf",
}

// ErrUnknownFontstack is returned for fontstacks that are not served.
var ErrUnknownFontstack = errors.New("unknown fontstack")

// ErrInvalidRange is returned for a range that is not a 256-code-point block.
var ErrInvalidRange = errors.New("glyph range must be N-(N+255) with N a multiple of 256 below 65536")

// Set generates and caches glyph ranges for the bundled fontstacks.
type Set struct {
	fonts map[string]*sfnt.Font
	mu    sync.Mutex
	cache map[string]*entry
}

type entry struct {
	once sync.Once
	rng  Range
	err  error
}

// Range is one generated glyph range and its HTTP validator.
type Range struct {
	Data []byte
	// ETag is a strong validator derived from Data, so any change to a
	// font or to the generator changes it.
	ETag string
}

// New parses the bundled fonts.
func New() (*Set, error) {
	fonts := map[string][]byte{}
	for name, file := range fontstackFiles {
		b, err := fontFiles.ReadFile(file)
		if err != nil {
			return nil, err
		}
		fonts[name] = b
	}
	return newSet(fonts)
}

// newSet builds a set from fontstack name -> TrueType bytes.
func newSet(fonts map[string][]byte) (*Set, error) {
	s := &Set{fonts: map[string]*sfnt.Font{}, cache: map[string]*entry{}}
	for name, b := range fonts {
		f, err := sfnt.Parse(b)
		if err != nil {
			return nil, fmt.Errorf("parse font for %q: %w", name, err)
		}
		s.fonts[name] = f
	}
	return s, nil
}

// Fontstacks lists the served fontstack names.
func (s *Set) Fontstacks() []string {
	out := make([]string, 0, len(s.fonts))
	for n := range s.fonts {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Range returns the protobuf glyph range starting at start (a multiple of
// 256) for a fontstack. Ranges without any glyph in the font are valid,
// empty ranges, so clients never see an error for uncovered blocks.
func (s *Set) Range(fontstack string, start, end int) (Range, error) {
	f, ok := s.fonts[fontstack]
	if !ok {
		return Range{}, ErrUnknownFontstack
	}
	if start < 0 || start%256 != 0 || end != start+255 || end > 65535 {
		return Range{}, ErrInvalidRange
	}
	key := fmt.Sprintf("%s/%d", fontstack, start)
	s.mu.Lock()
	e, ok := s.cache[key]
	if !ok {
		e = &entry{}
		s.cache[key] = e
	}
	s.mu.Unlock()
	e.once.Do(func() {
		var data []byte
		data, e.err = buildRange(f, fontstack, start)
		sum := sha256.Sum256(data)
		e.rng = Range{Data: data, ETag: `"` + hex.EncodeToString(sum[:16]) + `"`}
	})
	return e.rng, e.err
}

// Glyph is one rendered glyph and its metrics in pixels at 24 px/em.
type Glyph struct {
	ID      uint32
	Bitmap  []byte // (Width+6) x (Height+6) signed-distance values, or nil
	Width   uint32
	Height  uint32
	Left    int32
	Top     int32
	Advance uint32
}

func buildRange(f *sfnt.Font, name string, start int) ([]byte, error) {
	var buf sfnt.Buffer
	metrics, err := f.Metrics(&buf, fixed.I(emSize), font.HintingNone)
	if err != nil {
		return nil, err
	}
	ascent := math.Round(fix(metrics.Ascent))
	first := rune(start) // #nosec G115 -- Range checks 0 <= start <= 65280
	var glyphs []Glyph
	for cp := first; cp <= first+255; cp++ {
		idx, err := f.GlyphIndex(&buf, cp)
		if err != nil || idx == 0 {
			continue
		}
		g, err := render(f, &buf, idx, cp, ascent)
		if err != nil {
			return nil, fmt.Errorf("glyph U+%04X: %w", cp, err)
		}
		glyphs = append(glyphs, g)
	}
	return encodeRange(name, start, glyphs), nil
}

type point struct{ x, y float64 }
type segment struct{ a, b point }

func fix(v fixed.Int26_6) float64 { return float64(v) / 64 }

// render rasterises one glyph as a signed distance field. Distances are
// measured to the flattened outline; inside is decided by the non-zero
// winding rule, as TrueType fills glyphs.
func render(f *sfnt.Font, buf *sfnt.Buffer, idx sfnt.GlyphIndex, id rune, ascent float64) (Glyph, error) {
	adv, err := f.GlyphAdvance(buf, idx, fixed.I(emSize), font.HintingNone)
	if err != nil {
		return Glyph{}, err
	}
	g := Glyph{ID: uint32(id), Advance: uint32(math.Max(0, math.Round(fix(adv))))} // #nosec G115 -- id is a BMP code point
	segs, err := f.LoadGlyph(buf, idx, fixed.I(emSize), nil)
	if err != nil {
		return Glyph{}, err
	}
	lines := flatten(segs)
	if len(lines) == 0 {
		return g, nil // whitespace and zero-width characters
	}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, l := range lines {
		for _, p := range []point{l.a, l.b} {
			minX, maxX = math.Min(minX, p.x), math.Max(maxX, p.x)
			minY, maxY = math.Min(minY, p.y), math.Max(maxY, p.y)
		}
	}
	left, right := math.Floor(minX), math.Ceil(maxX)
	top, bottom := math.Floor(minY), math.Ceil(maxY) // y grows downwards
	g.Width, g.Height = uint32(right-left), uint32(bottom-top)
	g.Left = int32(left)
	// Glyph tops are relative to a line origin one ascent above the baseline.
	g.Top = int32(-top - ascent)
	w, h := int(right-left)+2*border, int(bottom-top)+2*border
	g.Bitmap = make([]byte, w*h)
	for j := 0; j < h; j++ {
		py := top - border + float64(j) + 0.5
		for i := 0; i < w; i++ {
			px := left - border + float64(i) + 0.5
			d := math.Sqrt(minDist2(lines, px, py))
			if winding(lines, px, py) != 0 {
				d = -d
			}
			v := 255 - 255*(d/radius+cutoff)
			g.Bitmap[j*w+i] = byte(math.Max(0, math.Min(255, math.Round(v))))
		}
	}
	return g, nil
}

func flatten(segs sfnt.Segments) []segment {
	var out []segment
	var cur, start point
	add := func(p point) {
		if p != cur {
			out = append(out, segment{cur, p})
		}
		cur = p
	}
	closePath := func() {
		if cur != start {
			out = append(out, segment{cur, start})
		}
		cur = start
	}
	for i, s := range segs {
		a := s.Args
		switch s.Op {
		case sfnt.SegmentOpMoveTo:
			if i > 0 {
				closePath()
			}
			cur = point{fix(a[0].X), fix(a[0].Y)}
			start = cur
		case sfnt.SegmentOpLineTo:
			add(point{fix(a[0].X), fix(a[0].Y)})
		case sfnt.SegmentOpQuadTo:
			p0, p1, p2 := cur, point{fix(a[0].X), fix(a[0].Y)}, point{fix(a[1].X), fix(a[1].Y)}
			for k := 1; k <= 8; k++ {
				t := float64(k) / 8
				u := 1 - t
				add(point{u*u*p0.x + 2*u*t*p1.x + t*t*p2.x, u*u*p0.y + 2*u*t*p1.y + t*t*p2.y})
			}
		case sfnt.SegmentOpCubeTo:
			p0 := cur
			p1 := point{fix(a[0].X), fix(a[0].Y)}
			p2 := point{fix(a[1].X), fix(a[1].Y)}
			p3 := point{fix(a[2].X), fix(a[2].Y)}
			for k := 1; k <= 12; k++ {
				t := float64(k) / 12
				u := 1 - t
				add(point{
					u*u*u*p0.x + 3*u*u*t*p1.x + 3*u*t*t*p2.x + t*t*t*p3.x,
					u*u*u*p0.y + 3*u*u*t*p1.y + 3*u*t*t*p2.y + t*t*t*p3.y,
				})
			}
		}
	}
	if len(segs) > 0 {
		closePath()
	}
	return out
}

func minDist2(lines []segment, x, y float64) float64 {
	best := math.Inf(1)
	for _, l := range lines {
		dx, dy := l.b.x-l.a.x, l.b.y-l.a.y
		t := 0.0
		if n := dx*dx + dy*dy; n > 0 {
			t = math.Max(0, math.Min(1, ((x-l.a.x)*dx+(y-l.a.y)*dy)/n))
		}
		ex, ey := l.a.x+t*dx-x, l.a.y+t*dy-y
		if d := ex*ex + ey*ey; d < best {
			best = d
		}
	}
	return best
}

func winding(lines []segment, x, y float64) int {
	w := 0
	for _, l := range lines {
		if l.a.y <= y {
			if l.b.y > y && cross(l, x, y) > 0 {
				w++
			}
		} else if l.b.y <= y && cross(l, x, y) < 0 {
			w--
		}
	}
	return w
}

func cross(l segment, x, y float64) float64 {
	return (l.b.x-l.a.x)*(y-l.a.y) - (x-l.a.x)*(l.b.y-l.a.y)
}

// encodeRange writes the glyphs protobuf message:
//
//	glyphs    { repeated fontstack stacks = 1; }
//	fontstack { string name = 1; string range = 2; repeated glyph glyphs = 3; }
//	glyph     { uint32 id = 1; bytes bitmap = 2; uint32 width = 3; uint32 height = 4;
//	            sint32 left = 5; sint32 top = 6; uint32 advance = 7; }
func encodeRange(name string, start int, glyphs []Glyph) []byte {
	var stack []byte
	stack = protowire.AppendTag(stack, 1, protowire.BytesType)
	stack = protowire.AppendString(stack, name)
	stack = protowire.AppendTag(stack, 2, protowire.BytesType)
	stack = protowire.AppendString(stack, fmt.Sprintf("%d-%d", start, start+255))
	for _, g := range glyphs {
		var m []byte
		m = appendUint(m, 1, uint64(g.ID))
		if g.Bitmap != nil {
			m = protowire.AppendTag(m, 2, protowire.BytesType)
			m = protowire.AppendBytes(m, g.Bitmap)
		}
		m = appendUint(m, 3, uint64(g.Width))
		m = appendUint(m, 4, uint64(g.Height))
		m = appendUint(m, 5, protowire.EncodeZigZag(int64(g.Left)))
		m = appendUint(m, 6, protowire.EncodeZigZag(int64(g.Top)))
		m = appendUint(m, 7, uint64(g.Advance))
		stack = protowire.AppendTag(stack, 3, protowire.BytesType)
		stack = protowire.AppendBytes(stack, m)
	}
	out := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(out, stack)
}

func appendUint(b []byte, num protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, num, protowire.VarintType), v)
}
