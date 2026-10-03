//go:build integration

// Stage 2 publication tests: the inbox, validation, activation, pinning,
// rollback, cleanup, crash recovery and disk exhaustion, against the running
// stack with the publisher (compose.test.yaml), using only the committed
// fixture snapshots A and B and variants generated from them here.
//
// Environment (set by `make test-integration`):
//
//	KARTA_INBOX_HOST_DIR      the host directory mounted as the publisher's inbox
//	KARTA_TEST_OPERATOR_URL   operator API (default http://127.0.0.1:18081)
//	KARTA_TEST_ARTIFACTS      where measurements are written (artifacts/)
package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/jackc/pgx/v5"

	"github.com/pouriya-sedaghat/karta/internal/pbfwrite"
	"github.com/pouriya-sedaghat/karta/openapi"
)

var (
	opBase    = env("KARTA_TEST_OPERATOR_URL", "http://127.0.0.1:18081")
	inboxDir  = os.Getenv("KARTA_INBOX_HOST_DIR")
	artifacts = env("KARTA_TEST_ARTIFACTS", filepath.Join(repoRoot, "artifacts"))

	opRouterOnce sync.Once
	opRouter     routers.Router
)

const (
	snapA = "testdata/fixture/snapshots/karta-fixture-a.osm.pbf"
	snapB = "testdata/fixture/snapshots/karta-fixture-b.osm.pbf"
	xmlB  = "testdata/fixture/karta-fixture-b.osm"
	// A tile over the fixture's lake with data in every release.
	lakeTile = "/tiles/14/8192/8191.pbf"
)

// --- operator API ------------------------------------------------------------

// publisherMetric reads one sample of the publisher's metrics (scraped with
// the monitoring token), by its exact series name and labels.
func publisherMetric(t *testing.T, series string) (float64, bool) {
	t.Helper()
	r := op(t, http.MethodGet, "/v1/operator/metrics", secret(t, "operator_monitor_token"), nil)
	expectStatus(t, r, http.StatusOK)
	for _, line := range strings.Split(string(r.body), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("%s: %v", line, err)
			}
			return f, true
		}
	}
	return 0, false
}

func secret(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, "secrets", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func operatorRouter(t *testing.T) routers.Router {
	t.Helper()
	opRouterOnce.Do(func() {
		doc, err := openapi3.NewLoader().LoadFromData(openapi.OperatorSpec)
		if err == nil {
			err = doc.Validate(context.Background())
		}
		if err != nil {
			t.Fatalf("operator.yaml: %v", err)
		}
		doc.Servers = openapi3.Servers{{URL: opBase}}
		if opRouter, err = legacy.NewRouter(doc); err != nil {
			t.Fatalf("operator router: %v", err)
		}
	})
	return opRouter
}

// op calls the operator API and validates the response against operator.yaml.
func op(t *testing.T, method, path, token string, body any) response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, opBase+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if route, params, err := operatorRouter(t).FindRoute(req); err == nil {
		in := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}},
			Status: resp.StatusCode, Header: resp.Header, Body: io.NopCloser(bytes.NewReader(b)),
			Options: &openapi3filter.Options{IncludeResponseStatus: true},
		}
		if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
			t.Errorf("%s %s: response does not match operator.yaml: %v\n%s", method, path, err, b)
		}
	}
	return response{status: resp.StatusCode, header: resp.Header, body: b}
}

type submission struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	State      string  `json:"state"`
	ReasonCode *string `json:"reason_code"`
	Reason     *string `json:"reason"`
	ReleaseID  *string `json:"release_id"`
	Attempts   int     `json:"attempts"`
	SHA256     *string `json:"sha256"`
}

func (s submission) code() string {
	if s.ReasonCode == nil {
		return ""
	}
	return *s.ReasonCode
}

func (s submission) release() string {
	if s.ReleaseID == nil {
		return ""
	}
	return *s.ReleaseID
}

type opRelease struct {
	ID               string     `json:"release_id"`
	State            string     `json:"state"`
	DataTimestamp    *time.Time `json:"data_timestamp"`
	PinnedUntil      *time.Time `json:"pinned_until"`
	DatabaseBytes    *int64     `json:"database_bytes"`
	Database         string     `json:"database"`
	RollbackEligible bool       `json:"rollback_eligible"`
	FailureReason    *string    `json:"failure_reason"`
}

type opStatus struct {
	Active      *opRelease   `json:"active"`
	Releases    []opRelease  `json:"releases"`
	Submissions []submission `json:"submissions"`
	Inbox       struct {
		LastScan struct {
			At      *time.Time `json:"at"`
			Pending []struct {
				Name, Waiting, Problem string
			} `json:"pending"`
		} `json:"last_scan"`
	} `json:"inbox"`
	Job *struct {
		Name, Phase string
	} `json:"job"`
	Storage map[string]any `json:"storage"`
}

func status(t *testing.T) opStatus {
	t.Helper()
	r := op(t, http.MethodGet, "/v1/operator/status", secret(t, "operator_monitor_token"), nil)
	expectStatus(t, r, http.StatusOK)
	var s opStatus
	r.json(t, &s)
	return s
}

func (s opStatus) release(id string) *opRelease {
	for i := range s.Releases {
		if s.Releases[i].ID == id {
			return &s.Releases[i]
		}
	}
	return nil
}

func (s opStatus) activeID() string {
	if s.Active == nil {
		return ""
	}
	return s.Active.ID
}

var terminal = map[string]bool{"published": true, "ready": true, "duplicate": true, "rejected": true, "failed": true}

// waitSubmission waits for the submission of that name to reach a final state.
func waitSubmission(t *testing.T, name string, timeout time.Duration) submission {
	return waitSubmissionPolling(t, name, timeout, false)
}

// The operator status endpoint may briefly return 503 while PostgreSQL
// reconnects after the deliberate restart. Only that scenario retries it;
// other publication tests still fail on an unexpected 503.
func waitSubmissionAfterDBRestart(t *testing.T, name string, timeout time.Duration) submission {
	return waitSubmissionPolling(t, name, timeout, true)
}

