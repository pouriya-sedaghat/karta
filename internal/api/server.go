// Package api implements Karta's public HTTP API (openapi/openapi.yaml):
// manifest, named-place search, release-pinned style and vector tiles,
// glyphs, health endpoints and the optional MapLibre demo.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pouriya-sedaghat/karta/internal/glyphs"
	"github.com/pouriya-sedaghat/karta/internal/mvt"
	"github.com/pouriya-sedaghat/karta/internal/release"
	"github.com/pouriya-sedaghat/karta/internal/releaseid"
	"github.com/pouriya-sedaghat/karta/internal/search"
	"github.com/pouriya-sedaghat/karta/internal/style"
	"github.com/pouriya-sedaghat/karta/openapi"
)

// Search limits; part of the API contract.
const (
	MaxQueryRunes = 100
	MaxQueryBytes = 400
	DefaultLimit  = 10
	MaxLimit      = 50
)

// Cache policies.
const (
	cacheImmutable = "public, max-age=31536000, immutable"
	cacheDay       = "public, max-age=86400"
	cacheShort     = "public, max-age=300"
	cacheRevalid   = "no-cache"
)

// demoCSP only allows same-origin resources, so the demo cannot reach any
// external host even if a URL were injected into it.
const demoCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; " +
	"font-src 'self'; connect-src 'self'; worker-src 'self' blob:; child-src 'self' blob:; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// Config is the part of the service configuration the handlers need.
type Config struct {
	PublicBaseURL      string
	RequestTimeout     time.Duration
	CORSAllowedOrigins []string
	WebDir             string
}

// Releases resolves releases; implemented by release.Manager.
type Releases interface {
	Active() *release.Release
	Get(id string) (*release.Release, bool)
	Status() release.Status
}

// Server holds the handlers' dependencies.
type Server struct {
	cfg      Config
	releases Releases
	glyphs   *glyphs.Set
	log      *slog.Logger
	web      *os.Root
	fontETag string
}

// New builds the HTTP handler.
func New(cfg Config, releases Releases, g *glyphs.Set, log *slog.Logger) (http.Handler, error) {
	s := &Server{cfg: cfg, releases: releases, glyphs: g, log: log}
	sum := sha256.Sum256([]byte(strings.Join(g.Fontstacks(), ",") + "|glyphs-v1"))
	s.fontETag = hex.EncodeToString(sum[:8])
	if cfg.WebDir != "" {
		root, err := os.OpenRoot(cfg.WebDir)
		if err != nil {
			return nil, fmt.Errorf("open web dir: %w", err)
		}
		s.web = root
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", s.live)
	mux.HandleFunc("GET /health/ready", s.ready)
	mux.HandleFunc("GET /v1/manifest", s.manifest)
	mux.HandleFunc("GET /v1/search", s.search)
	mux.HandleFunc("GET /v1/releases/{release_id}/style.json", s.style)
	mux.HandleFunc("GET /v1/releases/{release_id}/tiles/{z}/{x}/{y}", s.tile)
	mux.HandleFunc("GET /v1/fonts/{fontstack}/{range}", s.font)
	mux.HandleFunc("GET /v1/openapi.yaml", s.openapi)
	if s.web != nil {
		mux.HandleFunc("GET /demo/", s.demo)
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/demo/", http.StatusFound)
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such resource", "")
	})
	return s.middleware(mux), nil
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, cache string, v any, etag bool) {
	b, err := json.Marshal(v)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, CodeInternal, "encoding failed", "")
		return
	}
	writeBytes(w, r, status, "application/json; charset=utf-8", cache, b, etag)
}

// writeBytes sends a body with a strong ETag over its content if requested,
// answering If-None-Match with 304.
func writeBytes(w http.ResponseWriter, r *http.Request, status int, contentType, cache string, b []byte, etag bool) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", cache)
	if etag {
		sum := sha256.Sum256(b)
		tag := `"` + hex.EncodeToString(sum[:12]) + `"`
		h.Set("ETag", tag)
		if match(r, tag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func match(r *http.Request, tag string) bool {
	for _, t := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		t = strings.TrimSpace(t)
		if t == tag || t == "*" || strings.TrimPrefix(t, "W/") == tag {
			return true
		}
	}
	return false
}

// --- health -----------------------------------------------------------------

func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, "no-store", map[string]string{"status": "ok"}, false)
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	st := s.releases.Status()
	body := map[string]any{
		"status": "ready", "reason": st.Reason, "release_id": nullable(st.ReleaseID),
		"checked_at": st.CheckedAt.UTC().Format(time.RFC3339),
	}
	if st.Detail != "" {
		body["detail"] = st.Detail
	}
	code := http.StatusOK
	if !st.Ready {
		body["status"] = "not_ready"
		code = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "5")
	}
	writeJSON(w, r, code, "no-store", body, false)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- manifest ---------------------------------------------------------------

