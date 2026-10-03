package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	watchToken   = "4444444444444444444444444444444444444444444444444444444444444444"
	submitToken  = "5555555555555555555555555555555555555555555555555555555555555555"
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
func (f *fakeService) IntakeAuthorize(_ context.Context, p publish.Principal, channel string, r publish.IntakeAuthorizeRequest) (*registry.Authorization, bool, error) {
	f.record(fmt.Sprintf("intake_authorize %s %s %s %d %s %s %s", p.Name, channel, r.SHA256, r.SizeBytes, r.RegionID, r.TTL, r.Name))
	return &registry.Authorization{ID: 9, SHA256: r.SHA256, CreatedBy: p.Name, Channel: channel}, true, f.err
}
func (f *fakeService) IntakeClose(_ context.Context, p publish.Principal, id int64, reason string) (bool, error) {
	f.record(fmt.Sprintf("intake_close %s %d %s", p.Name, id, reason))
	return true, f.err
}
func (f *fakeService) IntakeList(_ context.Context, p publish.Principal) (publish.IntakeList, error) {
	f.record("intake_list " + p.Name)
	return publish.IntakeList{Records: []registry.IntakeRecord{}}, f.err
}
func (f *fakeService) IntakePause(_ context.Context, p publish.Principal, reason string) (publish.IntakePolicyResult, error) {
	f.record("intake_pause " + p.Name + " " + reason)
	return publish.IntakePolicyResult{Changed: true}, f.err
}
func (f *fakeService) IntakeResume(_ context.Context, p publish.Principal, reason string) (publish.IntakePolicyResult, error) {
	f.record("intake_resume " + p.Name + " " + reason)
	return publish.IntakePolicyResult{Changed: true}, f.err
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
		"\nmonitor status " + HashToken(monitorToken) + "\nlocal-intake intake_watch " + HashToken(watchToken) +
		"\nalice intake_submit " + HashToken(submitToken) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	svc := &fakeService{}
	logs := &bytes.Buffer{}
	return New(StaticCredentials(creds), svc, slog.New(slog.NewJSONHandler(logs, nil)), 5*time.Second), svc, logs
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
		// An intake scope must be a credential's only scope.
		"intake with publish": "w intake_watch,publish " + HashToken(adminToken),
		"intake with status":  "w status,intake_submit " + HashToken(adminToken),
		"both intake scopes":  "w intake_watch,intake_submit " + HashToken(adminToken),
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

func TestIntakeCredentialsParse(t *testing.T) {
	creds, err := ParseCredentials([]byte("w intake_watch " + HashToken(watchToken) + "\ns intake_submit " + HashToken(submitToken) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if creds[0].IntakeChannel() != ScopeIntakeWatch || creds[1].IntakeChannel() != ScopeIntakeSubmit {
		t.Errorf("channels %q %q", creds[0].IntakeChannel(), creds[1].IntakeChannel())
	}
}

// An intake credential reaches only its own endpoints, and other
// credentials do not reach the intake's (negative controls for the narrow
// capability, ADR 0006).
func TestIntakeScopeIsolation(t *testing.T) {
	h, svc, _ := setup(t)
	digest := strings.Repeat("a", 64)
	others := []struct{ method, path, body string }{
		{"GET", "/v1/operator/status", ""},
		{"GET", "/v1/operator/audit", ""},
		{"GET", "/v1/operator/openapi.yaml", ""},
		{"GET", "/v1/operator/metrics", ""},
		{"POST", "/v1/operator/authorizations", `{"sha256":"` + digest + `","size_bytes":10,"reason":"x"}`},
		{"POST", "/v1/operator/authorizations/" + digest + "/revoke", `{"reason":"x"}`},
		{"POST", "/v1/operator/releases/" + relID + "/activate", `{"reason":"x"}`},
		{"POST", "/v1/operator/rollback", `{"reason":"x"}`},
		{"POST", "/v1/operator/cleanup", `{"reason":"x"}`},
		{"POST", "/v1/operator/online/pause", `{"reason":"x"}`},
		{"POST", "/v1/operator/online/resume", `{"reason":"x"}`},
		{"POST", "/v1/operator/online/retry", `{"reason":"x"}`},
		{"POST", "/v1/operator/intake/pause", `{"reason":"x"}`},
		{"POST", "/v1/operator/intake/resume", `{"reason":"x"}`},
	}
	for _, tok := range []string{watchToken, submitToken} {
		for _, o := range others {
			rec := do(t, h, o.method, o.path, tok, o.body, nil)
			if rec.Code != 403 || code(t, rec) != CodeForbidden {
				t.Errorf("intake token %.4s %s %s: %d %s", tok, o.method, o.path, rec.Code, rec.Body)
			}
		}
	}
	good := `{"sha256":"` + digest + `","size_bytes":10,"region_id":"fixture","ttl_seconds":3600,"name":"fixture-w20261003T120000Z-aaaaaaaaaaaa","reason":"landing file x"}`
	for _, tok := range []string{adminToken, monitorToken} {
		for _, o := range []struct{ method, path, body string }{
			{"POST", "/v1/operator/intake/authorizations", good},
			{"GET", "/v1/operator/intake/authorizations", ""},
			{"POST", "/v1/operator/intake/authorizations/9/close", `{"reason":"x"}`},
		} {
			rec := do(t, h, o.method, o.path, tok, o.body, nil)
			if rec.Code != 403 || code(t, rec) != CodeForbidden {
				t.Errorf("token %.4s %s %s: %d %s", tok, o.method, o.path, rec.Code, rec.Body)
			}
		}
	}
	if len(svc.calls) != 0 {
		t.Fatalf("refused requests reached the service: %v", svc.calls)
	}
	// The channel comes from the scope, never from the request.
	if rec := do(t, h, "POST", "/v1/operator/intake/authorizations", watchToken, good, nil); rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/v1/operator/intake/authorizations", submitToken, good, nil); rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/v1/operator/intake/authorizations", watchToken,
		strings.Replace(good, `"reason"`, `"channel":"intake_submit","reason"`, 1), nil); rec.Code != 400 {
		t.Errorf("a claimed channel was accepted: %d %s", rec.Code, rec.Body)
	}
	want := []string{
		"intake_authorize local-intake intake_watch " + digest + " 10 fixture 1h0m0s fixture-w20261003T120000Z-aaaaaaaaaaaa",
		"intake_authorize alice intake_submit " + digest + " 10 fixture 1h0m0s fixture-w20261003T120000Z-aaaaaaaaaaaa",
	}
	if fmt.Sprint(svc.calls) != fmt.Sprint(want) {
		t.Errorf("calls %v", svc.calls)
	}
	// Pausing and resuming the watcher are operator actions.
	svc.calls = nil
	if rec := do(t, h, "POST", "/v1/operator/intake/resume", adminToken, `{"reason":"checked"}`, nil); rec.Code != 200 ||
		len(svc.calls) != 1 || svc.calls[0] != "intake_resume operator checked" {
		t.Errorf("resume: %d %s %v", rec.Code, rec.Body, svc.calls)
	}
}

func TestIntakeRequestValidation(t *testing.T) {
	h, svc, _ := setup(t)
	digest := strings.Repeat("b", 64)
	body := func(changes map[string]any) string {
		m := map[string]any{"sha256": digest, "size_bytes": 10, "region_id": "fixture", "ttl_seconds": 3600,
			"name": "fixture-c20261003T120000Z-bbbbbbbbbbbb", "reason": "operator attestation"}
		for k, v := range changes {
			if v == nil {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	for name, b := range map[string]string{
		"no size":         body(map[string]any{"size_bytes": nil}),
		"zero size":       body(map[string]any{"size_bytes": 0}),
		"no validity":     body(map[string]any{"ttl_seconds": nil}),
		"short validity":  body(map[string]any{"ttl_seconds": 5}),
		"long validity":   body(map[string]any{"ttl_seconds": 400 * 24 * 3600}),
		"no region":       body(map[string]any{"region_id": nil}),
		"bad region":      body(map[string]any{"region_id": "Fixture"}),
		"no name":         body(map[string]any{"name": nil}),
		"bad name":        body(map[string]any{"name": "../x"}),
		"upper digest":    body(map[string]any{"sha256": strings.ToUpper(digest)}),
		"no reason":       body(map[string]any{"reason": nil}),
		"expires_at":      body(map[string]any{"expires_at": "2030-01-01T00:00:00Z"}),
		"unknown field":   body(map[string]any{"scope": "publish"}),
		"two objects":     body(nil) + body(nil),
		"not json":        "sha256=x",
		"negative ttl":    body(map[string]any{"ttl_seconds": -1}),
		"fractional size": strings.Replace(body(nil), `"size_bytes":10`, `"size_bytes":10.5`, 1),
	} {
		rec := do(t, h, "POST", "/v1/operator/intake/authorizations", submitToken, b, nil)
		if rec.Code != 400 || code(t, rec) != CodeInvalidRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	for name, path := range map[string]string{"zero id": "0", "letters": "abc", "negative": "-1", "huge": strings.Repeat("9", 20)} {
		rec := do(t, h, "POST", "/v1/operator/intake/authorizations/"+path+"/close", submitToken, `{"reason":"x"}`, nil)
		if rec.Code != 400 {
			t.Errorf("close %s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := do(t, h, "GET", "/v1/operator/intake/authorizations?all=1", submitToken, "", nil); rec.Code != 400 {
		t.Errorf("list with a query: %d", rec.Code)
	}
	if len(svc.calls) != 0 {
		t.Fatalf("invalid requests reached the service: %v", svc.calls)
	}
	for _, c := range []struct {
		err          error
		status       int
		code, reason string
	}{
		{publish.ErrIntakeDisabled, 409, CodeIntakeDisabled, ""},
		{fmt.Errorf("%w (2 open)", publish.ErrIntakeLimit), 409, CodeIntakeLimit, ""},
		{publish.ErrIntakeNotOwned, 404, CodeNotFound, ""},
		{&publish.IntakeRefusal{Code: publish.CodeIntakeRegion, Msg: "other region"}, 409, CodeIntakeRefused, publish.CodeIntakeRegion},
		{&publish.IntakeRefusal{Code: publish.CodeIntakeTTL, Msg: "beyond cap"}, 409, CodeIntakeRefused, publish.CodeIntakeTTL},
	} {
		svc.err = c.err
		rec := do(t, h, "POST", "/v1/operator/intake/authorizations", watchToken, body(nil), nil)
		if rec.Code != c.status || code(t, rec) != c.code {
			t.Errorf("%v: %d %s", c.err, rec.Code, rec.Body)
		}
		if c.reason != "" && !strings.Contains(rec.Body.String(), `"reason_code":"`+c.reason+`"`) {
			t.Errorf("%v: reason code missing: %s", c.err, rec.Body)
		}
	}
	svc.err = nil
	svc.calls = nil
	if rec := do(t, h, "POST", "/v1/operator/intake/authorizations/42/close", watchToken, `{"reason":"final"}`, nil); rec.Code != 200 ||
		svc.calls[0] != "intake_close local-intake 42 final" {
		t.Errorf("close: %d %s %v", rec.Code, rec.Body, svc.calls)
	}
}

// The publisher's credentials reload when the file changes and fail
// closed while it is broken (ADR 0006): a removed credential stops at once,
// without a restart, and a half-written file cannot keep it alive.
func TestStrictCredentialReload(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "operator_tokens")
	t0 := time.Now().Add(-time.Hour)
	write := func(content string, mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	both := "operator status,publish " + HashToken(adminToken) + "\nlocal-intake intake_watch " + HashToken(watchToken) + "\n"
	write(both, t0)
	f, err := OpenStrictCredentialFile(path, log)
	if err != nil {
		t.Fatal(err)
	}
	h := New(f, &fakeService{}, log, 5*time.Second)
	status := func(tok string) int {
		return do(t, h, "GET", "/v1/operator/intake/authorizations", tok, "", nil).Code
	}
	if status(watchToken) != 200 {
		t.Fatal("intake credential refused")
	}
	write("operator status,publish "+HashToken(adminToken)+"\n", t0.Add(time.Minute))
	if got := status(watchToken); got != 401 {
		t.Fatalf("a removed credential still works: %d", got)
	}
	write("operator status,publish 12", t0.Add(2*time.Minute))
	rec := do(t, h, "GET", "/v1/operator/status", adminToken, "", nil)
	if rec.Code != 503 || code(t, rec) != CodeCredentialsUnavailable {
		t.Fatalf("a broken file did not fail closed: %d %s", rec.Code, rec.Body)
	}
	if got := status(watchToken); got != 503 {
		t.Fatalf("a broken file let a credential through: %d", got)
	}
	_ = os.Remove(path)
	if got := do(t, h, "GET", "/v1/operator/status", adminToken, "", nil).Code; got != 503 {
		t.Fatalf("a missing file did not fail closed: %d", got)
	}
	write(both, t0.Add(3*time.Minute))
	if status(watchToken) != 200 || do(t, h, "GET", "/v1/operator/status", adminToken, "", nil).Code != 200 {
		t.Fatal("a fixed file was not picked up")
	}
}
