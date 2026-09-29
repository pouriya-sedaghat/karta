// Package style owns the MapLibre basemap style: the embedded template, its
// compatibility check against a release's tile layers and available
// fontstacks, and rendering the release-pinned style served to clients.
package style

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/pouriya-sedaghat/karta/internal/schema"
)

//go:embed basemap.json
var template []byte

// SourceID is the vector source every data layer must use.
const SourceID = "karta"

// Template returns a copy of the embedded style template.
func Template() json.RawMessage {
	return append(json.RawMessage(nil), template...)
}

// Revision is a digest of the style template; it is part of every release
// identifier, so a template change creates a new release. It does not cover
// Render, whose output also depends on this code and the public base URL:
// the API content-addresses the style URL by the rendered bytes instead.
func Revision() string {
	sum := sha256.Sum256(template)
	return "st1-" + hex.EncodeToString(sum[:])[:16]
}

type styleDoc struct {
	Version int              `json:"version"`
	Sources map[string]any   `json:"sources"`
	Layers  []map[string]any `json:"layers"`
}

// Validate checks that a style only uses what a release provides: the one
// vector source, tile layers and fields present in the catalog, and
// fontstacks available to the glyph server. It returns every problem found.
func Validate(style json.RawMessage, catalog schema.Catalog, fontstacks []string) error {
	var doc styleDoc
	dec := json.NewDecoder(bytes.NewReader(style))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("style is not valid JSON: %w", err)
	}
	var problems []string
	if doc.Version != 8 {
		problems = append(problems, fmt.Sprintf("style version %d, want 8", doc.Version))
	}
	for id := range doc.Sources {
		if id != SourceID {
			problems = append(problems, fmt.Sprintf("unexpected source %q (only %q is served)", id, SourceID))
		}
	}
	fonts := map[string]bool{}
	for _, f := range fontstacks {
		fonts[f] = true
	}
	seen := map[string]bool{}
	for i, layer := range doc.Layers {
		id, _ := layer["id"].(string)
		if id == "" {
			problems = append(problems, fmt.Sprintf("layer %d has no id", i))
			continue
		}
		if seen[id] {
			problems = append(problems, fmt.Sprintf("duplicate layer id %q", id))
		}
		seen[id] = true
		typ, _ := layer["type"].(string)
		if typ == "background" {
			continue
		}
		if src, _ := layer["source"].(string); src != SourceID {
			problems = append(problems, fmt.Sprintf("layer %q uses source %q, want %q", id, src, SourceID))
			continue
		}
		sl, _ := layer["source-layer"].(string)
		tileLayer, ok := catalog.Layer(sl)
		if !ok {
			problems = append(problems, fmt.Sprintf("layer %q references tile layer %q, which the release does not emit", id, sl))
			continue
		}
		for _, field := range referencedFields(layer) {
			if _, ok := tileLayer.Fields[field]; !ok {
				problems = append(problems, fmt.Sprintf("layer %q reads field %q, which tile layer %q does not have", id, field, sl))
			}
		}
		if layout, ok := layer["layout"].(map[string]any); ok {
			if tf, ok := layout["text-font"]; ok {
				stack, err := fontstack(tf)
				if err != nil {
					problems = append(problems, fmt.Sprintf("layer %q: %v", id, err))
				} else if !fonts[stack] {
					problems = append(problems, fmt.Sprintf("layer %q uses fontstack %q, which is not served", id, stack))
				}
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// Fontstacks lists the fontstacks referenced by a style.
func Fontstacks(style json.RawMessage) ([]string, error) {
	var doc styleDoc
	if err := json.Unmarshal(style, &doc); err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, layer := range doc.Layers {
		layout, _ := layer["layout"].(map[string]any)
		if tf, ok := layout["text-font"]; ok {
			s, err := fontstack(tf)
			if err != nil {
				return nil, err
			}
			set[s] = true
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// fontstack accepts only a literal array of font names, the form MapLibre
// requests glyphs for; expressions would make the set of stacks open-ended.
func fontstack(v any) (string, error) {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return "", errors.New("text-font must be a non-empty literal array of font names")
	}
	names := make([]string, len(arr))
	for i, n := range arr {
		s, ok := n.(string)
		if !ok || s == "" {
			return "", errors.New("text-font must be a non-empty literal array of font names")
		}
		names[i] = s
	}
	return strings.Join(names, ","), nil
}

// referencedFields returns feature properties read by ["get", ...] and
// ["has", ...] expressions anywhere in the layer's filter, layout and paint.
func referencedFields(layer map[string]any) []string {
	set := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case []any:
			if len(t) == 2 {
				if op, ok := t[0].(string); ok && (op == "get" || op == "has") {
					if f, ok := t[1].(string); ok {
						set[f] = true
					}
				}
			}
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	for _, k := range []string{"filter", "layout", "paint"} {
		walk(layer[k])
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// Params are the per-release values written into a served style.
type Params struct {
	ReleaseID   string
	BaseURL     string // absolute, without trailing slash
	Bounds      [4]float64
	Center      [2]float64
	Zoom        float64
	MinZoom     int
	MaxZoom     int
	Attribution string
}

// TileURLTemplate is the release-pinned tile URL template.
func TileURLTemplate(baseURL, releaseID string) string {
	return baseURL + "/v1/releases/" + releaseID + "/tiles/{z}/{x}/{y}.pbf"
}

// GlyphURLTemplate is the glyph URL template shared by all releases.
func GlyphURLTemplate(baseURL string) string {
	return baseURL + "/v1/fonts/{fontstack}/{range}.pbf"
}

// Render produces the style served for one release. Keys are emitted in a
// stable order, so the same release and base URL always yield the same bytes.
func Render(stored json.RawMessage, p Params) ([]byte, error) {
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(stored))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("stored style: %w", err)
	}
	doc["glyphs"] = GlyphURLTemplate(p.BaseURL)
	doc["center"] = []float64{p.Center[0], p.Center[1]}
	doc["zoom"] = p.Zoom
	meta, _ := doc["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["karta:release_id"] = p.ReleaseID
	doc["metadata"] = meta
	doc["sources"] = map[string]any{
		SourceID: map[string]any{
			"type":        "vector",
			"tiles":       []string{TileURLTemplate(p.BaseURL, p.ReleaseID)},
			"minzoom":     p.MinZoom,
			"maxzoom":     p.MaxZoom,
			"bounds":      []float64{p.Bounds[0], p.Bounds[1], p.Bounds[2], p.Bounds[3]},
			"attribution": p.Attribution,
		},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
