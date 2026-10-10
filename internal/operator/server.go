package operator

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pouriya-sedaghat/karta/internal/publish"
	"github.com/pouriya-sedaghat/karta/internal/registry"
	"github.com/pouriya-sedaghat/karta/internal/releaseid"
	"github.com/pouriya-sedaghat/karta/openapi"
)

// Service is what the operator API drives; implemented by publish.Service.
type Service interface {
	Status(ctx context.Context) (publish.Status, error)
	Audit(ctx context.Context, limit int, before int64) ([]registry.AuditEntry, error)
	Authorize(ctx context.Context, p publish.Principal, digest string, size *int64, expires *time.Time, reason string) (*registry.Authorization, bool, error)
	Revoke(ctx context.Context, p publish.Principal, digest, reason string) (bool, error)
	Activate(ctx context.Context, p publish.Principal, a publish.ActionRequest) (registry.ActivateResult, error)
	Revalidate(ctx context.Context, p publish.Principal, releaseID, reason string) (publish.Revalidation, error)
	Rollback(ctx context.Context, p publish.Principal, a publish.ActionRequest) (registry.ActivateResult, error)
	Cleanup(ctx context.Context, p publish.Principal, reason string, dryRun bool) (publish.CleanupResult, error)
	OnlinePause(ctx context.Context, p publish.Principal, reason string) (publish.OnlinePolicyResult, error)
	OnlineResume(ctx context.Context, p publish.Principal, reason string) (publish.OnlinePolicyResult, error)
	OnlineRetry(ctx context.Context, p publish.Principal, reason string) (publish.OnlineRetryResult, error)
	IntakeAuthorize(ctx context.Context, p publish.Principal, channel string, r publish.IntakeAuthorizeRequest) (*registry.Authorization, bool, error)
	IntakeClose(ctx context.Context, p publish.Principal, id int64, reason string) (bool, error)
	IntakeList(ctx context.Context, p publish.Principal) (publish.IntakeList, error)
	IntakePause(ctx context.Context, p publish.Principal, reason string) (publish.IntakePolicyResult, error)
	IntakeResume(ctx context.Context, p publish.Principal, reason string) (publish.IntakePolicyResult, error)
	Metrics(ctx context.Context) ([]byte, error)
	AuditDenied(ctx context.Context, p publish.Principal, action, reason string)
	Ping(ctx context.Context) error
}

// Limits of the operator API.
const (
	MaxBodyBytes    = 16 << 10
	MaxReasonRunes  = 500
	MaxAuditLimit   = 500
	maxAuthorizeAge = 366 * 24 * time.Hour
	// deniedAuditPerMinute bounds audit rows written for refused requests,
	// so unauthenticated traffic cannot flood the audit log.
	deniedAuditPerMinute = 30
)

// Error codes of the operator API (openapi/operator.yaml).
const (
	CodeUnauthenticated    = "unauthenticated"
	CodeForbidden          = "forbidden"
	CodeInvalidRequest     = "invalid_request"
	CodeBodyTooLarge       = "request_body_too_large"
	CodeUnsupportedMedia   = "unsupported_media_type"
	CodeMethodNotAllowed   = "method_not_allowed"
	CodeNotFound           = "not_found"
	CodeUnknownRelease     = "unknown_release"
	CodeNotEligible        = "release_not_eligible"
	CodeIncompatible       = "release_incompatible"
	CodeActiveChanged      = "active_release_changed"
	CodeBusy               = "busy"
	CodeNoRollbackTarget   = "no_rollback_target"
	CodePolicyRefused      = "policy_refused"
	CodeTimeout            = "timeout"
	CodeServiceUnavailable = "service_unavailable"
	CodeInternal           = "internal_error"
	CodeOnlineDisabled     = "online_disabled"
	CodeNothingToRetry     = "nothing_to_retry"
	// CodeNotRevalidatable: this build cannot evaluate the release (another
	// schema or style revision, or its row counts are unreadable).
	CodeNotRevalidatable = "release_not_revalidatable"
	// CodeRevalidationInProgress: an evaluation of the release is already
	// running (one at a time per release, across processes).
	CodeRevalidationInProgress = "revalidation_in_progress"
	// Stage 5.
	CodeIntakeDisabled         = "intake_disabled"
	CodeIntakeLimit            = "intake_limit_reached"
	CodeIntakeRefused          = "intake_refused"
	CodeCredentialsUnavailable = "credentials_unavailable"
)

