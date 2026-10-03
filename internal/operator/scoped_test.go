package operator

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRequireScopeAndReload(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	monitor, admin, next := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	path := filepath.Join(t.TempDir(), "metrics_tokens")
	write := func(content string, mtime time.Time) {
		t.Helper()
		// In place, as scripts/gen-secrets.sh rewrites it (a bind-mounted
		// file keeps its inode).
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Now().Add(-time.Hour)
	write("monitor status "+HashToken(monitor)+"\nadmin publish "+HashToken(admin)+"\n", t0)
	f, err := OpenCredentialFile(path, log)
	if err != nil {
		t.Fatal(err)
	}
	h := RequireScope(f, ScopeStatus, "/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}), log)
	get := func(method, target, token string) int {
		req := httptest.NewRequest(method, target, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, c := range []struct {
		method, target, token string
		want                  int
	}{
		{"GET", "/metrics", monitor, 200},
		{"HEAD", "/metrics", monitor, 200},
		{"GET", "/metrics", "", 401},
		{"GET", "/metrics", strings.Repeat("d", 64), 401},
		{"GET", "/metrics", "not-a-token", 401},
		{"GET", "/metrics", admin, 403}, // no status scope
		{"POST", "/metrics", monitor, 405},
		{"GET", "/v1/operator/status", monitor, 404},
	} {
		if got := get(c.method, c.target, c.token); got != c.want {
			t.Errorf("%s %s token %.4s: %d, want %d", c.method, c.target, c.token, got, c.want)
		}
	}

	// Rotation: the new token works and the old one stops, without a restart.
	write("monitor status "+HashToken(next)+"\n", t0.Add(time.Minute))
	if get("GET", "/metrics", next) != 200 || get("GET", "/metrics", monitor) != 401 {
		t.Fatal("the rewritten credentials file was not picked up")
	}
	// A file caught half-written does not lock everyone out.
	write("monitor status 12", t0.Add(2*time.Minute))
	if get("GET", "/metrics", next) != 200 {
		t.Fatal("a broken rewrite dropped the previous credentials")
	}
}
