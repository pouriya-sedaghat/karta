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

// Handler serves the publish directory read-only: GET and HEAD of
// /manifest.json and of content-addressed /snapshots/<sha256>.osm.pbf files
// and their sidecars, nothing else. Assets carry a strong ETag (their
// digest) and answer Range requests, so the fetcher's verified resume works.
func Handler(publish string, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})
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
