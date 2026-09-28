package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = 1

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// statusRecorder captures the status and size for access logs.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// middleware wraps every request: request id, access log (path only; query
// strings may contain search terms and are not logged), panic recovery,
// security headers, CORS, method and body restrictions and a deadline.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-ID")
		if !requestIDPattern.MatchString(id) {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), requestIDKey, id), s.cfg.RequestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		rec := &statusRecorder{ResponseWriter: w}
		h := rec.Header()
		h.Set("X-Request-ID", id)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")

		defer func() {
			if p := recover(); p != nil {
				s.log.Error("panic", "request_id", id, "panic", p, "stack", string(debug.Stack()))
				if rec.status == 0 {
					writeError(rec, r, http.StatusInternalServerError, CodeInternal, "internal error", "")
				}
			}
			s.log.Info("request", "request_id", id, "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "bytes", rec.bytes, "ms", time.Since(start).Milliseconds())
		}()

		s.cors(rec, r)
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		case http.MethodOptions:
			h.Set("Allow", "GET, HEAD, OPTIONS")
			rec.WriteHeader(http.StatusNoContent)
			return
		default:
			h.Set("Allow", "GET, HEAD, OPTIONS")
			writeError(rec, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "only GET, HEAD and OPTIONS are supported", "")
			return
		}
		if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
			writeError(rec, r, http.StatusRequestEntityTooLarge, CodeBodyNotAllowed, "requests must not have a body", "")
			return
		}
		r.Body = http.MaxBytesReader(rec, r.Body, 0)
		next.ServeHTTP(rec, r)
	})
}

// cors implements the CORS policy: read-only GET/HEAD from the configured
// origins (or any origin with "*"), no credentials. Preflight requests are
// answered by the OPTIONS branch of the middleware.
func (s *Server) cors(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	h := w.Header()
	h.Add("Vary", "Origin")
	if origin == "" || len(s.cfg.CORSAllowedOrigins) == 0 {
		return
	}
	allowed := ""
	for _, o := range s.cfg.CORSAllowedOrigins {
		if o == "*" {
			allowed = "*"
			break
		}
		if strings.EqualFold(o, origin) {
			allowed = origin
			break
		}
	}
	if allowed == "" {
		return
	}
	h.Set("Access-Control-Allow-Origin", allowed)
	h.Set("Access-Control-Expose-Headers", "ETag, X-Request-ID")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "GET, HEAD")
		if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
			h.Set("Access-Control-Allow-Headers", "If-None-Match, X-Request-ID")
		}
		h.Set("Access-Control-Max-Age", "600")
	}
}
