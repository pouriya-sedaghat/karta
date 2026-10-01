// Command sourceserver is the controlled HTTPS source of the Stage 3
// acceptance tests: a test-only stand-in for a snapshot provider. It serves
// the files the tests write into its document root over TLS (a test CA)
// and misbehaves on request: redirects, error statuses, throttled, stalled,
// cut or corrupted transfers, extra bytes, compressed or unknown-length
// bodies, slow headers, ignored ranges. A plain-HTTP control listener (only
// reachable from the test host) sets the behaviour per path and returns the
// request log. It is never part of a deployment.
//
//	SOURCE_DOCROOT    document root (read-only)
//	SOURCE_TLS_CERT   server certificate chain (PEM)
//	SOURCE_TLS_KEY    server key (PEM)
//	SOURCE_ADDR       TLS listener (default :8443)
//	SOURCE_CTL_ADDR   control listener (default :8444)
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rule is the behaviour for one path.
type Rule struct {
	// Status, if set, is answered instead of the file.
	Status int `json:"status,omitempty"`
	// Location is sent with a 3xx Status.
	Location string `json:"location,omitempty"`
	// DelayHeaders waits before answering.
	DelayHeaders string `json:"delay_headers,omitempty"`
	// ThrottleBPS sends at most this many bytes per second.
	ThrottleBPS int `json:"throttle_bps,omitempty"`
	// StallAfter stops sending after this many body bytes and holds the
	// connection open.
	StallAfter int64 `json:"stall_after,omitempty"`
	// CloseAfter drops the connection after this many body bytes, for the
	// first CloseTimes requests (all requests if 0).
	CloseAfter int64 `json:"close_after,omitempty"`
	CloseTimes int   `json:"close_times,omitempty"`
	// Corrupt flips one byte in the middle of the body.
	Corrupt bool `json:"corrupt,omitempty"`
	// ExtraBytes appends bytes to the body.
	ExtraBytes int `json:"extra_bytes,omitempty"`
	// NoContentLength sends the body chunked.
	NoContentLength bool `json:"no_content_length,omitempty"`
	// ContentEncoding is sent as the Content-Encoding header.
	ContentEncoding string `json:"content_encoding,omitempty"`
	// IgnoreRange answers range requests with the whole body.
	IgnoreRange bool `json:"ignore_range,omitempty"`
}

// Request is one logged request.
type Request struct {
	At        time.Time `json:"at"`
	Method    string    `json:"method"`
	Host      string    `json:"host"`
	Path      string    `json:"path"`
	Query     string    `json:"query"`
	Range     string    `json:"range"`
	IfRange   string    `json:"if_range"`
	Auth      string    `json:"authorization"`
	UserAgent string    `json:"user_agent"`
	Status    int       `json:"status"`
	Bytes     int64     `json:"bytes"`
}

type server struct {
	root  *os.Root
	mu    sync.Mutex
	rules map[string]*Rule
	used  map[string]int
	log   []Request
}

var rangePattern = regexp.MustCompile(`^bytes=([0-9]+)-$`)

