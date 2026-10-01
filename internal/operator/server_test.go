package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/publish"
	"github.com/pouriya-sedaghat/karta/internal/registry"
)

const (
	adminToken   = "1111111111111111111111111111111111111111111111111111111111111111"
	monitorToken = "2222222222222222222222222222222222222222222222222222222222222222"
	relID        = "r0123456789abcdef01234567"
)

// fakeService records calls and returns canned results or errors.
type fakeService struct {
	mu     sync.Mutex
	calls  []string
	denied []string
	err    error
}

func (f *fakeService) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeService) Status(context.Context) (publish.Status, error) {
	f.record("status")
	return publish.Status{}, f.err
}
func (f *fakeService) Audit(_ context.Context, limit int, before int64) ([]registry.AuditEntry, error) {
	f.record("audit")
	return nil, f.err
}
func (f *fakeService) Authorize(_ context.Context, p publish.Principal, d string, size *int64, exp *time.Time, reason string) (*registry.Authorization, bool, error) {
	f.record("authorize " + p.Name + " " + d + " " + reason)
	return &registry.Authorization{SHA256: d, Reason: reason, CreatedBy: p.Name}, true, f.err
}
func (f *fakeService) Revoke(_ context.Context, p publish.Principal, d, reason string) (bool, error) {
	f.record("revoke " + d)
	return true, f.err
}
func (f *fakeService) Activate(_ context.Context, p publish.Principal, a publish.ActionRequest) (registry.ActivateResult, error) {
	f.record("activate " + a.ReleaseID + " expected=" + a.ExpectedActive)
	return registry.ActivateResult{Active: a.ReleaseID, Changed: true}, f.err
}
func (f *fakeService) Rollback(_ context.Context, p publish.Principal, a publish.ActionRequest) (registry.ActivateResult, error) {
	f.record("rollback " + a.ReleaseID)
	return registry.ActivateResult{Active: a.ReleaseID, Changed: true}, f.err
}
func (f *fakeService) Cleanup(_ context.Context, p publish.Principal, reason string, dry bool) (publish.CleanupResult, error) {
	f.record("cleanup")
	return publish.CleanupResult{DryRun: dry}, f.err
}
func (f *fakeService) OnlinePause(_ context.Context, p publish.Principal, reason string) (publish.OnlinePolicyResult, error) {
	f.record("online_pause " + p.Name + " " + reason)
	return publish.OnlinePolicyResult{Changed: true, Policy: &registry.OnlinePolicy{RegionID: "fixture"}}, f.err
}
func (f *fakeService) OnlineResume(_ context.Context, p publish.Principal, reason string) (publish.OnlinePolicyResult, error) {
	f.record("online_resume " + p.Name + " " + reason)
	return publish.OnlinePolicyResult{Changed: true, Policy: &registry.OnlinePolicy{RegionID: "fixture", AutoActivate: true}}, f.err
}
func (f *fakeService) OnlineRetry(_ context.Context, p publish.Principal, reason string) (publish.OnlineRetryResult, error) {
	f.record("online_retry " + p.Name + " " + reason)
	return publish.OnlineRetryResult{SubmissionID: 7, Name: "fixture-s000000000001-0123456789ab", State: "failed"}, f.err
}
func (f *fakeService) Metrics(context.Context) ([]byte, error) {
	f.record("metrics")
	return []byte("# HELP karta_online_enabled x\n# TYPE karta_online_enabled gauge\nkarta_online_enabled 1\n"), f.err
}
func (f *fakeService) AuditDenied(_ context.Context, p publish.Principal, action, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denied = append(f.denied, p.Name+" "+action)
}
func (f *fakeService) Ping(context.Context) error { return nil }