// MaxIntakeTTL bounds ttl_seconds before the publisher's own cap applies.
const MaxIntakeTTL = 366 * 24 * time.Hour

// Server is the operator HTTP API.
type Server struct {
	creds   CredentialSource
	svc     Service
	log     *slog.Logger
	timeout time.Duration

	deniedMu     sync.Mutex
	deniedWindow time.Time
	deniedCount  int
}

// New builds the operator handler. creds is consulted for every request
// (a reloading file in the publisher); timeout bounds every request.
func New(creds CredentialSource, svc Service, log *slog.Logger, timeout time.Duration) http.Handler {
	s := &Server{creds: creds, svc: svc, log: log, timeout: timeout}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("GET /v1/operator/status", s.auth(ScopeStatus, "status", s.status))
	mux.Handle("GET /v1/operator/audit", s.auth(ScopeStatus, "audit", s.audit))
	mux.Handle("GET /v1/operator/openapi.yaml", s.auth(ScopeStatus, "openapi", func(w http.ResponseWriter, _ *http.Request, _ *Credential) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		_, _ = w.Write(openapi.OperatorSpec)
	}))
	mux.Handle("POST /v1/operator/authorizations", s.auth(ScopePublish, "authorize_digest", s.authorize))
	mux.Handle("POST /v1/operator/authorizations/{sha256}/revoke", s.auth(ScopePublish, "revoke_digest", s.revoke))
	mux.Handle("POST /v1/operator/releases/{release_id}/activate", s.auth(ScopePublish, "activate", s.activate))
	mux.Handle("POST /v1/operator/releases/{release_id}/revalidate", s.auth(ScopePublish, "revalidate", s.revalidate))
	mux.Handle("POST /v1/operator/rollback", s.auth(ScopeRollback, "rollback", s.rollback))
	mux.Handle("POST /v1/operator/cleanup", s.auth(ScopeCleanup, "cleanup", s.cleanup))
	mux.Handle("GET /v1/operator/metrics", s.auth(ScopeStatus, "metrics", s.metrics))
	mux.Handle("POST /v1/operator/online/pause", s.auth(ScopePublish, "online_pause", s.onlinePolicy(false)))
	mux.Handle("POST /v1/operator/online/resume", s.auth(ScopePublish, "online_resume", s.onlinePolicy(true)))
	mux.Handle("POST /v1/operator/online/retry", s.auth(ScopePublish, "online_retry", s.onlineRetry))
	// The local intake (Stage 5): only intake credentials, only their own
	// bounded authorizations; pausing and resuming the watcher is an
	// operator action.
	mux.Handle("POST /v1/operator/intake/authorizations", s.authIntake("intake_authorize", s.intakeAuthorize))
	mux.Handle("GET /v1/operator/intake/authorizations", s.authIntake("intake_list", s.intakeList))
	mux.Handle("POST /v1/operator/intake/authorizations/{id}/close", s.authIntake("intake_close", s.intakeClose))
	mux.Handle("POST /v1/operator/intake/pause", s.auth(ScopePublish, "intake_pause", s.intakePolicy(false)))
	mux.Handle("POST /v1/operator/intake/resume", s.auth(ScopePublish, "intake_resume", s.intakePolicy(true)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such operator resource")
	})
	return s.middleware(mux)
}

type ctxKey int

const requestIDKey ctxKey = 1

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