type manifestBody struct {
	APIVersion   string            `json:"api_version"`
	Release      manifestRelease   `json:"release"`
	StyleURL     string            `json:"style_url"`
	Tiles        manifestTiles     `json:"tiles"`
	Search       manifestSearch    `json:"search"`
	DefaultView  manifestView      `json:"default_view"`
	Attribution  manifestAttrib    `json:"attribution"`
	Capabilities map[string]bool   `json:"capabilities"`
	Freshness    manifestFreshness `json:"freshness"`
}

type manifestRelease struct {
	ReleaseID      string         `json:"release_id"`
	Region         manifestRegion `json:"region"`
	DataTimestamp  string         `json:"osm_data_timestamp"`
	TimestampFrom  string         `json:"osm_data_timestamp_source"`
	SourceSHA256   string         `json:"source_sha256"`
	ImportedAt     string         `json:"imported_at"`
	SchemaRevision string         `json:"schema_revision"`
	StyleRevision  string         `json:"style_revision"`
}

type manifestRegion struct {
	ID   string     `json:"id"`
	Name string     `json:"name"`
	BBox [4]float64 `json:"bbox"`
}

type manifestTiles struct {
	URLTemplate string     `json:"url_template"`
	Format      string     `json:"format"`
	MinZoom     int        `json:"minzoom"`
	MaxZoom     int        `json:"maxzoom"`
	Bounds      [4]float64 `json:"bounds"`
	Layers      []string   `json:"layers"`
}

type manifestSearch struct {
	URL           string   `json:"url"`
	Scope         string   `json:"scope"`
	MaxLimit      int      `json:"max_limit"`
	MaxQueryChars int      `json:"max_query_chars"`
	Filters       []string `json:"filters"`
}

type manifestView struct {
	Center [2]float64 `json:"center"`
	Zoom   float64    `json:"zoom"`
}

type manifestAttrib struct {
	Text       string `json:"text"`
	HTML       string `json:"html"`
	License    string `json:"license"`
	LicenseURL string `json:"license_url"`
}

type manifestFreshness struct {
	UpdateMode    string `json:"update_mode"`
	DataTimestamp string `json:"osm_data_timestamp"`
}

func (s *Server) noRelease(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "5")
	st := s.releases.Status()
	writeError(w, r, http.StatusServiceUnavailable, CodeNoActiveRelease, "no release is being served ("+st.Reason+")", "")
}

func (s *Server) manifest(w http.ResponseWriter, r *http.Request) {
	if !s.noParams(w, r) {
		return
	}
	rel := s.releases.Active()
	if rel == nil {
		s.noRelease(w, r)
		return
	}
	i := rel.Info
	layers := make([]string, 0, len(rel.Catalog.Layers))
	for _, l := range rel.Catalog.Layers {
		layers = append(layers, l.ID)
	}
	base := s.cfg.PublicBaseURL
	ts := i.DataTimestamp.UTC().Format(time.RFC3339)
	writeJSON(w, r, http.StatusOK, cacheRevalid, manifestBody{
		APIVersion: "1",
		Release: manifestRelease{
			ReleaseID: i.ReleaseID, Region: manifestRegion{ID: i.RegionID, Name: i.RegionName, BBox: i.BBox},
			DataTimestamp: ts, TimestampFrom: i.DataTimestampSource, SourceSHA256: i.SourceSHA256,
			ImportedAt: i.ImportedAt.UTC().Format(time.RFC3339), SchemaRevision: i.SchemaRevision, StyleRevision: i.StyleRevision,
		},
		StyleURL: base + "/v1/releases/" + i.ReleaseID + "/style.json",
		Tiles: manifestTiles{
			URLTemplate: style.TileURLTemplate(base, i.ReleaseID), Format: "mvt",
			MinZoom: i.MinZoom, MaxZoom: i.MaxZoom, Bounds: i.BBox, Layers: layers,
		},
		Search: manifestSearch{
			URL:           base + "/v1/search",
			Scope:         "named places and points of interest; streets, addresses and house numbers are not searchable",
			MaxLimit:      MaxLimit,
			MaxQueryChars: MaxQueryRunes,
			Filters:       []string{"lang", "bbox", "release_id"},
		},
		DefaultView: manifestView{Center: i.Center, Zoom: i.Zoom},
		Attribution: manifestAttrib{Text: "© OpenStreetMap contributors", HTML: i.Attribution, License: i.License, LicenseURL: i.LicenseURL},
		Capabilities: map[string]bool{
			"vector_tiles": true, "place_search": true,
			"address_geocoding": false, "reverse_geocoding": false, "routing": false,
			"manual_updates": false, "online_updates": false,
		},
		Freshness: manifestFreshness{UpdateMode: "static", DataTimestamp: ts},
	}, true)
}

