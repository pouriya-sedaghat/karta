package operator

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"
)

// CredentialFile is a credentials file that is read again when it changes
// (size, modification time or inode), so a credential can be rotated
// without restarting the process that checks it. A changed file that does
// not parse (for example while it is being rewritten) keeps the previous
// credentials.
type CredentialFile struct {
	path string
	log  *slog.Logger

	mu    sync.Mutex
	key   fileKey
	creds []Credential
}

type fileKey struct {
	size  int64
	mtime time.Time
	ino   uint64
}

func statKey(path string) (fileKey, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileKey{}, err
	}
	k := fileKey{size: fi.Size(), mtime: fi.ModTime()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		k.ino = st.Ino
	}
	return k, nil
}

// OpenCredentialFile loads a credentials file (LoadCredentials format).
func OpenCredentialFile(path string, log *slog.Logger) (*CredentialFile, error) {
	k, err := statKey(path)
	if err != nil {
		return nil, err
	}
	creds, err := LoadCredentials(path)
	if err != nil {
		return nil, err
	}
	return &CredentialFile{path: path, log: log, key: k, creds: creds}, nil
}

// Current returns the credentials, reading the file again if it changed.
func (f *CredentialFile) Current() []Credential {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, err := statKey(f.path)
	if err != nil || k == f.key {
		return f.creds
	}
	creds, err := LoadCredentials(f.path)
	if err != nil {
		f.log.Warn("credentials file changed but does not parse; keeping the previous credentials", "file", f.path, "err", err)
		return f.creds
	}
	f.key, f.creds = k, creds
	f.log.Info("credentials reloaded", "file", f.path, "credentials", len(creds))
	return f.creds
}

// RequireScope serves h for GET and HEAD of path to requests whose bearer
// token belongs to a credential with scope. Anything else is refused:
// 404 for other paths, 405 for other methods, 401 without a valid token,
// 403 without the scope. Refusals are logged at most once a minute; tokens
// are never logged.
func RequireScope(f *CredentialFile, scope, path string, h http.Handler, log *slog.Logger) http.Handler {
	var mu sync.Mutex
	var lastLog time.Time
	refuse := func(w http.ResponseWriter, r *http.Request, status int, why string) {
		mu.Lock()
		if time.Since(lastLog) > time.Minute {
			lastLog = time.Now()
			log.Warn("metrics request refused", "status", status, "why", why, "remote", r.RemoteAddr)
		}
		mu.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, http.StatusText(status), status)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			refuse(w, r, http.StatusNotFound, "unknown path")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			refuse(w, r, http.StatusMethodNotAllowed, "method")
			return
		}
		c, err := Authenticate(f.Current(), r.Header.Get("Authorization"))
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="karta-metrics"`)
			why := "invalid token"
			if errors.Is(err, ErrNoCredentials) {
				why = "no token"
			}
			refuse(w, r, http.StatusUnauthorized, why)
			return
		}
		if !c.Scopes[scope] {
			refuse(w, r, http.StatusForbidden, "credential "+c.Name+" lacks scope "+scope)
			return
		}
		h.ServeHTTP(w, r)
	})
}