type recorder struct {
	http.ResponseWriter
	status int
	actor  string
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// middleware: request id, deadline, security headers, access log (method,
// path, status, actor; never headers, tokens, bodies or query strings),
// panic recovery and the method and body rules.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-ID")
		if !requestIDPattern.MatchString(id) {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), requestIDKey, id), s.timeout)
		defer cancel()
		r = r.WithContext(ctx)
		rec := &recorder{ResponseWriter: w}
		h := rec.Header()
		h.Set("X-Request-ID", id)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("panic", "request_id", id, "panic", p, "stack", string(debug.Stack()))
				if rec.status == 0 {
					writeError(rec, r, http.StatusInternalServerError, CodeInternal, "internal error")
				}
			}
			if r.URL.Path == "/health/live" && rec.status == http.StatusOK {
				return // the container health probe, every few seconds
			}
			s.log.Info("operator request", "request_id", id, "method", r.Method, "path", r.URL.Path, "status", rec.status,
				"actor", rec.actor, "ms", time.Since(start).Milliseconds())
		}()
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
				writeError(rec, r, http.StatusRequestEntityTooLarge, CodeBodyTooLarge, "GET requests must not have a body")
				return
			}
		case http.MethodPost:
			if r.ContentLength > MaxBodyBytes {
				writeError(rec, r, http.StatusRequestEntityTooLarge, CodeBodyTooLarge, fmt.Sprintf("request bodies are limited to %d bytes", MaxBodyBytes))
				return
			}
			r.Body = http.MaxBytesReader(rec, r.Body, MaxBodyBytes)
		default:
			h.Set("Allow", "GET, HEAD, POST")
			writeError(rec, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "only GET, HEAD and POST are supported")
			return
		}
		next.ServeHTTP(rec, r)
	})
}

type handler func(w http.ResponseWriter, r *http.Request, c *Credential)

// authenticate resolves the request's credential; it answers the request
// itself (401, or 503 while the credentials file is unusable) and returns
// nil when there is none.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, action string) *Credential {
	creds, err := s.creds.Credentials()
	if err != nil {
		w.Header().Set("Retry-After", "10")
		writeError(w, r, http.StatusServiceUnavailable, CodeCredentialsUnavailable,
			"the operator credentials file cannot be read or does not parse; every request is refused until it is fixed")
		return nil
	}
	c, err := Authenticate(creds, r.Header.Get("Authorization"))
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="karta-operator"`)
		s.denied(r, "anonymous", action, err.Error())
		writeError(w, r, http.StatusUnauthorized, CodeUnauthenticated, "a valid operator bearer token is required")
		return nil
	}
	if rec, ok := w.(*recorder); ok {
		rec.actor = c.Name
	}
	return c
}

// auth authenticates the request and checks the scope; refusals are logged
// and audited (rate-limited).
func (s *Server) auth(scope, action string, next handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := s.authenticate(w, r, action)
		if c == nil {
			return
		}
		if !c.Scopes[scope] {
			s.denied(r, c.Name, action, "credential lacks the "+scope+" scope")
			writeError(w, r, http.StatusForbidden, CodeForbidden, "this credential lacks the "+scope+" scope")
			return
		}
		next(w, r, c)
	})
}

// authIntake admits only intake credentials (intake_watch or
// intake_submit); the channel of what they create is their scope.
func (s *Server) authIntake(action string, next func(w http.ResponseWriter, r *http.Request, c *Credential, channel string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := s.authenticate(w, r, action)
		if c == nil {
			return
		}
		ch := c.IntakeChannel()
		if ch == "" {
			s.denied(r, c.Name, action, "credential lacks an intake scope")
			writeError(w, r, http.StatusForbidden, CodeForbidden, "this endpoint is for intake credentials (scope intake_watch or intake_submit)")
			return
		}
		next(w, r, c, ch)
	})
}

func (s *Server) denied(r *http.Request, actor, action, reason string) {
	s.log.Warn("operator request denied", "request_id", requestID(r.Context()), "path", r.URL.Path, "actor", actor, "reason", reason)
	s.deniedMu.Lock()
	now := time.Now()
	if now.Sub(s.deniedWindow) >= time.Minute {
		s.deniedWindow, s.deniedCount = now, 0
	}
	s.deniedCount++
	allow := s.deniedCount <= deniedAuditPerMinute
	s.deniedMu.Unlock()
	if allow {
		s.svc.AuditDenied(r.Context(), publish.Principal{Name: actor, Source: "operator_api", RequestID: requestID(r.Context())}, action, reason)
	}
}

func principal(r *http.Request, c *Credential) publish.Principal {
	return publish.Principal{Name: c.Name, Source: "operator_api", RequestID: requestID(r.Context())}
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	ReasonCode string `json:"reason_code,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status, b = http.StatusInternalServerError, []byte(`{"error":{"code":"internal_error","message":"encoding failed"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeErrorReason(w, r, status, code, "", msg)
}