// noParams rejects query parameters on endpoints that take none.
func (s *Server) noParams(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		writeError(w, r, http.StatusBadRequest, CodeUnknownParameter, "this endpoint takes no query parameters", "")
		return false
	}
	return true
}

// --- search -----------------------------------------------------------------

var searchParams = map[string]bool{"q": true, "limit": true, "lang": true, "bbox": true, "release_id": true}

type searchBody struct {
	ReleaseID       string          `json:"release_id"`
	Query           string          `json:"query"`
	NormalizedQuery string          `json:"normalized_query"`
	Limit           int             `json:"limit"`
	Lang            *string         `json:"lang"`
	BBox            *[4]float64     `json:"bbox"`
	Results         []search.Result `json:"results"`
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	params, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, CodeInvalidParameter, "malformed query string", "")
		return
	}
	for k, v := range params {
		if !searchParams[k] {
			writeError(w, r, http.StatusBadRequest, CodeUnknownParameter, "unknown parameter "+strconv.Quote(k), k)
			return
		}
		if len(v) > 1 {
			writeError(w, r, http.StatusBadRequest, CodeInvalidParameter, "parameter given more than once", k)
			return
		}
	}
	req := search.Request{Limit: DefaultLimit}
	q := strings.TrimSpace(params.Get("q"))
	switch {
	case !params.Has("q") || q == "":
		writeError(w, r, http.StatusBadRequest, CodeInvalidParameter, "q is required", "q")
		return
	case !utf8.ValidString(q):
		writeError(w, r, http.StatusBadRequest, CodeInvalidParameter, "q must be valid UTF-8", "q")
		return
	case len(q) > MaxQueryBytes || utf8.RuneCountInString(q) > MaxQueryRunes:
		writeError(w, r, http.StatusBadRequest, CodeInvalidParameter, fmt.Sprintf("q must be at most %d characters", MaxQueryRunes), "q")
		return
	case strings.IndexFunc(q, unicode.IsControl) >= 0:
		writeError(w, r, http.StatusBadRequest, CodeInvalidParameter, "q must not contain control characters", "q")
		return
	}
	req.Query = q
	if params.Has("limit") {
		n, err := strconv.Atoi(params.Get("limit"))
		if err != nil || n < 1 || n > MaxLimit {
			writeError(w, r, http.StatusBadRequest, CodeInvalidParameter, fmt.Sprintf("limit must be an integer 1..%d", MaxLimit), "limit")
			return
		}
		req.Limit = n
	}
	if params.Has("lang") {
		l := params.Get("lang")
		if !validLang(l) {
			writeError(w, r, http.StatusBadRequest, CodeInvalidParameter, "lang must be a 2- or 3-letter lowercase ISO 639 code", "lang")
			return
		}
		req.Lang = l
	}
	if params.Has("bbox") {
		b, err := parseBBox(params.Get("bbox"))
		if err != nil {
			writeError(w, r, http.StatusBadRequest, CodeInvalidParameter, err.Error(), "bbox")
			return
		}
		req.BBox = &b
	}
	rel, pinned, ok := s.resolve(w, r, params.Get("release_id"), params.Has("release_id"))
	if !ok {
		return
	}
	nq, err := search.Normalize(r.Context(), rel.Pool, req.Query)
	if err != nil {
		s.dbError(w, r, err)
		return
	}
	if nq == "" {
		writeError(w, r, http.StatusBadRequest, CodeInvalidQuery, "q contains no letters or digits", "q")
		return
	}
	results, err := search.Run(r.Context(), rel.Pool, req)
	if err != nil {
		s.dbError(w, r, err)
		return
	}
	body := searchBody{
		ReleaseID: rel.Info.ReleaseID, Query: req.Query, NormalizedQuery: nq, Limit: req.Limit,
		BBox: req.BBox, Results: results,
	}
	if req.Lang != "" {
		body.Lang = &req.Lang
	}
	cache := cacheRevalid
	if pinned {
		cache = cacheShort
	}
	writeJSON(w, r, http.StatusOK, cache, body, false)
}