func (s *server) serve(w http.ResponseWriter, r *http.Request) {
	rec := Request{At: time.Now().UTC(), Method: r.Method, Host: r.Host, Path: r.URL.Path, Query: r.URL.RawQuery,
		Range: r.Header.Get("Range"), IfRange: r.Header.Get("If-Range"), Auth: r.Header.Get("Authorization"), UserAgent: r.Header.Get("User-Agent")}
	defer func() {
		s.mu.Lock()
		s.log = append(s.log, rec)
		if len(s.log) > 5000 {
			s.log = s.log[1000:]
		}
		s.mu.Unlock()
	}()
	s.mu.Lock()
	rule := Rule{}
	if p, ok := s.rules[r.URL.Path]; ok {
		rule = *p
	}
	s.used[r.URL.Path]++
	use := s.used[r.URL.Path]
	s.mu.Unlock()
	if d, err := time.ParseDuration(rule.DelayHeaders); err == nil && d > 0 {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
	}
	if rule.Status != 0 {
		if rule.Location != "" {
			w.Header().Set("Location", rule.Location)
		}
		rec.Status = rule.Status
		w.WriteHeader(rule.Status)
		return
	}
	// os.Root confines every lookup to the document root (no "..", no
	// symlink out of it).
	body, err := s.root.ReadFile(strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/"))
	if err != nil {
		rec.Status = http.StatusNotFound
		http.NotFound(w, r)
		return
	}
	if rule.Corrupt && len(body) > 0 {
		body = append([]byte(nil), body...)
		body[len(body)/2] ^= 0xff
	}
	body = append(body, bytes.Repeat([]byte{'x'}, rule.ExtraBytes)...)
	etag := fmt.Sprintf(`"%x-%d"`, len(body), bytesSum(body))
	w.Header().Set("ETag", etag)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if rule.ContentEncoding != "" {
		w.Header().Set("Content-Encoding", rule.ContentEncoding)
	}
	start := int64(0)
	status := http.StatusOK
	if m := rangePattern.FindStringSubmatch(r.Header.Get("Range")); m != nil && !rule.IgnoreRange &&
		(r.Header.Get("If-Range") == "" || r.Header.Get("If-Range") == etag) {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		if n >= int64(len(body)) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(body)))
			rec.Status = http.StatusRequestedRangeNotSatisfiable
			w.WriteHeader(rec.Status)
			return
		}
		start, status = n, http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", n, len(body)-1, len(body)))
	}
	out := body[start:]
	if !rule.NoContentLength {
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	}
	rec.Status = status
	w.WriteHeader(status)
	limit := int64(len(out))
	closeConn := false
	if rule.CloseAfter > 0 && (rule.CloseTimes == 0 || use <= rule.CloseTimes) && rule.CloseAfter < limit {
		limit, closeConn = rule.CloseAfter, true
	}
	stall := rule.StallAfter > 0 && rule.StallAfter < limit
	if stall {
		limit = rule.StallAfter
	}
	rec.Bytes = write(w, r, out[:limit], rule.ThrottleBPS)
	if stall {
		<-r.Context().Done()
		return
	}
	if closeConn {
		panic(http.ErrAbortHandler)
	}
}

// write sends b, at most bps bytes per second if bps > 0.
func write(w http.ResponseWriter, r *http.Request, b []byte, bps int) int64 {
	f, _ := w.(http.Flusher)
	chunk := len(b)
	tick := time.Duration(0)
	if bps > 0 {
		chunk = max(1, bps/10)
		tick = 100 * time.Millisecond
	}
	var n int64
	for len(b) > 0 {
		c := min(chunk, len(b))
		k, err := w.Write(b[:c]) // #nosec G705 -- test fixture bytes served as application/octet-stream with nosniff
		n += int64(k)
		if err != nil {
			return n
		}
		if f != nil {
			f.Flush()
		}
		b = b[c:]
		if tick > 0 && len(b) > 0 {
			select {
			case <-time.After(tick):
			case <-r.Context().Done():
				return n
			}
		}
	}
	return n
}

func bytesSum(b []byte) uint32 {
	var h uint32 = 2166136261
	for _, c := range b {
		h = (h ^ uint32(c)) * 16777619
	}
	return h
}

func (s *server) control() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("PUT /rules", func(w http.ResponseWriter, r *http.Request) {
		var rules map[string]*Rule
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rules); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.rules, s.used = rules, map[string]int{}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /requests", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		b, _ := json.Marshal(s.log)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("DELETE /requests", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.log = nil
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	root, err := os.OpenRoot(env("SOURCE_DOCROOT", "/docroot"))
	if err != nil {
		log.Fatal(err)
	}
	s := &server{root: root, rules: map[string]*Rule{}, used: map[string]int{}}
	cert, err := tls.LoadX509KeyPair(env("SOURCE_TLS_CERT", "/tls/server.pem"), env("SOURCE_TLS_KEY", "/tls/server-key.pem"))
	if err != nil {
		log.Fatal(err)
	}
	tlsSrv := &http.Server{Addr: env("SOURCE_ADDR", ":8443"), Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}}
	ctl := &http.Server{Addr: env("SOURCE_CTL_ADDR", ":8444"), Handler: s.control(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- tlsSrv.ListenAndServeTLS("", "") }()
	go func() { errc <- ctl.ListenAndServe() }()
	log.Printf("sourceserver: TLS %s, control %s, root %s", tlsSrv.Addr, ctl.Addr, s.root.Name())
	err = <-errc
	if !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(strings.TrimSpace(err.Error()))
	}
}