func writeErrorReason(w http.ResponseWriter, r *http.Request, status int, code, reasonCode, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: msg, ReasonCode: reasonCode, RequestID: requestID(r.Context())}})
}

// serviceError maps service errors to responses; unexpected errors are
// logged with details and answered generically.
func (s *Server) serviceError(w http.ResponseWriter, r *http.Request, err error) {
	var refusal *publish.IntakeRefusal
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, r, http.StatusNotFound, CodeUnknownRelease, err.Error())
	case errors.Is(err, registry.ErrNotEligible):
		writeError(w, r, http.StatusConflict, CodeNotEligible, err.Error())
	case errors.Is(err, publish.ErrIncompatible):
		writeError(w, r, http.StatusConflict, CodeIncompatible, err.Error())
	case errors.Is(err, publish.ErrRevalidation):
		writeError(w, r, http.StatusConflict, CodeNotRevalidatable, err.Error())
	case errors.Is(err, registry.ErrActiveChanged):
		writeError(w, r, http.StatusConflict, CodeActiveChanged, err.Error())
	case errors.Is(err, registry.ErrRevalidating):
		w.Header().Set("Retry-After", "30")
		writeError(w, r, http.StatusConflict, CodeRevalidationInProgress, err.Error())
	case errors.Is(err, registry.ErrBusy):
		w.Header().Set("Retry-After", "5")
		writeError(w, r, http.StatusConflict, CodeBusy, err.Error())
	case errors.Is(err, publish.ErrNoRollbackTarget):
		writeError(w, r, http.StatusConflict, CodeNoRollbackTarget, err.Error())
	case errors.Is(err, publish.ErrOnlineDisabled):
		writeError(w, r, http.StatusConflict, CodeOnlineDisabled, err.Error())
	case errors.Is(err, publish.ErrNothingToRetry):
		writeError(w, r, http.StatusConflict, CodeNothingToRetry, err.Error())
	case errors.Is(err, publish.ErrIntakeDisabled):
		writeError(w, r, http.StatusConflict, CodeIntakeDisabled, err.Error())
	case errors.Is(err, publish.ErrIntakeLimit):
		writeError(w, r, http.StatusConflict, CodeIntakeLimit, err.Error())
	case errors.Is(err, publish.ErrIntakeNotOwned):
		writeError(w, r, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, publish.ErrPolicy):
		writeErrorReason(w, r, http.StatusConflict, CodePolicyRefused, publish.PolicyCode(err), err.Error())
	case errors.As(err, &refusal):
		writeErrorReason(w, r, http.StatusConflict, CodeIntakeRefused, refusal.Code, refusal.Msg)
	case errors.Is(err, context.DeadlineExceeded):
		w.Header().Set("Retry-After", "5")
		writeError(w, r, http.StatusServiceUnavailable, CodeTimeout, "the operation did not finish within the request timeout")
	default:
		s.log.Error("operator action failed", "request_id", requestID(r.Context()), "path", r.URL.Path, "err", err)
		w.Header().Set("Retry-After", "5")
		writeError(w, r, http.StatusServiceUnavailable, CodeServiceUnavailable, "the operation failed; see the publisher log for request "+requestID(r.Context()))
	}
}