func setup(t *testing.T) (http.Handler, *fakeService, *bytes.Buffer) {
	t.Helper()
	creds, err := ParseCredentials([]byte("# comment\noperator status,publish,rollback,cleanup " + HashToken(adminToken) +
		"\nmonitor status " + HashToken(monitorToken) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	svc := &fakeService{}
	logs := &bytes.Buffer{}
	return New(creds, svc, slog.New(slog.NewJSONHandler(logs, nil)), 5*time.Second), svc, logs
}

func do(t *testing.T, h http.Handler, method, path, token, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func code(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("not an error body: %s", rec.Body)
	}
	return e.Error.Code
}

func TestParseCredentials(t *testing.T) {
	for name, content := range map[string]string{
		"empty":         "# nothing\n",
		"bad name":      "Op status " + HashToken(adminToken),
		"bad scope":     "op root " + HashToken(adminToken),
		"short hash":    "op status abc",
		"repeated name": "op status " + HashToken(adminToken) + "\nop status " + HashToken(monitorToken),
		"repeated hash": "a status " + HashToken(adminToken) + "\nb status " + HashToken(adminToken),
		"extra field":   "op status " + HashToken(adminToken) + " x",
	} {
		if _, err := ParseCredentials([]byte(content)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAuthenticationAndScopes(t *testing.T) {
	h, svc, logs := setup(t)
	cases := []struct {
		name, method, path, token, body string
		status                          int
		code                            string
	}{
		{"no token", "GET", "/v1/operator/status", "", "", 401, CodeUnauthenticated},
		{"wrong token", "GET", "/v1/operator/status", strings.Repeat("3", 64), "", 401, CodeUnauthenticated},
		{"malformed token", "GET", "/v1/operator/status", "not-a-token", "", 401, CodeUnauthenticated},
		{"monitor can read status", "GET", "/v1/operator/status", monitorToken, "", 200, ""},
		{"monitor cannot roll back", "POST", "/v1/operator/rollback", monitorToken, `{"reason":"x"}`, 403, CodeForbidden},
		{"monitor cannot clean up", "POST", "/v1/operator/cleanup", monitorToken, `{"reason":"x"}`, 403, CodeForbidden},
		{"monitor cannot authorize", "POST", "/v1/operator/authorizations", monitorToken, `{"sha256":"` + strings.Repeat("a", 64) + `","reason":"x"}`, 403, CodeForbidden},
		{"admin rolls back", "POST", "/v1/operator/rollback", adminToken, `{"reason":"bad data"}`, 200, ""},
	}
	for _, c := range cases {
		rec := do(t, h, c.method, c.path, c.token, c.body, nil)
		if rec.Code != c.status || (c.code != "" && code(t, rec) != c.code) {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
		if rec.Code == 401 && rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: no WWW-Authenticate", c.name)
		}
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Request-ID") == "" {
			t.Errorf("%s: headers %v", c.name, rec.Header())
		}
	}
	if len(svc.denied) != 6 || svc.denied[0] != "anonymous status" || svc.denied[3] != "monitor rollback" {
		t.Errorf("denied audit %v", svc.denied)
	}
	// Tokens never reach the log.
	for _, tok := range []string{adminToken, monitorToken, strings.Repeat("3", 64)} {
		if strings.Contains(logs.String(), tok) {
			t.Fatal("a token was logged")
		}
	}
	if !strings.Contains(logs.String(), `"actor":"operator"`) {
		t.Error("the actor of an accepted request is not logged")
	}
}

func TestRequestValidation(t *testing.T) {
	h, svc, _ := setup(t)
	digest := strings.Repeat("a", 64)
	cases := []struct {
		name, method, path, body string
		hdr                      map[string]string
		status                   int
		code                     string
	}{
		{"missing reason", "POST", "/v1/operator/rollback", `{}`, nil, 400, CodeInvalidRequest},
		{"blank reason", "POST", "/v1/operator/rollback", `{"reason":"   "}`, nil, 400, CodeInvalidRequest},
		{"control character", "POST", "/v1/operator/rollback", `{"reason":"a\u0007b"}`, nil, 400, CodeInvalidRequest},
		{"long reason", "POST", "/v1/operator/rollback", `{"reason":"` + strings.Repeat("x", 501) + `"}`, nil, 400, CodeInvalidRequest},
		{"unknown field", "POST", "/v1/operator/rollback", `{"reason":"x","force":true}`, nil, 400, CodeInvalidRequest},
		{"two objects", "POST", "/v1/operator/rollback", `{"reason":"x"}{"reason":"y"}`, nil, 400, CodeInvalidRequest},
		{"not json", "POST", "/v1/operator/rollback", `reason=x`, nil, 400, CodeInvalidRequest},
		{"wrong media type", "POST", "/v1/operator/rollback", `{"reason":"x"}`, map[string]string{"Content-Type": "text/plain"}, 415, CodeUnsupportedMedia},
		{"too large", "POST", "/v1/operator/rollback", `{"reason":"` + strings.Repeat("x", MaxBodyBytes) + `"}`, nil, 413, CodeBodyTooLarge},
		{"bad release id", "POST", "/v1/operator/rollback", `{"reason":"x","release_id":"latest"}`, nil, 400, CodeInvalidRequest},
		{"bad expected", "POST", "/v1/operator/rollback", `{"reason":"x","expected_active_release_id":"abc"}`, nil, 400, CodeInvalidRequest},
		{"bad path release id", "POST", "/v1/operator/releases/latest/activate", `{"reason":"x"}`, nil, 400, CodeInvalidRequest},
		{"body id differs", "POST", "/v1/operator/releases/" + relID + "/activate", `{"reason":"x","release_id":"r000000000000000000000000"}`, nil, 400, CodeInvalidRequest},
		{"uppercase digest", "POST", "/v1/operator/authorizations", `{"sha256":"` + strings.ToUpper(digest) + `","reason":"x"}`, nil, 400, CodeInvalidRequest},
		{"zero size", "POST", "/v1/operator/authorizations", `{"sha256":"` + digest + `","size_bytes":0,"reason":"x"}`, nil, 400, CodeInvalidRequest},
		{"past expiry", "POST", "/v1/operator/authorizations", `{"sha256":"` + digest + `","expires_at":"2020-01-01T00:00:00Z","reason":"x"}`, nil, 400, CodeInvalidRequest},
		{"far expiry", "POST", "/v1/operator/authorizations", `{"sha256":"` + digest + `","expires_at":"2999-01-01T00:00:00Z","reason":"x"}`, nil, 400, CodeInvalidRequest},
		{"bad revoke digest", "POST", "/v1/operator/authorizations/xyz/revoke", `{"reason":"x"}`, nil, 400, CodeInvalidRequest},
		{"method", "DELETE", "/v1/operator/status", "", nil, 405, CodeMethodNotAllowed},
		{"GET with body", "GET", "/v1/operator/status", `{}`, nil, 413, CodeBodyTooLarge},
		{"status query", "GET", "/v1/operator/status?x=1", "", nil, 400, CodeInvalidRequest},
		{"audit limit", "GET", "/v1/operator/audit?limit=501", "", nil, 400, CodeInvalidRequest},
		{"audit unknown", "GET", "/v1/operator/audit?x=1", "", nil, 400, CodeInvalidRequest},
		{"unknown path", "GET", "/v1/operator/secrets", "", nil, 404, CodeNotFound},
	}
	for _, c := range cases {
		rec := do(t, h, c.method, c.path, adminToken, c.body, c.hdr)
		if rec.Code != c.status || code(t, rec) != c.code {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	if len(svc.calls) != 0 {
		t.Errorf("invalid requests reached the service: %v", svc.calls)
	}
	// Valid requests reach it with the credential name and trimmed reason.
	rec := do(t, h, "POST", "/v1/operator/authorizations", adminToken, `{"sha256":"`+digest+`","size_bytes":10,"reason":"  verified out of band  "}`, nil)
	if rec.Code != 201 || svc.calls[0] != "authorize operator "+digest+" verified out of band" {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, svc.calls)
	}
	rec = do(t, h, "POST", "/v1/operator/releases/"+relID+"/activate", adminToken, `{"reason":"x","expected_active_release_id":"none"}`, nil)
	if rec.Code != 200 || svc.calls[1] != "activate "+relID+" expected=none" {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, svc.calls)
	}
}

func TestServiceErrorMapping(t *testing.T) {
	h, svc, _ := setup(t)
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{registry.ErrNotFound, 404, CodeUnknownRelease},
		{registry.ErrNotEligible, 409, CodeNotEligible},
		{publish.ErrIncompatible, 409, CodeIncompatible},
		{registry.ErrActiveChanged, 409, CodeActiveChanged},
		{registry.ErrBusy, 409, CodeBusy},
		{publish.ErrNoRollbackTarget, 409, CodeNoRollbackTarget},
		{&publish.PolicyError{Code: publish.CodeOlderThanActive, Msg: "older"}, 409, CodePolicyRefused},
		{context.DeadlineExceeded, 503, CodeTimeout},
		{errors.New("connection refused to 10.0.0.1"), 503, CodeServiceUnavailable},
	}
	for _, c := range cases {
		svc.err = c.err
		rec := do(t, h, "POST", "/v1/operator/rollback", adminToken, `{"reason":"x"}`, nil)
		if rec.Code != c.status || code(t, rec) != c.code {
			t.Errorf("%v: %d %s", c.err, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "10.0.0.1") {
			t.Errorf("internal error details leaked: %s", rec.Body)
		}
	}
	svc.err = &publish.PolicyError{Code: publish.CodeOlderThanActive, Msg: "older"}
	rec := do(t, h, "POST", "/v1/operator/rollback", adminToken, `{"reason":"x"}`, nil)
	if !strings.Contains(rec.Body.String(), `"reason_code":"older_than_active"`) {
		t.Errorf("policy reason code missing: %s", rec.Body)
	}
}

// Refusals are audited at most deniedAuditPerMinute times per minute.
func TestDeniedAuditIsRateLimited(t *testing.T) {
	h, svc, _ := setup(t)
	for i := 0; i < deniedAuditPerMinute+20; i++ {
		do(t, h, "GET", "/v1/operator/status", "", "", nil)
	}
	if len(svc.denied) != deniedAuditPerMinute {
		t.Fatalf("%d denied audit rows", len(svc.denied))
	}
}

func TestLiveNeedsNoToken(t *testing.T) {
	h, _, _ := setup(t)
	if rec := do(t, h, "GET", "/health/live", "", "", nil); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestOnlineEndpoints(t *testing.T) {
	h, svc, _ := setup(t)
	for _, c := range []struct {
		name, method, path, token, body string
		status                          int
		code, call                      string
	}{
		{"monitor cannot pause", "POST", "/v1/operator/online/pause", monitorToken, `{"reason":"x"}`, 403, CodeForbidden, ""},
		{"monitor cannot resume", "POST", "/v1/operator/online/resume", monitorToken, `{"reason":"x"}`, 403, CodeForbidden, ""},
		{"monitor cannot retry", "POST", "/v1/operator/online/retry", monitorToken, `{"reason":"x"}`, 403, CodeForbidden, ""},
		{"pause needs a reason", "POST", "/v1/operator/online/pause", adminToken, `{}`, 400, CodeInvalidRequest, ""},
		{"pause rejects unknown fields", "POST", "/v1/operator/online/pause", adminToken, `{"reason":"x","force":true}`, 400, CodeInvalidRequest, ""},
		{"pause", "POST", "/v1/operator/online/pause", adminToken, `{"reason":"provider incident"}`, 200, "", "online_pause operator provider incident"},
		{"resume", "POST", "/v1/operator/online/resume", adminToken, `{"reason":"fixed"}`, 200, "", "online_resume operator fixed"},
		{"retry", "POST", "/v1/operator/online/retry", adminToken, `{"reason":"disk freed"}`, 200, "", "online_retry operator disk freed"},
		{"metrics need a token", "GET", "/v1/operator/metrics", "", "", 401, CodeUnauthenticated, ""},
		{"metrics take no query", "GET", "/v1/operator/metrics?x=1", monitorToken, "", 400, CodeInvalidRequest, ""},
		{"monitor reads metrics", "GET", "/v1/operator/metrics", monitorToken, "", 200, "", "metrics"},
	} {
		svc.calls = nil
		rec := do(t, h, c.method, c.path, c.token, c.body, nil)
		if rec.Code != c.status || (c.code != "" && code(t, rec) != c.code) {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
		if c.call != "" && (len(svc.calls) != 1 || svc.calls[0] != c.call) {
			t.Errorf("%s: calls %v", c.name, svc.calls)
		}
		if c.call == "" && len(svc.calls) != 0 {
			t.Errorf("%s: the service was called: %v", c.name, svc.calls)
		}
	}
	rec := do(t, h, "GET", "/v1/operator/metrics", monitorToken, "", nil)
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" || !strings.Contains(rec.Body.String(), "karta_online_enabled 1") {
		t.Errorf("metrics: %q %s", ct, rec.Body)
	}
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{publish.ErrOnlineDisabled, 409, CodeOnlineDisabled},
		{publish.ErrNothingToRetry, 409, CodeNothingToRetry},
	} {
		svc.err = c.err
		rec := do(t, h, "POST", "/v1/operator/online/retry", adminToken, `{"reason":"x"}`, nil)
		if rec.Code != c.status || code(t, rec) != c.code {
			t.Errorf("%v: %d %s", c.err, rec.Code, rec.Body)
		}
	}
}