func validLang(s string) bool {
	if len(s) < 2 || len(s) > 3 {
		return false
	}
	for _, c := range s {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

func parseBBox(s string) ([4]float64, error) {
	var b [4]float64
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return b, errors.New("bbox must be west,south,east,north")
	}
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return b, errors.New("bbox values must be finite numbers")
		}
		b[i] = v
	}
	if !(b[0] >= -180 && b[0] < b[2] && b[2] <= 180 && b[1] >= -85.05112878 && b[1] < b[3] && b[3] <= 85.05112878) {
		return b, errors.New("bbox must satisfy -180 <= west < east <= 180 and -85.0511 <= south < north <= 85.0511")
	}
	return b, nil
}

// resolve returns the requested release (pinned) or the active one.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request, id string, explicit bool) (*release.Release, bool, bool) {
	if explicit {
		if !releaseid.Valid(id) {
			writeError(w, r, http.StatusBadRequest, CodeInvalidParameter, "release_id must match "+releaseid.Pattern.String(), "release_id")
			return nil, false, false
		}
		rel, ok := s.releases.Get(id)
		if !ok {
			writeError(w, r, http.StatusNotFound, CodeUnknownRelease, "release "+id+" is not served", "release_id")
			return nil, false, false
		}
		return rel, true, true
	}
	rel := s.releases.Active()
	if rel == nil {
		s.noRelease(w, r)
		return nil, false, false
	}
	return rel, false, true
}

// dbError maps database failures: timeouts and cancellations are 503
// timeout, other failures 503 unavailable; details are logged, not returned.
func (s *Server) dbError(w http.ResponseWriter, r *http.Request, err error) {
	var pgErr *pgconn.PgError
	timeout := errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &pgErr) && pgErr.Code == "57014")
	s.log.Warn("database error", "request_id", requestID(r.Context()), "path", r.URL.Path, "err", err)
	w.Header().Set("Retry-After", "2")
	if timeout {
		writeError(w, r, http.StatusServiceUnavailable, CodeTimeout, "the request took too long", "")
		return
	}
	writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "the data store is unavailable", "")
}

// --- style and tiles ----------------------------------------------------------

func (s *Server) style(w http.ResponseWriter, r *http.Request) {
	if !s.noParams(w, r) {
		return
	}
	rel, _, ok := s.resolve(w, r, r.PathValue("release_id"), true)
	if !ok {
		return
	}
	i := rel.Info
	b, err := style.Render(rel.Style, style.Params{
		ReleaseID: i.ReleaseID, BaseURL: s.cfg.PublicBaseURL, Bounds: i.BBox, Center: i.Center,
		Zoom: i.Zoom, MinZoom: i.MinZoom, MaxZoom: i.MaxZoom, Attribution: i.Attribution,
	})
	if err != nil {
		s.log.Error("render style", "err", err)
		writeError(w, r, http.StatusInternalServerError, CodeInternal, "style rendering failed", "")
		return
	}
	writeBytes(w, r, http.StatusOK, "application/json; charset=utf-8", cacheDay, b, true)
}