// decode reads a strict JSON body: application/json, one object, no
// unknown fields, no trailing data, within the size limit.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	ct := r.Header.Get("Content-Type")
	if mt, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(strings.ToLower(mt)) != "application/json" {
		writeError(w, r, http.StatusUnsupportedMediaType, CodeUnsupportedMedia, "the body must be application/json")
		return false
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, r, http.StatusRequestEntityTooLarge, CodeBodyTooLarge, fmt.Sprintf("request bodies are limited to %d bytes", MaxBodyBytes))
			return false
		}
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "could not read the request body")
		return false
	}
	if !utf8.Valid(b) {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "the body must be UTF-8")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "invalid JSON body: "+err.Error())
		return false
	}
	if dec.More() {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "the body must hold exactly one JSON object")
		return false
	}
	return true
}

// validReason checks the free-text reason every action records.
func validReason(w http.ResponseWriter, r *http.Request, reason string) (string, bool) {
	reason = strings.TrimSpace(reason)
	switch {
	case reason == "":
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "reason is required: it is recorded in the audit log")
	case utf8.RuneCountInString(reason) > MaxReasonRunes:
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("reason must be at most %d characters", MaxReasonRunes))
	case strings.IndexFunc(reason, unicode.IsControl) >= 0:
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "reason must not contain control characters")
	default:
		return reason, true
	}
	return "", false
}

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Server) status(w http.ResponseWriter, r *http.Request, _ *Credential) {
	if r.URL.RawQuery != "" {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "this endpoint takes no query parameters")
		return
	}
	st, err := s.svc.Status(r.Context())
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request, _ *Credential) {
	q := r.URL.Query()
	limit, before := 100, int64(0)
	for k, v := range q {
		if len(v) != 1 {
			writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "parameter "+k+" given more than once")
			return
		}
		switch k {
		case "limit":
			n, err := strconv.Atoi(v[0])
			if err != nil || n < 1 || n > MaxAuditLimit {
				writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("limit must be an integer 1..%d", MaxAuditLimit))
				return
			}
			limit = n
		case "before_id":
			n, err := strconv.ParseInt(v[0], 10, 64)
			if err != nil || n < 1 {
				writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "before_id must be a positive integer")
				return
			}
			before = n
		default:
			writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "unknown parameter "+strconv.Quote(k))
			return
		}
	}
	entries, err := s.svc.Audit(r.Context(), limit, before)
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	if entries == nil {
		entries = []registry.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

type authorizeBody struct {
	SHA256    string  `json:"sha256"`
	SizeBytes *int64  `json:"size_bytes"`
	ExpiresAt *string `json:"expires_at"`
	Reason    string  `json:"reason"`
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request, c *Credential) {
	var b authorizeBody
	if !decode(w, r, &b) {
		return
	}
	reason, ok := validReason(w, r, b.Reason)
	if !ok {
		return
	}
	if !digestPattern.MatchString(b.SHA256) {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "sha256 must be 64 lowercase hex digits")
		return
	}
	if b.SizeBytes != nil && (*b.SizeBytes < 1 || *b.SizeBytes > 1<<50) {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "size_bytes must be a positive byte count")
		return
	}
	var expires *time.Time
	if b.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *b.ExpiresAt)
		now := time.Now()
		if err != nil || !t.After(now) || t.After(now.Add(maxAuthorizeAge)) {
			writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "expires_at must be an RFC 3339 time in the future, at most one year ahead")
			return
		}
		expires = &t
	}
	a, created, err := s.svc.Authorize(r.Context(), principal(r, c), b.SHA256, b.SizeBytes, expires, reason)
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"authorization": a, "created": created})
}