func waitSubmissionPolling(t *testing.T, name string, timeout time.Duration, retryUnavailable bool) submission {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *submission
	var lastUnavailable string
	for time.Now().Before(deadline) {
		var st opStatus
		if retryUnavailable {
			r := op(t, http.MethodGet, "/v1/operator/status", secret(t, "operator_monitor_token"), nil)
			if r.status == http.StatusServiceUnavailable {
				if code, _ := r.errorCode(t); code != "service_unavailable" {
					t.Fatalf("unexpected operator status error: %d %s", r.status, r.body)
				}
				lastUnavailable = string(r.body)
				time.Sleep(500 * time.Millisecond)
				continue
			}
			expectStatus(t, r, http.StatusOK)
			r.json(t, &st)
		} else {
			st = status(t)
		}
		for _, s := range st.Submissions {
			if s.Name == name {
				c := s
				last = &c
				break
			}
		}
		if last != nil && terminal[last.State] {
			return *last
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("submission %s did not finish within %s; last %+v; last transient status error: %s", name, timeout, last, lastUnavailable)
	return submission{}
}

// waitNewSubmission waits for a submission of that name newer than afterID
// (a re-touched marker is a new submission) to reach a final state.
func waitNewSubmission(t *testing.T, name string, afterID int64, timeout time.Duration) submission {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *submission
	for time.Now().Before(deadline) {
		for _, s := range status(t).Submissions {
			if s.Name == name && s.ID > afterID {
				c := s
				last = &c
				break
			}
		}
		if last != nil && terminal[last.State] {
			return *last
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("no new submission %s finished within %s; last %+v", name, timeout, last)
	return submission{}
}

// opTolerant sends an operator request whose connection may break because
// the publisher exits during it (fault injection).
func opTolerant(t *testing.T, method, path, token string, body any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(method, opBase+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	}
}

func waitJobPhase(t *testing.T, phase string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if j := status(t).Job; j != nil && j.Phase == phase {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("publisher did not reach phase %s within %s", phase, timeout)
}

// waitManifest waits until the API's manifest names release id.
func waitManifest(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/v1/manifest", nil)
		if resp, err := client.Do(req); err == nil {
			var m struct {
				Release struct {
					ReleaseID string `json:"release_id"`
				} `json:"release"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&m)
			resp.Body.Close()
			if got = m.Release.ReleaseID; got == id {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("manifest names %s, want %s", got, id)
}

// --- the inbox ------------------------------------------------------------------

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func writeAtomic(t *testing.T, name string, b []byte, mode os.FileMode) {
	t.Helper()
	tmp := filepath.Join(inboxDir, fmt.Sprintf(".%s.%d", name, rand.Int()))
	if err := os.WriteFile(tmp, b, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, mode); err != nil { // not subject to the umask
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(inboxDir, name)); err != nil {
		t.Fatal(err)
	}
}

// submit follows the completion protocol: snapshot, optional sidecar, then
// the ready marker holding the digest (marker overrides it when not "").
func submit(t *testing.T, name string, data, sidecar []byte, marker string) {
	t.Helper()
	writeAtomic(t, name+".osm.pbf", data, 0o644)
	if sidecar != nil {
		writeAtomic(t, name+".osm.pbf.provenance.json", sidecar, 0o644)
	}
	if marker == "" {
		marker = digestOf(data) + "  " + name + ".osm.pbf\n"
	}
	writeAtomic(t, name+".osm.pbf.ready", []byte(marker), 0o644)
}

func readRepo(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, p))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// variant is snapshot B's data with another timestamp, box or edit.
func variant(t *testing.T, ts *time.Time, bbox *[4]float64, edit func(*pbfwrite.Data)) []byte {
	t.Helper()
	d, err := pbfwrite.ParseXML(bytes.NewReader(readRepo(t, xmlB)))
	if err != nil {
		t.Fatal(err)
	}
	d.Timestamp = ts
	if bbox != nil {
		d.BBox = bbox
	}
	if edit != nil {
		edit(d)
	}
	b, err := pbfwrite.Encode(d, pbfwrite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func at(s string) *time.Time {
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &v
}

func authorize(t *testing.T, data []byte, reason string) {
	t.Helper()
	r := op(t, http.MethodPost, "/v1/operator/authorizations", secret(t, "operator_token"),
		map[string]any{"sha256": digestOf(data), "size_bytes": len(data), "reason": reason})
	if r.status != http.StatusCreated && r.status != http.StatusOK {
		t.Fatalf("authorize: %d %s", r.status, r.body)
	}
}

// --- the publisher container ------------------------------------------------------

// restartPublisher recreates the publisher with KARTA_TEST_* settings (as
// compose.test.yaml interpolates them) and waits until it serves the
// operator API. Unset settings take their test defaults.
func restartPublisher(t *testing.T, settings map[string]string) {
	t.Helper()
	var envs []string
	for k, v := range settings {
		envs = append(envs, k+"="+v)
	}
	if _, stderr, code := composeEnv(t, envs, "up", "-d", "--no-deps", "--force-recreate", "publisher"); code != 0 {
		t.Fatalf("recreate publisher: %s", stderr)
	}
	waitPublisher(t)
}

func waitPublisher(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, opBase+"/v1/operator/status", nil)
		req.Header.Set("Authorization", "Bearer "+secret(t, "operator_monitor_token"))
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	out, _, _ := compose(t, "logs", "--no-color", "--tail=30", "publisher")
	t.Fatalf("publisher not up:\n%s", out)
}

func publisherContainer(t *testing.T) string {
	t.Helper()
	out, _, code := compose(t, "ps", "-a", "-q", "publisher")
	if code != 0 || strings.TrimSpace(out) == "" {
		t.Fatal("no publisher container")
	}
	return strings.TrimSpace(out)
}

// waitPublisherExit waits for the publisher to stop and returns its exit code.
func waitPublisherExit(t *testing.T, timeout time.Duration) int {
	t.Helper()
	id := publisherContainer(t)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "inspect", "-f", "{{.State.Status}} {{.State.ExitCode}}", id).Output()
		if err == nil {
			f := strings.Fields(string(out))
			if len(f) == 2 && f[0] == "exited" {
				code, _ := strconv.Atoi(f[1])
				return code
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("publisher did not exit within %s", timeout)
	return -1
}

// resetAll deletes every release and the registry, empties the inbox and
// restarts the publisher, which migrates a fresh registry.
func resetAll(t *testing.T) {
	t.Helper()
	compose(t, "stop", "publisher")
	ctx := context.Background()
	pg := superuser(t, "postgres")
	defer pg.Close(ctx)
	rows, err := pg.Query(ctx, `SELECT datname FROM pg_database WHERE datname ~ '^karta_(r|c)[0-9a-f]{24}$'`)
	if err != nil {
		t.Fatal(err)
	}
	dbs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range dbs {
		if _, err := pg.Exec(ctx, "DROP DATABASE "+db+" WITH (FORCE)"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pg.Exec(ctx, `DROP TABLESPACE IF EXISTS karta_tight`); err != nil {
		t.Fatal(err)
	}
	compose(t, "exec", "-T", "db", "rm", "-f", "/var/lib/postgresql/tight/filler")
	reg := superuser(t, "karta_registry")
	if _, err := reg.Exec(ctx, `DROP SCHEMA IF EXISTS registry CASCADE`); err != nil {
		t.Fatal(err)
	}
	reg.Close(ctx)
	entries, _ := os.ReadDir(inboxDir)
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(inboxDir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	restartPublisher(t, nil)
	waitReady(t, false, "no_active_release")
}

func databases(t *testing.T) map[string]bool {
	t.Helper()
	ctx := context.Background()
	pg := superuser(t, "postgres")
	defer pg.Close(ctx)
	rows, err := pg.Query(ctx, `SELECT datname FROM pg_database WHERE datname ~ '^karta_(r|c)[0-9a-f]{24}$'`)
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out
}

// waitCandidate waits until a build has created its candidate database.
func waitCandidate(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for candidates(t) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no candidate database appeared")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func candidates(t *testing.T) int {
	t.Helper()
	n := 0
	for db := range databases(t) {
		if strings.HasPrefix(db, "karta_c") {
			n++
		}
	}
	return n
}

// --- continuous load --------------------------------------------------------------

// load keeps requesting the manifest, unpinned and pinned search, tiles and
// style redirects, like clients do, and records every failure and latency.
type load struct {
	stop     chan struct{}
	wg       sync.WaitGroup
	mu       sync.Mutex
	requests int
	failures []string
	latency  []time.Duration
	releases map[string]bool
}

func startLoad(workers int) *load {
	l := &load{stop: make(chan struct{}), releases: map[string]bool{}}
	hc := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string, ok ...int) []byte {
		start := time.Now()
		resp, err := hc.Get(base + path)
		d := time.Since(start)
		l.mu.Lock()
		defer l.mu.Unlock()
		l.requests++
		l.latency = append(l.latency, d)
		if err != nil {
			l.failures = append(l.failures, path+": "+err.Error())
			return nil
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		for _, c := range ok {
			if resp.StatusCode == c {
				return b
			}
		}
		l.failures = append(l.failures, fmt.Sprintf("%s: %d %s", path, resp.StatusCode, b))
		return nil
	}
	for i := 0; i < workers; i++ {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			for {
				select {
				case <-l.stop:
					return
				default:
				}
				var m struct {
					Release struct {
						ReleaseID string `json:"release_id"`
					} `json:"release"`
				}
				b := get("/v1/manifest", 200)
				if b == nil || json.Unmarshal(b, &m) != nil || m.Release.ReleaseID == "" {
					continue
				}
				rel := m.Release.ReleaseID
				l.mu.Lock()
				l.releases[rel] = true
				l.mu.Unlock()
				get("/v1/search?"+url.Values{"q": {"دریاچه آزمون"}}.Encode(), 200)
				get("/v1/releases/"+rel+lakeTile, 200, 204)
				get("/v1/releases/"+rel+"/style.json", 307)
				// Pinned to the release the manifest just named: it must stay
				// consistent even if a switch happens meanwhile.
				if b := get("/v1/search?"+url.Values{"q": {"Test Lake"}, "release_id": {rel}}.Encode(), 200); b != nil {
					var s struct {
						ReleaseID string `json:"release_id"`
					}
					if json.Unmarshal(b, &s) != nil || s.ReleaseID != rel {
						l.mu.Lock()
						l.failures = append(l.failures, "pinned search answered from "+s.ReleaseID+", pinned "+rel)
						l.mu.Unlock()
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}
	return l
}

type loadSummary struct {
	Requests int      `json:"requests"`
	Failures int      `json:"failures"`
	P50ms    float64  `json:"p50_ms"`
	P95ms    float64  `json:"p95_ms"`
	P99ms    float64  `json:"p99_ms"`
	MaxMs    float64  `json:"max_ms"`
	Releases []string `json:"releases_seen"`
}

func (l *load) finish(t *testing.T, label string) loadSummary {
	t.Helper()
	close(l.stop)
	l.wg.Wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	lat := append([]time.Duration(nil), l.latency...)
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) float64 {
		if len(lat) == 0 {
			return 0
		}
		return float64(lat[int(p*float64(len(lat)-1))].Microseconds()) / 1000
	}
	s := loadSummary{Requests: l.requests, Failures: len(l.failures), P50ms: pct(0.5), P95ms: pct(0.95), P99ms: pct(0.99), MaxMs: pct(1)}
	for r := range l.releases {
		s.Releases = append(s.Releases, r)
	}
	sort.Strings(s.Releases)
	t.Logf("load %s: %+v", label, s)
	for i, f := range l.failures {
		if i == 20 {
			t.Errorf("... %d more failures", len(l.failures)-20)
			break
		}
		t.Errorf("load %s failure: %s", label, f)
	}
	if s.Requests == 0 {
		t.Errorf("load %s made no requests", label)
	}
	return s
}

func recordMeasurement(t *testing.T, name string, v any) {
	t.Helper()
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Log(err)
		return
	}
	p := filepath.Join(artifacts, "publication-measurements.json")
	all := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &all)
	}
	all[name] = v
	b, _ := json.MarshalIndent(all, "", "  ")
	_ = os.WriteFile(p, b, 0o644)
}

// --- the test ----------------------------------------------------------------------

func TestPublication(t *testing.T) {
	if inboxDir == "" {
		t.Fatal("KARTA_INBOX_HOST_DIR is not set; run `make test-integration`")
	}
	admin, monitor := secret(t, "operator_token"), secret(t, "operator_monitor_token")
	resetAll(t)
	t.Cleanup(func() { resetAll(t) })
	dataA, dataB := readRepo(t, snapA), readRepo(t, snapB)
	var relA, relB string

	t.Run("operator API is separate, authenticated and scoped", func(t *testing.T) {
		expectError(t, get(t, "/v1/operator/status"), http.StatusNotFound, "not_found", "")
		if r := op(t, http.MethodGet, "/v1/operator/status", "", nil); r.status != http.StatusUnauthorized {
			t.Errorf("no token: %d", r.status)
		}
		if r := op(t, http.MethodGet, "/v1/operator/status", strings.Repeat("0", 64), nil); r.status != http.StatusUnauthorized {
			t.Errorf("wrong token: %d", r.status)
		}
		for _, p := range []string{"/v1/operator/rollback", "/v1/operator/cleanup"} {
			if r := op(t, http.MethodPost, p, monitor, map[string]any{"reason": "x"}); r.status != http.StatusForbidden {
				t.Errorf("monitor %s: %d", p, r.status)
			}
		}
		if st := status(t); st.Active != nil || len(st.Releases) != 0 {
			t.Fatalf("not empty after reset: %+v", st)
		}
	})

	t.Run("an incomplete submission is never read", func(t *testing.T) {
		// The snapshot is unreadable for the publisher (UID 10001) and has
		// no ready marker: any attempt to open it would be recorded as a
		// rejection. The publisher must only list it as waiting.
		p := filepath.Join(inboxDir, "a.osm.pbf")
		if err := os.WriteFile(p, dataA, 0o000); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o000); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(20 * time.Second)
		var st opStatus
		for time.Now().Before(deadline) {
			st = status(t)
			if len(st.Inbox.LastScan.Pending) == 1 && st.Inbox.LastScan.Pending[0].Waiting == "waiting_for_ready_marker" {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		time.Sleep(3 * time.Second) // several more scans
		st = status(t)
		if len(st.Inbox.LastScan.Pending) != 1 || st.Inbox.LastScan.Pending[0].Name != "a" || len(st.Submissions) != 0 {
			t.Fatalf("pending %+v submissions %+v", st.Inbox.LastScan.Pending, st.Submissions)
		}
		waitReady(t, false, "no_active_release")
		// Completion: readable, then the marker.
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
		writeAtomic(t, "a.osm.pbf.ready", []byte(digestOf(dataA)+"\n"), 0o644)
		s := waitSubmission(t, "a", 60*time.Second)
		if s.State != "published" || s.release() == "" {
			t.Fatalf("submission %+v", s)
		}
		relA = s.release()
		waitReady(t, true, "ready")
		waitManifest(t, relA)
	})
	if relA == "" {
		t.Fatal("snapshot A was not published; stopping")
	}

	// Clients keep using the API for the rest of the test (until the
	// deliberate outages at the end): not a single request may fail.
	traffic := startLoad(4)

	t.Run("serving continues through a slow import and the switch; pinned clients keep their release", func(t *testing.T) {
		restartPublisher(t, map[string]string{"KARTA_TEST_OSM2PGSQL": "/usr/local/bin/slow-osm2pgsql"})
		pinned := startPinned(relA)
		importLoad := startLoad(2)
		submit(t, "b", dataB, nil, "")
		waitJobPhase(t, "building", 30*time.Second)
		waitCandidate(t)
		// While the candidate imports, the API serves A and nothing else.
		if st := status(t); st.activeID() != relA || candidates(t) != 1 {
			t.Fatalf("during import: active %s, candidates %d", st.activeID(), candidates(t))
		}
		expectStatus(t, get(t, "/v1/releases/"+relA+lakeTile), http.StatusOK)
		s := waitSubmission(t, "b", 90*time.Second)
		if s.State != "published" {
			t.Fatalf("submission %+v", s)
		}
		relB = s.release()
		waitManifest(t, relB)
		time.Sleep(2 * time.Second)
		sum := importLoad.finish(t, "slow import and switch")
		recordMeasurement(t, "load_during_slow_import_and_switch", sum)
		if len(sum.Releases) != 2 {
			t.Errorf("load saw releases %v, want A then B", sum.Releases)
		}
		pinned.finish(t)
		st := status(t)
		if a := st.release(relA); a == nil || a.State != "retired" || a.PinnedUntil == nil {
			t.Errorf("A after the switch: %+v", a)
		}
		if candidates(t) != 0 {
			t.Error("candidate database left after publication")
		}
		// B's content differs from A's in both directions.
		sb, _ := search(t, url.Values{"q": {"Second Edition Cafe"}, "release_id": {relB}})
		sa, _ := search(t, url.Values{"q": {"Second Edition Cafe"}, "release_id": {relA}})
		if len(sb.Results) == 0 || sb.Results[0].ID != "node/520" || len(sa.Results) != 0 {
			t.Errorf("B %v, A %v", ids(sb), ids(sa))
		}
		restartPublisher(t, nil)
	})
	if relB == "" {
		t.Fatal("snapshot B was not published; stopping")
	}

	t.Run("duplicate and older submissions change nothing", func(t *testing.T) {
		// A duplicate builds a candidate database and drops it at once; the
		// operator status sizes every database meanwhile and must not fail
		// on one that disappears (a race these tests once caught).
		stopPoll := make(chan struct{})
		polled, pollErrs := 0, []string{}
		var pollWG sync.WaitGroup
		pollWG.Add(1)
		go func() {
			defer pollWG.Done()
			for {
				select {
				case <-stopPoll:
					return
				default:
				}
				req, _ := http.NewRequest(http.MethodGet, opBase+"/v1/operator/status", nil)
				req.Header.Set("Authorization", "Bearer "+monitor)
				resp, err := client.Do(req)
				polled++
				if err != nil {
					pollErrs = append(pollErrs, err.Error())
					continue
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					pollErrs = append(pollErrs, fmt.Sprintf("%d %s", resp.StatusCode, b))
				}
			}
		}()
		defer func() {
			close(stopPoll)
			pollWG.Wait()
			if len(pollErrs) > 0 {
				t.Errorf("%d of %d status requests failed during duplicate submissions: %v", len(pollErrs), polled, pollErrs[0])
			}
		}()
		for i := 0; i < 5; i++ {
			submit(t, fmt.Sprintf("b-dup-%d", i), dataB, nil, "")
		}
		submit(t, "b-again", dataB, nil, "")
		submit(t, "a-again", dataA, nil, "")
		for _, name := range []string{"b-dup-0", "b-dup-1", "b-dup-2", "b-dup-3", "b-dup-4", "b-again"} {
			if s := waitSubmission(t, name, 60*time.Second); s.State != "duplicate" || s.code() != "duplicate_active" || s.release() != relB {
				t.Errorf("duplicate %s: %+v", name, s)
			}
		}
		if s := waitSubmission(t, "a-again", 60*time.Second); s.State != "rejected" || s.code() != "older_than_active" {
			t.Errorf("older: %+v", s)
		}
		// The same files again (same fingerprint) are not a new submission.
		before := len(status(t).Submissions)
		time.Sleep(3 * time.Second)
		st := status(t)
		if len(st.Submissions) != before || st.activeID() != relB || len(st.Releases) != 2 {
			t.Errorf("after duplicates: %d submissions (was %d), active %s, %d releases", len(st.Submissions), before, st.activeID(), len(st.Releases))
		}
	})

	var relC string
	t.Run("invalid, incomplete, changed and unauthorized inputs are rejected", func(t *testing.T) {
		otherBox := variant(t, at("2026-03-01T00:00:00Z"), &[4]float64{0, 0, 0.03, 0.02}, nil)
		future := variant(t, at("2099-01-01T00:00:00Z"), nil, nil)
		noTimestamp := variant(t, nil, nil, nil)
		newer := variant(t, at("2026-03-01T00:00:00Z"), nil, nil)
		badSidecar, _ := json.Marshal(map[string]any{"output_sha256": strings.Repeat("0", 64), "bbox_wgs84": "0,0,0.02,0.015",
			"output_fileinfo": map[string]any{"file": map[string]any{"size": len(dataB)}}, "license": "ODbL 1.0",
			"source_fileinfo": map[string]any{"header": map[string]any{"option": map[string]any{"timestamp": "2026-02-01T00:00:00Z"}}}})
		// The last whole blob removed: a well-formed, shorter PBF.
		boundary := dataB[:blobBoundary(t, dataB)]

		submit(t, "truncated", dataB[:len(dataB)-500], nil, "")
		submit(t, "boundary-cut", boundary, nil, "")
		submit(t, "garbage", []byte("this is not an OSM PBF file at all"), nil, "")
		submit(t, "wrong-marker", dataA, nil, digestOf(dataB)+"\n")
		submit(t, "bad-marker", dataB, nil, "done\n")
		submit(t, "other-region", otherBox, nil, "")
		submit(t, "future", future, nil, "")
		submit(t, "no-timestamp", noTimestamp, nil, "")
		submit(t, "bad-provenance", dataB, badSidecar, "")
		submit(t, "unauthorized", newer, nil, "")
		// Changed after its marker was written: the marker describes other bytes.
		submit(t, "changed", dataB, nil, "")
		writeAtomic(t, "changed.osm.pbf", dataA, 0o644)
		// A symlink to a valid snapshot, a symlinked marker, a directory and a FIFO.
		target := filepath.Join(t.TempDir(), "real.osm.pbf")
		if err := os.WriteFile(target, dataB, 0o644); err != nil {
			t.Fatal(err)
		}
		must(t, os.Symlink(target, filepath.Join(inboxDir, "symlink.osm.pbf")))
		writeAtomic(t, "symlink.osm.pbf.ready", []byte(digestOf(dataB)), 0o644)
		writeAtomic(t, "marker-link.osm.pbf", dataB, 0o644)
		markerTarget := filepath.Join(t.TempDir(), "marker")
		must(t, os.WriteFile(markerTarget, []byte(digestOf(dataB)), 0o644))
		must(t, os.Symlink(markerTarget, filepath.Join(inboxDir, "marker-link.osm.pbf.ready")))
		must(t, os.Mkdir(filepath.Join(inboxDir, "directory.osm.pbf"), 0o755))
		writeAtomic(t, "directory.osm.pbf.ready", []byte(digestOf(dataB)), 0o644)
		must(t, syscall.Mkfifo(filepath.Join(inboxDir, "fifo.osm.pbf"), 0o644))
		writeAtomic(t, "fifo.osm.pbf.ready", []byte(digestOf(dataB)), 0o644)

		want := map[string]string{
			"truncated": "malformed_snapshot", "boundary-cut": "unauthorized_digest", "garbage": "malformed_snapshot",
			"wrong-marker": "marker_digest_mismatch", "bad-marker": "invalid_marker", "other-region": "region_mismatch",
			"future": "timestamp_untrusted", "no-timestamp": "timestamp_missing", "bad-provenance": "provenance_invalid",
			"unauthorized": "unauthorized_digest", "changed": "marker_digest_mismatch", "symlink": "symlink",
			"marker-link": "symlink", "directory": "not_regular_file", "fifo": "not_regular_file",
		}
		for name, code := range want {
			s := waitSubmission(t, name, 60*time.Second)
			if s.State != "rejected" || s.code() != code {
				t.Errorf("%s: %s %s (%s), want rejected %s", name, s.State, s.code(), strOr(s.Reason), code)
			}
		}
		st := status(t)
		if st.activeID() != relB || len(st.Releases) != 2 || candidates(t) != 0 {
			t.Fatalf("after rejections: active %s, %d releases, %d candidates", st.activeID(), len(st.Releases), candidates(t))
		}
		waitReady(t, true, "ready")

		// KartaPublicationFailed's input: the authorized rejection below
		// leaves "rejected" while a new one enters it, so the rejected count
		// stays the same; the latest finish time still moves to the new one.
		const rejectedCount = `karta_submissions{source="inbox",state="rejected"}`
		const lastRejected = `karta_submission_last_finished_timestamp_seconds{source="inbox",state="rejected"}`
		countBefore, okCount := publisherMetric(t, rejectedCount)
		lastBefore, okLast := publisherMetric(t, lastRejected)
		if !okCount || !okLast || countBefore < float64(len(want)) {
			t.Fatalf("before the retry: %s %v (%v), %s %v (%v)", rejectedCount, countBefore, okCount, lastRejected, lastBefore, okLast)
		}

		// Authorizing exactly the unauthorized digest publishes the same
		// submission on the next scan; nothing else was authorized.
		authorize(t, newer, "integration test: verified variant C")
		s := waitSubmission(t, "unauthorized", 5*time.Second) // still the rejection
		deadline := time.Now().Add(60 * time.Second)
		for s.State != "published" && time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
			s = waitSubmission(t, "unauthorized", 60*time.Second)
		}
		if s.State != "published" || s.Attempts != 2 {
			t.Fatalf("after authorization: %+v", s)
		}
		relC = s.release()
		waitManifest(t, relC)
		if s := waitSubmission(t, "boundary-cut", time.Second); s.State != "rejected" {
			t.Errorf("an unrelated rejection was re-evaluated: %+v", s)
		}

		newRejection := time.Now()
		submit(t, "rejected-after-retry", []byte("not an OSM PBF file either"), nil, "")
		if s := waitSubmission(t, "rejected-after-retry", 60*time.Second); s.State != "rejected" || s.code() != "malformed_snapshot" {
			t.Fatalf("the new rejection: %+v", s)
		}
		countAfter, _ := publisherMetric(t, rejectedCount)
		lastAfter, ok := publisherMetric(t, lastRejected)
		if countAfter != countBefore {
			t.Errorf("%s %v, was %v: one rejection left the state and one entered it", rejectedCount, countAfter, countBefore)
		}
		if !ok || lastAfter <= lastBefore || lastAfter < float64(newRejection.Unix())-1 {
			t.Errorf("%s %v (was %v): not the new rejection, which finished after %d", lastRejected, lastAfter, lastBefore, newRejection.Unix())
		}
	})
	if relC == "" {
		t.Fatal("variant C was not published; stopping")
	}

	var relD string
	t.Run("concurrent operator actions and a rollback during an import", func(t *testing.T) {
		// Two identical rollbacks with the same compare-and-swap guard:
		// exactly one switches, the other is told the active release changed.
		var wg sync.WaitGroup
		results := make([]response, 2)
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = op(t, http.MethodPost, "/v1/operator/rollback", admin,
					map[string]any{"release_id": relB, "expected_active_release_id": relC, "reason": fmt.Sprintf("concurrent rollback %d", i)})
			}()
		}
		wg.Wait()
		statuses := []int{results[0].status, results[1].status}
		sort.Ints(statuses)
		if statuses[0] != 200 || statuses[1] != 409 {
			t.Fatalf("concurrent rollbacks: %d %s / %d %s", results[0].status, results[0].body, results[1].status, results[1].body)
		}
		for _, r := range results {
			if r.status == 409 {
				if c, _ := r.errorCode(t); c != "active_release_changed" {
					t.Errorf("loser: %s", r.body)
				}
			}
		}
		waitManifest(t, relB)

		// A rollback is not blocked by a running import, and the import that
		// finishes afterwards stays ready instead of undoing the rollback.
		restartPublisher(t, map[string]string{"KARTA_TEST_OSM2PGSQL": "/usr/local/bin/slow-osm2pgsql"})
		d := variant(t, at("2026-03-15T00:00:00Z"), nil, nil)
		authorize(t, d, "integration test: variant D")
		submit(t, "d", d, nil, "")
		waitJobPhase(t, "building", 30*time.Second)
		start := time.Now()
		r := op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"release_id": relA, "reason": "rollback during an import"})
		if r.status != 200 || time.Since(start) > 5*time.Second {
			t.Fatalf("rollback during import: %d in %s: %s", r.status, time.Since(start), r.body)
		}
		waitManifest(t, relA)
		s := waitSubmission(t, "d", 90*time.Second)
		if s.State != "ready" || s.code() != "active_changed" {
			t.Fatalf("import after rollback: %+v", s)
		}
		relD = s.release()
		if st := status(t); st.activeID() != relA {
			t.Fatalf("active %s after the import finished, want %s", st.activeID(), relA)
		}
		restartPublisher(t, nil)
		// Activating D is an explicit, forward operator action.
		r = op(t, http.MethodPost, "/v1/operator/releases/"+relD+"/activate", admin, map[string]any{"reason": "D verified"})
		if r.status != 200 {
			t.Fatalf("activate D: %d %s", r.status, r.body)
		}
		waitManifest(t, relD)
		// Forward only: activating older B is refused (that is a rollback).
		r = op(t, http.MethodPost, "/v1/operator/releases/"+relB+"/activate", admin, map[string]any{"reason": "try going back"})
		if c, _ := r.errorCode(t); r.status != 409 || c != "policy_refused" || !strings.Contains(string(r.body), "older_than_active") {
			t.Fatalf("activate older: %d %s", r.status, r.body)
		}
		// Unknown and failed releases cannot be rolled back to.
		r = op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"release_id": "r000000000000000000000000", "reason": "x"})
		if c, _ := r.errorCode(t); r.status != 404 || c != "unknown_release" {
			t.Errorf("unknown target: %d %s", r.status, r.body)
		}
	})
	if relD == "" {
		t.Fatal("variant D was not built; stopping")
	}

	t.Run("a pinned release is served until its grace ends, then 410", func(t *testing.T) {
		// relC was replaced (by the rollback to B) more than 20 s ago at the
		// earliest; wait for its pin to end if needed.
		st := status(t)
		c := st.release(relC)
		if c == nil || c.State != "retired" || c.PinnedUntil == nil {
			t.Fatalf("C: %+v", c)
		}
		if w := time.Until(*c.PinnedUntil); w > 0 {
			expectStatus(t, get(t, "/v1/releases/"+relC+lakeTile), http.StatusOK)
			time.Sleep(w + 2*time.Second)
		}
		expectError(t, get(t, "/v1/releases/"+relC+lakeTile), http.StatusGone, "release_expired", "release_id")
		expectError(t, get(t, "/v1/search?q=lake&release_id="+relC), http.StatusGone, "release_expired", "release_id")
		expectError(t, get(t, "/v1/releases/"+relC+"/style.json"), http.StatusGone, "release_expired", "release_id")
		// Expired pins are still retained: a rollback serves C again.
		r := op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"release_id": relC, "reason": "rollback to an expired pin"})
		if r.status != 200 {
			t.Fatalf("rollback to C: %d %s", r.status, r.body)
		}
		waitManifest(t, relC)
		expectStatus(t, get(t, "/v1/releases/"+relC+lakeTile), http.StatusOK)
		r = op(t, http.MethodPost, "/v1/operator/releases/"+relD+"/activate", admin, map[string]any{"reason": "back to D"})
		if r.status != 200 {
			t.Fatalf("activate D: %d %s", r.status, r.body)
		}
		waitManifest(t, relD)
	})

	active := relD
	t.Run("crash at every critical transition is recovered", func(t *testing.T) {
		points := []string{"stage.after_copy", "build.after_create", "build.after_import", "build.after_rename",
			"activate.before_commit", "activate.after_commit"}
		for i, fp := range points {
			t.Run(fp, func(t *testing.T) {
				data := variant(t, at(fmt.Sprintf("2026-04-%02dT00:00:00Z", i+1)), nil, nil)
				authorize(t, data, "integration test: crash "+fp)
				restartPublisher(t, map[string]string{"KARTA_TEST_FAILPOINTS": fp})
				name := "crash-" + strings.ReplaceAll(fp, ".", "-")
				submit(t, name, data, nil, "")
				if code := waitPublisherExit(t, 60*time.Second); code != 99 {
					t.Fatalf("publisher exit %d, want the failpoint's 99", code)
				}
				// The process is gone; serving is not affected.
				if fp == "activate.after_commit" {
					// The switch committed before the crash.
					if st := activeFromRegistry(t); st == active {
						t.Fatal("the committed switch is not visible")
					}
				} else {
					waitReady(t, true, "ready")
					if got := activeFromRegistry(t); got != active {
						t.Fatalf("active %s after a crash at %s, want %s", got, fp, active)
					}
					expectStatus(t, get(t, "/v1/releases/"+active+lakeTile), http.StatusOK)
				}
				restartPublisher(t, nil)
				s := waitSubmission(t, name, 90*time.Second)
				want := "published"
				if fp == "activate.after_commit" {
					want = "duplicate" // its release is already active
				}
				if s.State != want || s.Attempts != 2 {
					t.Fatalf("after restart: %+v, want %s on attempt 2", s, want)
				}
				waitManifest(t, s.release())
				active = s.release()
				if candidates(t) != 0 {
					t.Error("candidate database left after recovery")
				}
				assertRegistryConsistent(t)
			})
		}

		// An operator rollback uses the same pointer transaction: a crash just
		// before its commit changes nothing, just after it the rollback holds.
		for _, fp := range []string{"activate.before_commit", "activate.after_commit"} {
			t.Run("rollback interrupted at "+fp, func(t *testing.T) {
				st := status(t)
				var target string
				for _, r := range st.Releases {
					if r.State == "retired" && r.RollbackEligible {
						target = r.ID
						break
					}
				}
				if target == "" {
					t.Fatal("no rollback target")
				}
				before := activeFromRegistry(t)
				restartPublisher(t, map[string]string{"KARTA_TEST_FAILPOINTS": fp})
				opTolerant(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"release_id": target, "reason": "rollback interrupted at " + fp})
				if code := waitPublisherExit(t, 30*time.Second); code != 99 {
					t.Fatalf("exit %d", code)
				}
				got := activeFromRegistry(t)
				switch fp {
				case "activate.before_commit":
					if got != before {
						t.Fatalf("active %s after a crash before the commit, want %s", got, before)
					}
				default:
					if got != target {
						t.Fatalf("active %s after a crash after the commit, want %s", got, target)
					}
				}
				restartPublisher(t, nil)
				waitManifest(t, got)
				assertRegistryConsistent(t)
				// Back to the release that was active, explicitly.
				if got != before {
					r := op(t, http.MethodPost, "/v1/operator/releases/"+before+"/activate", admin, map[string]any{"reason": "undo the test rollback"})
					if r.status != 200 {
						t.Fatalf("activate %s: %d %s", before, r.status, r.body)
					}
					waitManifest(t, before)
				}
			})
		}

		t.Run("kill -9 during osm2pgsql", func(t *testing.T) {
			data := variant(t, at("2026-04-20T00:00:00Z"), nil, nil)
			authorize(t, data, "integration test: kill during import")
			restartPublisher(t, map[string]string{"KARTA_TEST_OSM2PGSQL": "/usr/local/bin/slow-osm2pgsql"})
			submit(t, "killed", data, nil, "")
			waitJobPhase(t, "building", 30*time.Second)
			waitCandidate(t)
			if out, err := exec.Command("docker", "kill", "-s", "KILL", publisherContainer(t)).CombinedOutput(); err != nil {
				t.Fatalf("kill: %v %s", err, out)
			}
			waitPublisherExit(t, 30*time.Second)
			if candidates(t) != 1 {
				t.Errorf("expected the interrupted candidate database, found %d", candidates(t))
			}
			waitReady(t, true, "ready")
			if got := activeFromRegistry(t); got != active {
				t.Fatalf("active %s after the kill, want %s", got, active)
			}
			restartPublisher(t, nil)
			s := waitSubmission(t, "killed", 90*time.Second)
			if s.State != "published" || s.Attempts != 2 {
				t.Fatalf("after restart: %+v", s)
			}
			active = s.release()
			waitManifest(t, active)
			if candidates(t) != 0 {
				t.Error("candidate database left after recovery")
			}
			assertRegistryConsistent(t)
		})
	})

	t.Run("cleanup keeps the active, retained and pinned releases and removes the rest", func(t *testing.T) {
		restartPublisher(t, map[string]string{"KARTA_TEST_RETAIN": "1", "KARTA_TEST_PIN_GRACE": "5s"})
		// A dry run removes nothing.
		r := op(t, http.MethodPost, "/v1/operator/cleanup", admin, map[string]any{"reason": "dry run", "dry_run": true})
		expectStatus(t, r, http.StatusOK)
		dbsBefore := databases(t)
		var dry struct {
			Decisions []struct {
				ReleaseID string `json:"release_id"`
				Action    string `json:"action"`
				Why       string `json:"why"`
			} `json:"decisions"`
			Removed []string `json:"removed"`
		}
		r.json(t, &dry)
		if len(dry.Removed) != 0 || len(databases(t)) != len(dbsBefore) {
			t.Fatal("dry run removed releases")
		}
		// It is audited as what it did: nothing (noop), listing what a real
		// cleanup would remove, with no error.
		var wouldRemove []any
		for _, d := range dry.Decisions {
			if d.Action == "remove" {
				wouldRemove = append(wouldRemove, d.ReleaseID)
			}
		}
		if e := lastAudit(t, "cleanup", "dry run"); e.Outcome != "noop" || e.Detail["dry_run"] != true || e.Detail["error"] != nil ||
			fmt.Sprint(e.Detail["would_remove"]) != fmt.Sprint(wouldRemove) || fmt.Sprint(e.Detail["removed"]) != "[]" {
			t.Errorf("dry run audit record %+v, want noop would_remove %v", e, wouldRemove)
		}
		// A session on the oldest retired release blocks its removal.
		st := status(t)
		var retired []opRelease
		for _, rel := range st.Releases {
			if rel.State == "retired" {
				retired = append(retired, rel)
			}
		}
		if len(retired) < 3 {
			t.Fatalf("want at least 3 retired releases, have %d", len(retired))
		}
		inUse := retired[len(retired)-1]
		held := superuser(t, inUse.Database)
		// Wait for every pin (5 s grace here, 20 s before) plus the 30 s
		// drain margin to end.
		time.Sleep(55 * time.Second)
		r = op(t, http.MethodPost, "/v1/operator/cleanup", admin, map[string]any{"reason": "integration cleanup"})
		expectStatus(t, r, http.StatusOK)
		var res struct {
			Removed []string `json:"removed"`
			Skipped []struct {
				ReleaseID string `json:"release_id"`
				Why       string `json:"why"`
			} `json:"skipped"`
		}
		r.json(t, &res)
		held.Close(context.Background())
		if e := lastAudit(t, "cleanup", "integration cleanup"); e.Outcome != "succeeded" || e.Detail["error"] != nil ||
			fmt.Sprint(e.Detail["removed"]) != fmt.Sprint(res.Removed) {
			t.Errorf("cleanup audit record %+v, removed %v", e, res.Removed)
		}
		after := status(t)
		if after.activeID() != active {
			t.Fatalf("cleanup changed the active release to %s", after.activeID())
		}
		kept := 0
		for _, rel := range after.Releases {
			switch rel.State {
			case "ready", "retired":
				kept++
			}
		}
		// The in-use release was skipped; one other is the retained rollback target.
		if kept != 2 || len(res.Removed) < 2 {
			t.Fatalf("after cleanup: %d kept, removed %v, skipped %+v", kept, res.Removed, res.Skipped)
		}
		skipped := false
		for _, s := range res.Skipped {
			skipped = skipped || (s.ReleaseID == inUse.ID && strings.Contains(s.Why, "session"))
		}
		if !skipped {
			t.Errorf("the release in use was not skipped: %+v", res.Skipped)
		}
		dbs := databases(t)
		for _, id := range res.Removed {
			if dbs["karta_"+id] {
				t.Errorf("removed release %s still has a database", id)
			}
			if rel := after.release(id); rel == nil || rel.State != "removed" {
				t.Errorf("removed release %s: %+v", id, rel)
			}
			expectError(t, get(t, "/v1/releases/"+id+lakeTile), http.StatusGone, "release_expired", "release_id")
			r := op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"release_id": id, "reason": "roll back to a removed release"})
			if c, _ := r.errorCode(t); r.status != 409 || c != "release_not_eligible" {
				t.Errorf("rollback to removed %s: %d %s", id, r.status, r.body)
			}
		}
		// Released, the session no longer blocks: the next cleanup removes it.
		r = op(t, http.MethodPost, "/v1/operator/cleanup", admin, map[string]any{"reason": "after the session ended"})
		r.json(t, &res)
		if len(res.Removed) != 1 || res.Removed[0] != inUse.ID {
			t.Errorf("second cleanup removed %v, want %s", res.Removed, inUse.ID)
		}
		assertRegistryConsistent(t)
	})

	t.Run("an interrupted removal is finished at restart", func(t *testing.T) {
		for i, fp := range []string{"cleanup.after_mark", "cleanup.after_drop"} {
			// Publish a newer release so the previous one becomes removable.
			data := variant(t, at(fmt.Sprintf("2026-05-%02dT00:00:00Z", i+1)), nil, nil)
			authorize(t, data, "integration test: "+fp)
			restartPublisher(t, map[string]string{"KARTA_TEST_RETAIN": "0", "KARTA_TEST_PIN_GRACE": "5s"})
			prev := active
			submit(t, "removal-"+strconv.Itoa(i), data, nil, "")
			s := waitSubmission(t, "removal-"+strconv.Itoa(i), 60*time.Second)
			if s.State != "published" {
				t.Fatalf("%+v", s)
			}
			active = s.release()
			waitManifest(t, active)
			time.Sleep(40 * time.Second) // grace + drain margin
			restartPublisher(t, map[string]string{"KARTA_TEST_RETAIN": "0", "KARTA_TEST_PIN_GRACE": "5s", "KARTA_TEST_FAILPOINTS": fp})
			opTolerant(t, http.MethodPost, "/v1/operator/cleanup", admin, map[string]any{"reason": "crash during removal"})
			if code := waitPublisherExit(t, 30*time.Second); code != 99 {
				t.Fatalf("exit %d", code)
			}
			if got := activeFromRegistry(t); got != active {
				t.Fatalf("active %s, want %s", got, active)
			}
			restartPublisher(t, map[string]string{"KARTA_TEST_RETAIN": "0", "KARTA_TEST_PIN_GRACE": "5s"})
			st := status(t)
			if rel := st.release(prev); rel == nil || rel.State != "removed" || databases(t)["karta_"+prev] {
				t.Fatalf("%s: interrupted removal of %s not finished: %+v", fp, prev, rel)
			}
			assertRegistryConsistent(t)
		}
		restartPublisher(t, nil)
	})

	t.Run("a cleanup that fails after it started is audited as failed, with what it removed", func(t *testing.T) {
		t.Cleanup(func() { restartPublisher(t, nil) })
		restartPublisher(t, map[string]string{"KARTA_TEST_RETAIN": "0", "KARTA_TEST_PIN_GRACE": "5s"})
		// Two releases published back to back retire the active one and the
		// first of them; after the pin grace and drain margin both are
		// removable, the more recently replaced first.
		older := active
		var names []string
		for i := range 2 {
			name := fmt.Sprintf("cleanup-failure-%d", i)
			data := variant(t, at(fmt.Sprintf("2026-05-%02dT00:00:00Z", 10+i)), nil, nil)
			authorize(t, data, "integration test: "+name)
			submit(t, name, data, nil, "")
			names = append(names, name)
		}
		var newer string
		for i, name := range names {
			s := waitSubmission(t, name, 90*time.Second)
			if s.State != "published" {
				t.Fatalf("%s: %+v", name, s)
			}
			if i == 0 {
				newer = s.release()
			}
			active = s.release()
		}
		waitManifest(t, active)
		time.Sleep(40 * time.Second) // grace + drain margin

		// Hold the registry row of the release cleanup removes second:
		// marking it removing waits for the lock and times out (5 s) after
		// the first release was already removed.
		ctx := context.Background()
		reg := superuser(t, "karta_registry")
		defer reg.Close(ctx)
		lock, err := reg.Begin(ctx)
		must(t, err)
		if _, err := lock.Exec(ctx, `SELECT 1 FROM registry.releases WHERE release_id = $1 FOR UPDATE`, older); err != nil {
			t.Fatal(err)
		}
		r := op(t, http.MethodPost, "/v1/operator/cleanup", admin, map[string]any{"reason": "cleanup failing after it started"})
		must(t, lock.Rollback(ctx))
		if c, _ := r.errorCode(t); r.status != http.StatusServiceUnavailable || c != "service_unavailable" {
			t.Fatalf("cleanup with a locked release: %d %s", r.status, r.body)
		}
		e := lastAudit(t, "cleanup", "cleanup failing after it started")
		if e.Outcome != "failed" || fmt.Sprint(e.Detail["removed"]) != "["+newer+"]" || !strings.Contains(fmt.Sprint(e.Detail["error"]), "lock timeout") {
			t.Errorf("failed cleanup audit record %+v, want failed with %s removed", e, newer)
		}
		st := status(t)
		if rel := st.release(newer); rel == nil || rel.State != "removed" || databases(t)["karta_"+newer] {
			t.Errorf("%s: %+v", newer, rel)
		}
		if rel := st.release(older); rel == nil || rel.State != "retired" || !databases(t)["karta_"+older] {
			t.Errorf("%s was changed by the failed cleanup: %+v", older, rel)
		}
		// Without the lock the next cleanup finishes the job.
		r = op(t, http.MethodPost, "/v1/operator/cleanup", admin, map[string]any{"reason": "cleanup after the lock was released"})
		expectStatus(t, r, http.StatusOK)
		if e := lastAudit(t, "cleanup", "cleanup after the lock was released"); e.Outcome != "succeeded" || fmt.Sprint(e.Detail["removed"]) != "["+older+"]" {
			t.Errorf("second cleanup audit record %+v", e)
		}
		assertRegistryConsistent(t)
	})

	t.Run("disk exhaustion fails safely", func(t *testing.T) {
		t.Cleanup(func() {
			restartPublisher(t, nil)
			active = activeFromRegistry(t)
		})
		// 1. The storage budget refuses a build before it starts.
		data := variant(t, at("2026-06-01T00:00:00Z"), nil, nil)
		authorize(t, data, "integration test: disk exhaustion")
		restartPublisher(t, map[string]string{"KARTA_TEST_BUDGET_MB": "1"})
		submit(t, "over-budget", data, nil, "")
		if s := waitSubmission(t, "over-budget", 60*time.Second); s.State != "rejected" || s.code() != "insufficient_storage" {
			t.Errorf("budget: %+v", s)
		}
		// 2. A staging filesystem too small for the snapshot.
		restartPublisher(t, map[string]string{"KARTA_TEST_STAGING_SIZE": "1m"})
		big := make([]byte, 3<<20)
		submit(t, "too-big-for-staging", big, nil, "")
		if s := waitSubmission(t, "too-big-for-staging", 60*time.Second); s.State != "rejected" || s.code() != "insufficient_storage" {
			t.Errorf("staging: %+v", s)
		}
		// 3. The database filesystem fills up during the import: new release
		// databases go to a tablespace on a small filesystem, filled so that
		// the candidate can be created but not imported.
		tightAvail := fillTightTablespace(t)
		restartPublisher(t, map[string]string{"KARTA_TEST_TABLESPACE": "karta_tight"})
		submit(t, "disk-full", data, nil, "")
		s := waitSubmission(t, "disk-full", 90*time.Second)
		if s.State != "failed" || s.code() != "insufficient_storage" {
			t.Fatalf("disk full: %+v (%s)", s, strOr(s.Reason))
		}
		waitReady(t, true, "ready")
		if got := activeFromRegistry(t); got != active || candidates(t) != 0 {
			t.Fatalf("after disk full: active %s, candidates %d", got, candidates(t))
		}
		expectStatus(t, get(t, "/v1/releases/"+active+lakeTile), http.StatusOK)
		// Space freed: the same snapshot, submitted again (a new marker), is
		// published.
		emptyTightTablespace(t)
		writeAtomic(t, "disk-full.osm.pbf.ready", []byte(digestOf(data)+"\n"), 0o644)
		if s := waitNewSubmission(t, "disk-full", s.ID, 90*time.Second); s.State != "published" {
			t.Fatalf("after freeing space: %+v", s)
		}
		recordMeasurement(t, "tight_tablespace_bytes_free_before_import", tightAvail)
		waitManifest(t, activeFromRegistry(t))
	})

	t.Run("forward activation checks row counts against the release active at the switch", func(t *testing.T) {
		t.Cleanup(func() {
			restartPublisher(t, nil)
			active = activeFromRegistry(t)
		})
		base := activeFromRegistry(t)
		// H: snapshot B with 40 more named POIs, published over base (more
		// rows pass the relative gate), then replaced again by a rollback.
		more := variant(t, at("2026-06-10T00:00:00Z"), nil, func(d *pbfwrite.Data) {
			for i := range int64(40) {
				d.Nodes = append(d.Nodes, pbfwrite.Node{ID: 900000 + i, Lat: 80000 + 1000*(i%8), Lon: 20000 + 1000*(i/8),
					Tags: []pbfwrite.Tag{{K: "amenity", V: "cafe"}, {K: "name", V: fmt.Sprintf("Extra Cafe %d", i)}}})
			}
		})
		authorize(t, more, "integration test: a release with more data")
		submit(t, "more-data", more, nil, "")
		s := waitSubmission(t, "more-data", 90*time.Second)
		if s.State != "published" {
			t.Fatalf("more data: %+v (%s)", s, strOr(s.Reason))
		}
		relH := s.release()
		waitManifest(t, relH)
		r := op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"release_id": base, "reason": "back to the base release"})
		expectStatus(t, r, http.StatusOK)
		waitManifest(t, base)

		// X: newer than H, with base's counts. Built and validated while base
		// is active (the relative gate passes against base), kept ready.
		restartPublisher(t, map[string]string{"KARTA_TEST_AUTO_ACTIVATE": "false"})
		fewer := variant(t, at("2026-06-20T00:00:00Z"), nil, nil)
		authorize(t, fewer, "integration test: validated against the base release")
		submit(t, "fewer-data", fewer, nil, "")
		s = waitSubmission(t, "fewer-data", 90*time.Second)
		if s.State != "ready" || s.code() != "manual_activation" {
			t.Fatalf("candidate: %+v (%s)", s, strOr(s.Reason))
		}
		relX := s.release()
		restartPublisher(t, nil)

		// A rollback (its own policy: no count gate) makes H active.
		r = op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"release_id": relH, "reason": "roll back to the release with more data"})
		expectStatus(t, r, http.StatusOK)
		waitManifest(t, relH)

		// Activating X is judged against H, the release active at the
		// switch, not against base: far fewer POIs, refused.
		r = op(t, http.MethodPost, "/v1/operator/releases/"+relX+"/activate", admin, map[string]any{"reason": "activate the candidate after the rollback"})
		code, _ := r.errorCode(t)
		if r.status != http.StatusConflict || code != "policy_refused" || r.reasonCode(t) != "excessive_data_loss" ||
			!strings.Contains(string(r.body), relH) || !strings.Contains(string(r.body), "pois") {
			t.Fatalf("activate X over H: %d %s", r.status, r.body)
		}
		if got := activeFromRegistry(t); got != relH {
			t.Fatalf("active %s after the refused activation, want %s", got, relH)
		}
		expectStatus(t, get(t, "/v1/releases/"+relH+lakeTile), http.StatusOK)
		if e := lastAudit(t, "activate", "activate the candidate after the rollback"); e.Outcome != "rejected" || e.Target != relX ||
			!strings.Contains(fmt.Sprint(e.Detail["error"]), "would lose too much data") {
			t.Errorf("refused activation audit record %+v", e)
		}
		// Submitting X's snapshot again resumes the never-activated release;
		// its activation is refused by the same gate and it stays ready.
		submit(t, "fewer-data-again", fewer, nil, "")
		if s := waitSubmission(t, "fewer-data-again", 90*time.Second); s.State != "ready" || s.code() != "excessive_data_loss" || s.release() != relX {
			t.Errorf("resubmitted candidate: %+v (%s)", s, strOr(s.Reason))
		}
		if got := activeFromRegistry(t); got != relH {
			t.Fatalf("active %s after the resubmission, want %s", got, relH)
		}

		// With base active again, the same activation passes.
		r = op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"release_id": base, "reason": "back to the base release again"})
		expectStatus(t, r, http.StatusOK)
		r = op(t, http.MethodPost, "/v1/operator/releases/"+relX+"/activate", admin, map[string]any{"reason": "activate the candidate over the base release"})
		expectStatus(t, r, http.StatusOK)
		waitManifest(t, relX)
	})

	t.Run("a publication fails safely when the active release's counts cannot be read", func(t *testing.T) {
		cur := activeFromRegistry(t)
		ctx := context.Background()
		// Make the active release look like a Stage 1 release (no counts in
		// the registry) whose stored import report cannot be decoded.
		reg := superuser(t, "karta_registry")
		defer reg.Close(ctx)
		var saved string
		must(t, reg.QueryRow(ctx, `SELECT counts::text FROM registry.releases WHERE release_id = $1`, cur).Scan(&saved))
		rel := superuser(t, "karta_"+cur)
		defer rel.Close(ctx)
		for _, sql := range []string{`SET default_transaction_read_only = off`,
			`CREATE TEMP TABLE saved_report AS SELECT report FROM karta.release_info`,
			`UPDATE karta.release_info SET report = jsonb_set(report, '{counts}', '"unreadable"')`} {
			if _, err := rel.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := reg.Exec(ctx, `UPDATE registry.releases SET counts = NULL WHERE release_id = $1`, cur); err != nil {
			t.Fatal(err)
		}
		restored := false
		restore := func() {
			if restored {
				return
			}
			restored = true
			if _, err := reg.Exec(ctx, `UPDATE registry.releases SET counts = $2::jsonb WHERE release_id = $1`, cur, saved); err != nil {
				t.Error(err)
			}
			if _, err := rel.Exec(ctx, `UPDATE karta.release_info SET report = (SELECT report FROM saved_report)`); err != nil {
				t.Error(err)
			}
		}
		defer restore()

		data := variant(t, at("2026-06-25T00:00:00Z"), nil, nil)
		authorize(t, data, "integration test: active counts unavailable")
		dbsBefore := databases(t)
		submit(t, "counts-unavailable", data, nil, "")
		s := waitSubmission(t, "counts-unavailable", 60*time.Second)
		if s.State != "failed" || s.code() != "counts_unavailable" || s.release() != "" || !strings.Contains(strOr(s.Reason), cur) {
			t.Fatalf("with unreadable active counts: %+v (%s)", s, strOr(s.Reason))
		}
		// Failed before any build: no candidate or new database, no switch.
		if got := activeFromRegistry(t); got != cur || candidates(t) != 0 || len(databases(t)) != len(dbsBefore) {
			t.Fatalf("after the failure: active %s, %d candidates, %d databases (was %d)", got, candidates(t), len(databases(t)), len(dbsBefore))
		}
		waitReady(t, true, "ready")

		// Counts restored: the same snapshot, submitted again, is checked
		// against them and published.
		restore()
		writeAtomic(t, "counts-unavailable.osm.pbf.ready", []byte(digestOf(data)+"\n"), 0o644)
		s = waitNewSubmission(t, "counts-unavailable", s.ID, 90*time.Second)
		if s.State != "published" {
			t.Fatalf("after restoring the counts: %+v (%s)", s, strOr(s.Reason))
		}
		active = s.release()
		waitManifest(t, active)
		assertRegistryConsistent(t)
	})

	t.Run("an incompatible release is not rolled back to", func(t *testing.T) {
		active = activeFromRegistry(t)
		st := status(t)
		var target *opRelease
		for i := range st.Releases {
			if st.Releases[i].RollbackEligible {
				target = &st.Releases[i]
				break
			}
		}
		if target == nil {
			t.Fatal("no rollback target")
		}
		conn := superuser(t, target.Database)
		defer conn.Close(context.Background())
		ctx := context.Background()
		for _, sql := range []string{`SET default_transaction_read_only = off`, `UPDATE karta.release_info SET schema_major = 99`} {
			if _, err := conn.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
		}
		r := op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"release_id": target.ID, "reason": "incompatible target"})
		if c, _ := r.errorCode(t); r.status != 409 || c != "release_incompatible" {
			t.Errorf("rollback to an incompatible release: %d %s", r.status, r.body)
		}
		if _, err := conn.Exec(ctx, `UPDATE karta.release_info SET schema_major = 1`); err != nil {
			t.Fatal(err)
		}
		if got := activeFromRegistry(t); got != active {
			t.Fatalf("active changed to %s", got)
		}
	})

	sum := traffic.finish(t, "whole publication test")
	recordMeasurement(t, "load_during_publication_test", sum)

	t.Run("a database restart during an import is retried", func(t *testing.T) {
		t.Cleanup(func() { restartPublisher(t, nil) })
		data := variant(t, at("2026-07-01T00:00:00Z"), nil, nil)
		authorize(t, data, "integration test: database restart")
		restartPublisher(t, map[string]string{"KARTA_TEST_OSM2PGSQL": "/usr/local/bin/slow-osm2pgsql"})
		submit(t, "db-restart", data, nil, "")
		waitJobPhase(t, "building", 30*time.Second)
		waitCandidate(t)
		if _, stderr, code := compose(t, "restart", "db"); code != 0 {
			t.Fatalf("restart db: %s", stderr)
		}
		waitReady(t, true, "ready")
		s := waitSubmissionAfterDBRestart(t, "db-restart", 180*time.Second)
		if s.State != "published" || s.Attempts < 2 {
			t.Fatalf("after the database restart: %+v", s)
		}
		active = s.release()
		waitManifest(t, active)
		restartPublisher(t, nil)
		if candidates(t) != 0 {
			t.Error("candidate left after the retried import")
		}
		assertRegistryConsistent(t)
	})

	t.Run("serving refuses a release built with another database toolchain", func(t *testing.T) {
		active = activeFromRegistry(t)
		conn := superuser(t, "karta_"+active)
		defer conn.Close(context.Background())
		ctx := context.Background()
		for _, sql := range []string{`SET default_transaction_read_only = off`,
			`CREATE TEMP TABLE saved AS SELECT toolchain FROM karta.release_info`,
			`UPDATE karta.release_info SET toolchain = jsonb_set(toolchain, '{geos}', '"0.0.0-drift"')`} {
			if _, err := conn.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
		}
		compose(t, "restart", "api")
		st := waitReady(t, false, "release_incompatible")
		if d := fmt.Sprint(st["detail"]); !strings.Contains(d, "toolchain differs") || !strings.Contains(d, "geos") {
			t.Errorf("detail %s", d)
		}
		expectError(t, get(t, "/v1/manifest"), http.StatusServiceUnavailable, "no_active_release", "")
		if _, err := conn.Exec(ctx, `UPDATE karta.release_info SET toolchain = (SELECT toolchain FROM saved)`); err != nil {
			t.Fatal(err)
		}
		compose(t, "restart", "api")
		waitReady(t, true, "ready")
	})

	t.Run("every step is audited and the audit log is append-only", func(t *testing.T) {
		r := op(t, http.MethodGet, "/v1/operator/audit?limit=500", monitor, nil)
		expectStatus(t, r, http.StatusOK)
		var a struct {
			Entries []struct {
				Actor, Source, Action, Target, Outcome, Reason string
			} `json:"entries"`
		}
		r.json(t, &a)
		seen := map[string]bool{}
		for _, e := range a.Entries {
			seen[e.Source+"/"+e.Action+"/"+e.Outcome] = true
			if e.Actor == "" || e.Action == "" || e.Outcome == "" {
				t.Errorf("incomplete audit record %+v", e)
			}
		}
		for _, want := range []string{
			"operator_api/status/denied", "operator_api/rollback/denied", "operator_api/rollback/succeeded", "operator_api/rollback/rejected",
			"operator_api/activate/succeeded", "operator_api/authorize_digest/succeeded", "operator_api/cleanup/succeeded", "operator_api/cleanup/noop",
			"operator_api/cleanup/failed", "operator_api/activate/rejected",
			"inbox/publish/succeeded", "inbox/submission/succeeded", "inbox/submission/rejected", "inbox/submission/failed",
			"inbox/import_started/succeeded", "inbox/import_failed/failed", "system/recover/succeeded", "operator_api/remove_release/succeeded",
		} {
			if !seen[want] {
				t.Errorf("no audit record %s", want)
			}
		}
		// The owner role itself cannot change or delete audit records.
		pw := secret(t, "db_importer_password")
		cfg, _ := pgx.ParseConfig(fmt.Sprintf("host=127.0.0.1 port=%s user=karta_importer dbname=karta_registry sslmode=disable", dbPort))
		cfg.Password = pw
		conn, err := pgx.ConnectConfig(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())
		for _, sql := range []string{`DELETE FROM registry.audit`, `UPDATE registry.audit SET reason = 'x'`, `TRUNCATE registry.audit`} {
			if _, err := conn.Exec(context.Background(), sql); err == nil || !strings.Contains(err.Error(), "append-only") {
				t.Errorf("%s: %v", sql, err)
			}
		}
		// The API role cannot read operator data.
		apiCfg, _ := pgx.ParseConfig(fmt.Sprintf("host=127.0.0.1 port=%s user=karta_api dbname=karta_registry sslmode=disable", dbPort))
		apiCfg.Password = secret(t, "db_api_password")
		apiConn, err := pgx.ConnectConfig(context.Background(), apiCfg)
		if err != nil {
			t.Fatal(err)
		}
		defer apiConn.Close(context.Background())
		for _, table := range []string{"audit", "submissions", "authorizations"} {
			var n int
			if err := apiConn.QueryRow(context.Background(), "SELECT count(*) FROM registry."+table).Scan(&n); err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Errorf("karta_api reads registry.%s: %v", table, err)
			}
		}
	})
}

// reasonCode returns the reason_code of an operator API error.
func (r response) reasonCode(t *testing.T) string {
	t.Helper()
	var e struct {
		Error struct {
			ReasonCode string `json:"reason_code"`
		} `json:"error"`
	}
	r.json(t, &e)
	return e.Error.ReasonCode
}

// auditRecord is an operator audit entry.
type auditRecord struct {
	Actor   string         `json:"actor"`
	Source  string         `json:"source"`
	Action  string         `json:"action"`
	Target  string         `json:"target"`
	Outcome string         `json:"outcome"`
	Reason  string         `json:"reason"`
	Detail  map[string]any `json:"detail"`
}

// lastAudit returns the newest audit record of an operator action with
// that reason.
func lastAudit(t *testing.T, action, reason string) auditRecord {
	t.Helper()
	r := op(t, http.MethodGet, "/v1/operator/audit?limit=200", secret(t, "operator_monitor_token"), nil)
	expectStatus(t, r, http.StatusOK)
	var a struct {
		Entries []auditRecord `json:"entries"`
	}
	r.json(t, &a)
	for _, e := range a.Entries {
		if e.Source == "operator_api" && e.Action == action && e.Reason == reason {
			return e
		}
	}
	t.Fatalf("no audit record for %s %q", action, reason)
	return auditRecord{}
}

// startPinned requests a release's tile and pinned search continuously and
// fails on any non-200 answer or an answer from another release.
type pinnedLoad struct{ *load }

func startPinned(rel string) pinnedLoad {
	l := &load{stop: make(chan struct{}), releases: map[string]bool{rel: true}}
	hc := &http.Client{Timeout: 15 * time.Second}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		for {
			select {
			case <-l.stop:
				return
			default:
			}
			for _, p := range []string{"/v1/releases/" + rel + lakeTile, "/v1/search?" + url.Values{"q": {"English Only Cafe"}, "release_id": {rel}}.Encode()} {
				start := time.Now()
				resp, err := hc.Get(base + p)
				l.mu.Lock()
				l.requests++
				l.latency = append(l.latency, time.Since(start))
				switch {
				case err != nil:
					l.failures = append(l.failures, err.Error())
				case resp.StatusCode != 200:
					l.failures = append(l.failures, fmt.Sprintf("pinned %s: %d", p, resp.StatusCode))
				}
				if err == nil {
					b, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if strings.Contains(p, "search") && resp.StatusCode == 200 && !bytes.Contains(b, []byte(`"node/506"`)) {
						l.failures = append(l.failures, "pinned search did not answer from the pinned release")
					}
				}
				l.mu.Unlock()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return pinnedLoad{l}
}

func (p pinnedLoad) finish(t *testing.T) {
	t.Helper()
	p.load.finish(t, "pinned to the replaced release")
}

func activeFromRegistry(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	reg := superuser(t, "karta_registry")
	defer reg.Close(ctx)
	var id string
	if err := reg.QueryRow(ctx, `SELECT release_id FROM registry.active_release`).Scan(&id); err != nil && err != pgx.ErrNoRows {
		t.Fatal(err)
	}
	return id
}

// assertRegistryConsistent checks the invariants recovery maintains: one
// active release, whose row says active; no release stuck importing,
// validating or removing; a database for every retained release and none for
// any other; no submission left processing.
func assertRegistryConsistent(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	reg := superuser(t, "karta_registry")
	defer reg.Close(ctx)
	var actives, stuck, processing int
	if err := reg.QueryRow(ctx, `SELECT (SELECT count(*) FROM registry.releases WHERE state = 'active'),
       (SELECT count(*) FROM registry.releases WHERE state IN ('importing', 'validating', 'removing')),
       (SELECT count(*) FROM registry.submissions WHERE state = 'processing')`).Scan(&actives, &stuck, &processing); err != nil {
		t.Fatal(err)
	}
	var pointer string
	_ = reg.QueryRow(ctx, `SELECT a.release_id FROM registry.active_release a JOIN registry.releases r USING (release_id) WHERE r.state = 'active'`).Scan(&pointer)
	if actives != 1 || pointer == "" || stuck != 0 || processing != 0 {
		t.Errorf("registry: %d active rows, pointer %q, %d stuck, %d processing", actives, pointer, stuck, processing)
	}
	rows, err := reg.Query(ctx, `SELECT database_name, state FROM registry.releases`)
	if err != nil {
		t.Fatal(err)
	}
	retained := map[string]bool{}
	for rows.Next() {
		var db, state string
		if err := rows.Scan(&db, &state); err != nil {
			t.Fatal(err)
		}
		retained[db] = state == "active" || state == "ready" || state == "retired"
	}
	rows.Close()
	dbs := databases(t)
	for db, keep := range retained {
		if keep != dbs[db] {
			t.Errorf("%s: retained=%v, database exists=%v", db, keep, dbs[db])
		}
	}
	for db := range dbs {
		if strings.HasPrefix(db, "karta_r") && !retained[db] {
			t.Errorf("orphan database %s", db)
		}
	}
}

// blobBoundary returns the offset of the last blob of a PBF file.
func blobBoundary(t *testing.T, b []byte) int {
	t.Helper()
	off, last := 0, 0
	for off < len(b) {
		hl := int(b[off])<<24 | int(b[off+1])<<16 | int(b[off+2])<<8 | int(b[off+3])
		hdr := b[off+4 : off+4+hl]
		// datasize is field 3 (varint) of the BlobHeader.
		size, i := 0, 0
		for i < len(hdr) {
			key := hdr[i]
			i++
			if key == 0x18 {
				shift := 0
				for {
					c := hdr[i]
					i++
					size |= int(c&0x7f) << shift
					if c < 0x80 {
						break
					}
					shift += 7
				}
				continue
			}
			if key&7 == 2 { // length-delimited: skip
				l := int(hdr[i])
				i += 1 + l
			}
		}
		last = off
		off += 4 + hl + size
	}
	return last
}

// fillTightTablespace creates the karta_tight tablespace on the db
// container's small tmpfs, lets karta_importer use it, and fills it so a
// new database fits but an import does not. It returns the bytes left.
//
// The space a database copy really takes is measured with df: PostgreSQL
// copies a template (strategy WAL_LOG) into files that are largely sparse,
// so pg_database_size (the files' apparent size, about 16 MB here) is far
// more than the blocks tmpfs allocates (about 2 MB).
func fillTightTablespace(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	pg := superuser(t, "postgres")
	defer pg.Close(ctx)
	for _, sql := range []string{
		`CREATE TABLESPACE karta_tight LOCATION '/var/lib/postgresql/tight'`,
		`GRANT CREATE ON TABLESPACE karta_tight TO karta_importer`,
	} {
		if _, err := pg.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	empty := tightAvail(t)
	if _, err := pg.Exec(ctx, `CREATE DATABASE karta_probe TEMPLATE karta_template TABLESPACE karta_tight`); err != nil {
		t.Fatal(err)
	}
	copyCost := empty - tightAvail(t)
	if _, err := pg.Exec(ctx, `DROP DATABASE karta_probe`); err != nil {
		t.Fatal(err)
	}
	avail := tightAvail(t)
	// Leave room for the template copy plus 256 KiB; importing the fixture
	// writes several MiB more.
	filler := avail - copyCost - 256<<10
	if copyCost <= 0 || filler <= 0 {
		t.Fatalf("tight filesystem: %d available, a template copy takes %d", avail, copyCost)
	}
	if out, _, code := compose(t, "exec", "-T", "db", "sh", "-c",
		fmt.Sprintf("'head -c %d /dev/zero > /var/lib/postgresql/tight/filler && sync'", filler)); code != 0 {
		t.Fatalf("fill: %s", out)
	}
	left := tightAvail(t)
	t.Logf("tight tablespace: a template copy allocates %d bytes; %d bytes left after filling", copyCost, left)
	return left
}

func tightAvail(t *testing.T) int64 {
	t.Helper()
	out, stderr, code := compose(t, "exec", "-T", "db", "df", "-B1", "--output=avail", "/var/lib/postgresql/tight")
	if code != 0 {
		t.Fatalf("df: %s", stderr)
	}
	f := strings.Fields(out)
	n, err := strconv.ParseInt(f[len(f)-1], 10, 64)
	if err != nil {
		t.Fatalf("df output %q", out)
	}
	return n
}

func emptyTightTablespace(t *testing.T) {
	t.Helper()
	if _, stderr, code := compose(t, "exec", "-T", "db", "rm", "-f", "/var/lib/postgresql/tight/filler"); code != 0 {
		t.Fatalf("rm filler: %s", stderr)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func strOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