func parseTileCoord(s string, maxDigits int) (int, bool) {
	if s == "" || len(s) > maxDigits {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	v, err := strconv.Atoi(s)
	return v, err == nil
}

func (s *Server) tile(w http.ResponseWriter, r *http.Request) {
	if !s.noParams(w, r) {
		return
	}
	yPart, found := strings.CutSuffix(r.PathValue("y"), ".pbf")
	if !found {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "tiles are served as {z}/{x}/{y}.pbf", "")
		return
	}
	rel, _, ok := s.resolve(w, r, r.PathValue("release_id"), true)
	if !ok {
		return
	}
	z, okz := parseTileCoord(r.PathValue("z"), 2)
	x, okx := parseTileCoord(r.PathValue("x"), 10)
	y, oky := parseTileCoord(yPart, 10)
	if !okz || !okx || !oky {
		writeError(w, r, http.StatusBadRequest, CodeInvalidTile, "z, x and y must be non-negative integers", "")
		return
	}
	if z < rel.Info.MinZoom || z > rel.Info.MaxZoom {
		writeError(w, r, http.StatusNotFound, CodeZoomOutOfRange,
			fmt.Sprintf("tiles exist for zoom %d..%d; clients overzoom beyond %d", rel.Info.MinZoom, rel.Info.MaxZoom, rel.Info.MaxZoom), "")
		return
	}
	if n := 1 << z; x >= n || y >= n {
		writeError(w, r, http.StatusBadRequest, CodeInvalidTile, fmt.Sprintf("x and y must be below %d at zoom %d", 1<<z, z), "")
		return
	}
	h := w.Header()
	etag := fmt.Sprintf(`"%s-%d-%d-%d"`, rel.Info.ReleaseID, z, x, y)
	h.Set("Cache-Control", cacheImmutable)
	h.Set("ETag", etag)
	if match(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	// Tiles away from the region hold no data: answer without the database.
	tb, rb := mvt.TileBounds(z, x, y), rel.Info.BBox
	if tb[2] < rb[0] || tb[0] > rb[2] || tb[3] < rb[1] || tb[1] > rb[3] {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var data []byte
	if err := rel.Pool.QueryRow(r.Context(), `SELECT karta.tile($1, $2, $3)`, z, x, y).Scan(&data); err != nil {
		h.Del("ETag")
		h.Del("Cache-Control")
		s.dbError(w, r, err)
		return
	}
	if len(data) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.Set("Content-Type", "application/vnd.mapbox-vector-tile")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// --- fonts, spec, demo ----------------------------------------------------------

func (s *Server) font(w http.ResponseWriter, r *http.Request) {
	if !s.noParams(w, r) {
		return
	}
	rng, found := strings.CutSuffix(r.PathValue("range"), ".pbf")
	a, b, dash := strings.Cut(rng, "-")
	start, ok1 := parseTileCoord(a, 5)
	end, ok2 := parseTileCoord(b, 5)
	if !found || !dash || !ok1 || !ok2 {
		writeError(w, r, http.StatusBadRequest, CodeInvalidParameter, "glyph range must look like 0-255.pbf", "range")
		return
	}
	data, err := s.glyphs.Range(r.PathValue("fontstack"), start, end)
	switch {
	case errors.Is(err, glyphs.ErrUnknownFontstack):
		writeError(w, r, http.StatusNotFound, CodeUnknownFontstack, "fontstack is not served; available: "+strings.Join(s.glyphs.Fontstacks(), ", "), "fontstack")
		return
	case errors.Is(err, glyphs.ErrInvalidRange):
		writeError(w, r, http.StatusBadRequest, CodeInvalidParameter, err.Error(), "range")
		return
	case err != nil:
		s.log.Error("glyph range", "err", err)
		writeError(w, r, http.StatusInternalServerError, CodeInternal, "glyph generation failed", "")
		return
	}
	tag := fmt.Sprintf(`"%s-%d"`, s.fontETag, start)
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", cacheDay)
	if match(r, tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data) // #nosec G705 -- generated binary glyph PBF, served as application/x-protobuf with nosniff
}

func (s *Server) openapi(w http.ResponseWriter, r *http.Request) {
	writeBytes(w, r, http.StatusOK, "application/yaml; charset=utf-8", cacheDay, openapi.Spec, true)
}

var demoTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8",
	".mjs": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8",
	".json": "application/json; charset=utf-8", ".map": "application/json; charset=utf-8",
	".txt": "text/plain; charset=utf-8", ".svg": "image/svg+xml", ".png": "image/png",
}

// demo serves the static MapLibre demo from KARTA_WEB_DIR through an
// os.Root, so paths cannot escape the directory; directories are not listed.
func (s *Server) demo(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/demo")
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		name = "index.html"
	}
	ctype, ok := demoTypes[path.Ext(name)]
	if !ok {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such file", "")
		return
	}
	f, err := s.web.Open(name)
	if err != nil {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such file", "")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such file", "")
		return
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	if strings.HasPrefix(name, "vendor/") {
		h.Set("Cache-Control", cacheDay)
	} else {
		h.Set("Cache-Control", cacheRevalid)
	}
	h.Set("Content-Security-Policy", demoCSP)
	h.Set("X-Frame-Options", "DENY")
	http.ServeContent(w, r, name, st.ModTime(), f)
}