type reasonBody struct {
	Reason string `json:"reason"`
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request, c *Credential) {
	digest := r.PathValue("sha256")
	if !digestPattern.MatchString(digest) {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "sha256 must be 64 lowercase hex digits")
		return
	}
	var b reasonBody
	if !decode(w, r, &b) {
		return
	}
	reason, ok := validReason(w, r, b.Reason)
	if !ok {
		return
	}
	revoked, err := s.svc.Revoke(r.Context(), principal(r, c), digest, reason)
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": revoked})
}

type switchBody struct {
	ReleaseID         string `json:"release_id"`
	ExpectedActive    string `json:"expected_active_release_id"`
	AllowRegionChange bool   `json:"allow_region_change"`
	Reason            string `json:"reason"`
}

func validExpected(w http.ResponseWriter, r *http.Request, v string) bool {
	if v != "" && v != "none" && !releaseid.Valid(v) {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "expected_active_release_id must be a release id or \"none\"")
		return false
	}
	return true
}

func (s *Server) activate(w http.ResponseWriter, r *http.Request, c *Credential) {
	id := r.PathValue("release_id")
	if !releaseid.Valid(id) {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "release_id must match "+releaseid.Pattern.String())
		return
	}
	var b switchBody
	if !decode(w, r, &b) {
		return
	}
	reason, ok := validReason(w, r, b.Reason)
	if !ok || !validExpected(w, r, b.ExpectedActive) {
		return
	}
	if b.ReleaseID != "" && b.ReleaseID != id {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "release_id in the body differs from the path")
		return
	}
	res, err := s.svc.Activate(r.Context(), principal(r, c), publish.ActionRequest{ReleaseID: id, ExpectedActive: b.ExpectedActive,
		AllowRegionChange: b.AllowRegionChange, Reason: reason})
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res})
}

type revalidateBody struct {
	Reason string `json:"reason"`
}

// revalidate evaluates an existing release against the content policy in
// force, without a rebuild; it never activates it. A completed evaluation is
// 200 whether the release passed or not ("passed").
func (s *Server) revalidate(w http.ResponseWriter, r *http.Request, c *Credential) {
	id := r.PathValue("release_id")
	if !releaseid.Valid(id) {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "release_id must match "+releaseid.Pattern.String())
		return
	}
	var b revalidateBody
	if !decode(w, r, &b) {
		return
	}
	reason, ok := validReason(w, r, b.Reason)
	if !ok {
		return
	}
	res, err := s.svc.Revalidate(r.Context(), principal(r, c), id, reason)
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res})
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request, c *Credential) {
	var b switchBody
	if !decode(w, r, &b) {
		return
	}
	reason, ok := validReason(w, r, b.Reason)
	if !ok || !validExpected(w, r, b.ExpectedActive) {
		return
	}
	if b.ReleaseID != "" && !releaseid.Valid(b.ReleaseID) {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "release_id must match "+releaseid.Pattern.String())
		return
	}
	res, err := s.svc.Rollback(r.Context(), principal(r, c), publish.ActionRequest{ReleaseID: b.ReleaseID, ExpectedActive: b.ExpectedActive,
		AllowRegionChange: b.AllowRegionChange, Reason: reason})
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res})
}

type cleanupBody struct {
	Reason string `json:"reason"`
	DryRun bool   `json:"dry_run"`
}

