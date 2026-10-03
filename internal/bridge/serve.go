package bridge

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/online"
	"github.com/pouriya-sedaghat/karta/internal/safefile"
)

var assetPath = regexp.MustCompile(`^/` + SnapshotsDir + `/([0-9a-f]{64})\.osm\.pbf(\.provenance\.json)?$`)

// Bounds of the serving process: at most MaxConcurrent requests at a time
// (others get 503 with Retry-After), and a response whose client reads
// nothing for writeStall is abandoned (the fetcher resumes with Range).
var (
	MaxConcurrent = 16
	writeStall    = time.Minute
)

// limitConcurrency answers 503 beyond n requests in progress.
func limitConcurrency(n int, h http.Handler) http.Handler {
	sem := make(chan struct{}, n)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			h.ServeHTTP(w, r)
		default:
			w.Header().Set("Retry-After", "5")
			http.Error(w, "busy", http.StatusServiceUnavailable)
		}
	})
}

// stallWriter extends the connection's write deadline before every write,
// so a slow but moving download continues and a stalled one ends.
type stallWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func (s stallWriter) Write(b []byte) (int, error) {
	_ = s.rc.SetWriteDeadline(time.Now().Add(writeStall))
	return s.ResponseWriter.Write(b)
}

// Handler serves the publish directory read-only: GET and HEAD of
// /manifest.json and of content-addressed /snapshots/<sha256>.osm.pbf files
// and their sidecars, nothing else. Assets carry a strong ETag (their
// digest) and answer Range requests, so the fetcher's verified resume works.
func Handler(publish string, log *slog.Logger) http.Handler {
	return limitConcurrency(MaxConcurrent, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w = stallWriter{ResponseWriter: w, rc: http.NewResponseController(w)}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.RawQuery != "" {
			http.Error(w, "no query", http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/"+ManifestFile {
			b, err := safefile.ReadRegular(filepath.Join(publish, ManifestFile), online.MaxEnvelopeBytes)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			h.Set("Content-Type", "application/json")
			h.Set("Cache-Control", "no-cache")
			h.Set("ETag", `"sha256:`+safefile.SHA256Hex(b)+`"`)
			http.ServeContent(w, r, ManifestFile, time.Time{}, bytes.NewReader(b))
			return
		}
		m := assetPath.FindStringSubmatch(r.URL.Path)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		name := m[1] + ".osm.pbf" + m[2]
		f, err := safefile.OpenNoFollow(filepath.Join(publish, SnapshotsDir, name))
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				log.Warn("asset unreadable", "name", name, "err", err)
			}
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		if m[2] != "" {
			h.Set("Content-Type", "application/json")
		} else {
			h.Set("Content-Type", "application/octet-stream")
		}
		// An asset never changes under its name.
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		etag := m[1]
		if m[2] != "" {
			etag = "sidecar-" + m[1]
		}
		h.Set("ETag", `"sha256:`+etag+`"`)
		http.ServeContent(w, r, name, fi.ModTime(), f)
	}))
}

// Statuses reads acquire's and sign's reports for the serve process's
// metrics (both mounted read-only).
func Statuses(spool, publish string) (*AcquireState, *SignerState, error) {
	var errs []error
	a, err := ReadAcquireState(spool)
	if err != nil {
		errs = append(errs, err)
	}
	s, err := ReadSignerStatus(publish)
	if err != nil {
		errs = append(errs, err)
	}
	if _, err := os.Stat(spool); err != nil {
		errs = append(errs, err)
	}
	return a, s, errors.Join(errs...)
}