func (s *Server) cleanup(w http.ResponseWriter, r *http.Request, c *Credential) {
	var b cleanupBody
	if !decode(w, r, &b) {
		return
	}
	reason, ok := validReason(w, r, b.Reason)
	if !ok {
		return
	}
	res, err := s.svc.Cleanup(r.Context(), principal(r, c), reason, b.DryRun)
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request, _ *Credential) {
	if r.URL.RawQuery != "" {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "this endpoint takes no query parameters")
		return
	}
	b, err := s.svc.Metrics(r.Context())
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (s *Server) onlinePolicy(resume bool) handler {
	return func(w http.ResponseWriter, r *http.Request, c *Credential) {
		var b reasonBody
		if !decode(w, r, &b) {
			return
		}
		reason, ok := validReason(w, r, b.Reason)
		if !ok {
			return
		}
		f := s.svc.OnlinePause
		if resume {
			f = s.svc.OnlineResume
		}
		res, err := f(r.Context(), principal(r, c), reason)
		if err != nil {
			s.serviceError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}

func (s *Server) onlineRetry(w http.ResponseWriter, r *http.Request, c *Credential) {
	var b reasonBody
	if !decode(w, r, &b) {
		return
	}
	reason, ok := validReason(w, r, b.Reason)
	if !ok {
		return
	}
	res, err := s.svc.OnlineRetry(r.Context(), principal(r, c), reason)
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queued": res})
}

type intakeAuthorizeBody struct {
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"size_bytes"`
	RegionID   string `json:"region_id"`
	TTLSeconds int64  `json:"ttl_seconds"`
	Name       string `json:"name"`
	Reason     string `json:"reason"`
}

var (
	regionIDPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	intakeNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	authorizationIDPat = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
)

func (s *Server) intakeAuthorize(w http.ResponseWriter, r *http.Request, c *Credential, channel string) {
	var b intakeAuthorizeBody
	if !decode(w, r, &b) {
		return
	}
	reason, ok := validReason(w, r, b.Reason)
	if !ok {
		return
	}
	switch {
	case !digestPattern.MatchString(b.SHA256):
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "sha256 must be 64 lowercase hex digits")
	case b.SizeBytes < 1 || b.SizeBytes > 1<<50:
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "size_bytes is required: the exact size of the snapshot")
	case !regionIDPattern.MatchString(b.RegionID):
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "region_id is required: the region the delivery is for")
	case b.TTLSeconds < 60 || time.Duration(b.TTLSeconds)*time.Second > MaxIntakeTTL:
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "ttl_seconds is required: the validity, at least 60 s and at most the publisher's cap")
	case !intakeNamePattern.MatchString(b.Name):
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "name must be the handoff name, matching "+intakeNamePattern.String())
	default:
		a, created, err := s.svc.IntakeAuthorize(r.Context(), principal(r, c), channel, publish.IntakeAuthorizeRequest{SHA256: b.SHA256,
			SizeBytes: b.SizeBytes, RegionID: b.RegionID, TTL: time.Duration(b.TTLSeconds) * time.Second, Name: b.Name, Reason: reason})
		if err != nil {
			s.serviceError(w, r, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, map[string]any{"authorization": a, "created": created})
	}
}

func (s *Server) intakeList(w http.ResponseWriter, r *http.Request, c *Credential, _ string) {
	if r.URL.RawQuery != "" {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "this endpoint takes no query parameters")
		return
	}
	l, err := s.svc.IntakeList(r.Context(), principal(r, c))
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) intakeClose(w http.ResponseWriter, r *http.Request, c *Credential, _ string) {
	raw := r.PathValue("id")
	if !authorizationIDPat.MatchString(raw) {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "the authorization id must be a positive integer")
		return
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "the authorization id must be a positive integer")
		return
	}
	var b reasonBody
	if !decode(w, r, &b) {
		return
	}
	reason, ok := validReason(w, r, b.Reason)
	if !ok {
		return
	}
	closed, err := s.svc.IntakeClose(r.Context(), principal(r, c), id, reason)
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"closed": closed})
}

func (s *Server) intakePolicy(resume bool) handler {
	return func(w http.ResponseWriter, r *http.Request, c *Credential) {
		var b reasonBody
		if !decode(w, r, &b) {
			return
		}
		reason, ok := validReason(w, r, b.Reason)
		if !ok {
			return
		}
		f := s.svc.IntakePause
		if resume {
			f = s.svc.IntakeResume
		}
		res, err := f(r.Context(), principal(r, c), reason)
		if err != nil {
			s.serviceError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}
