//go:build integration

// Stage 5 acceptance tests, local intake: the authenticated command
// (intake-cli) and the protected-folder watcher (intake-watch) against the
// running stack (compose.test.yaml), with committed fixtures and variants
// generated here. The landing area and the handoff directory are host
// directories the test controls (KARTA_INTAKE_LANDING_HOST_DIR,
// KARTA_INTAKE_HOST_DIR); the intake credentials are registered for the run
// with scripts/operator-credential.sh and removed afterwards.
package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/intake"
)

var (
	intakeDir  = os.Getenv("KARTA_INTAKE_HOST_DIR")
	landingDir = os.Getenv("KARTA_INTAKE_LANDING_HOST_DIR")
)

type intakeSuite struct {
	watchName string
	removeAt  []string // credentials the run registered
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func credentialScript(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(repoRoot, "scripts", "operator-credential.sh"), args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("operator-credential.sh %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// registerWatch registers the watcher's credential under a test name,
// unless secrets/intake_watch_token already belongs to a registered one.
func (s *intakeSuite) registerWatch(t *testing.T) {
	t.Helper()
	tok := filepath.Join(repoRoot, "secrets", "intake_watch_token")
	if b, err := os.ReadFile(tok); err == nil {
		sum := sha256.Sum256([]byte(strings.TrimSpace(string(b))))
		extra, _ := os.ReadFile(filepath.Join(repoRoot, "secrets", "operator_tokens.extra"))
		for _, line := range strings.Split(string(extra), "\n") {
			if f := strings.Fields(line); len(f) == 3 && f[1] == "intake_watch" && f[2] == hex.EncodeToString(sum[:]) {
				s.watchName = f[0]
				return
			}
		}
		t.Fatalf("%s exists but is not a registered intake_watch credential; remove it or register it first", tok)
	}
	credentialScript(t, "add", "test-intake-watch", "intake_watch", "secrets/intake_watch_token")
	s.watchName = "test-intake-watch"
	s.removeAt = append(s.removeAt, "test-intake-watch:secrets/intake_watch_token")
}

func (s *intakeSuite) registerPerson(t *testing.T, name string) string {
	t.Helper()
	credentialScript(t, "add", name, "intake_submit")
	s.removeAt = append(s.removeAt, name+":secrets/"+name+".token")
	return secret(t, name+".token")
}

func (s *intakeSuite) cleanup(t *testing.T) {
	for _, r := range s.removeAt {
		name, file, _ := strings.Cut(r, ":")
		cmd := exec.Command(filepath.Join(repoRoot, "scripts", "operator-credential.sh"), "remove", name, file)
		_ = cmd.Run()
	}
}

func cleanDir(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

// --- the command -------------------------------------------------------------------

type submitResult struct {
	Result struct {
		Name            string `json:"name"`
		SHA256          string `json:"sha256"`
		AuthorizationID int64  `json:"authorization_id"`
		Submission      *struct {
			State      string  `json:"state"`
			ReasonCode *string `json:"reason_code"`
			ReleaseID  *string `json:"release_id"`
		} `json:"submission"`
	} `json:"result"`
	Error any `json:"error"`
}

// runSubmit runs `karta intake submit` in the intake-cli container with
// the person's token file and the snapshot's directory mounted.
func runSubmit(t *testing.T, env []string, tokenName, file string, flags ...string) (submitResult, int, string) {
	t.Helper()
	tok, err := filepath.Abs(filepath.Join(repoRoot, "secrets", tokenName+".token"))
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"run", "--rm", "-T", "-v", shellQuote(filepath.Dir(file) + ":/submit:ro"),
		"-v", shellQuote(tok + ":/run/secrets/intake_token:ro"), "intake-cli", "submit", shellQuote("/submit/" + filepath.Base(file))}
	for _, f := range flags {
		args = append(args, shellQuote(f))
	}
	stdout, stderr, code := composeEnv(t, env, args...)
	var res submitResult
	if i := strings.Index(stdout, "{"); i >= 0 {
		_ = json.Unmarshal([]byte(stdout[i:]), &res)
	}
	return res, code, stderr
}

// commandFile writes a snapshot for the command into a fresh directory.
func commandFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name+".osm.pbf")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// --- the watcher -------------------------------------------------------------------

func startWatcher(t *testing.T, settings map[string]string) {
	t.Helper()
	var envs []string
	for k, v := range settings {
		envs = append(envs, k+"="+v)
	}
	if _, stderr, code := composeEnv(t, envs, "--profile", "intake", "up", "-d", "--no-deps", "--force-recreate", "intake-watch"); code != 0 {
		t.Fatalf("start the watcher: %s", stderr)
	}
}

func stopWatcher(t *testing.T) {
	t.Helper()
	compose(t, "--profile", "intake", "stop", "-t", "5", "intake-watch")
}

func watcherExit(t *testing.T, timeout time.Duration) int {
	t.Helper()
	out, _, _ := compose(t, "--profile", "intake", "ps", "-a", "-q", "intake-watch")
	id := strings.TrimSpace(out)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := exec.Command("docker", "inspect", "-f", "{{.State.Status}} {{.State.ExitCode}}", id).Output()
		if f := strings.Fields(string(b)); err == nil && len(f) == 2 && f[0] == "exited" {
			c, _ := strconv.Atoi(f[1])
			return c
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the watcher did not exit within %s", timeout)
	return -1
}

func waitWatcher(t *testing.T, what string, timeout time.Duration, ok func(*intake.State) bool) *intake.State {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *intake.State
	for time.Now().Before(deadline) {
		if s, err := intake.ReadState(intakeDir); err == nil && s != nil {
			last = s
			if ok(s) {
				return s
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	b, _ := json.MarshalIndent(last, "", "  ")
	out, _, _ := compose(t, "--profile", "intake", "logs", "--no-color", "--tail=40", "intake-watch")
	t.Fatalf("watcher: %s not reached within %s; last state %s\n%s", what, timeout, b, out)
	return nil
}

func landingEntry(name string) func(*intake.State) (intake.LandingEntry, bool) {
	return func(s *intake.State) (intake.LandingEntry, bool) {
		for _, e := range s.Entries {
			if e.Name == name {
				return e, true
			}
		}
		return intake.LandingEntry{}, false
	}
}

// land writes a landing delivery the way a producer does: the snapshot,
// then (if marker is not nil) the completion marker last, both renamed
// into place.
func land(t *testing.T, name string, data []byte, marker []byte) {
	t.Helper()
	put := func(n string, b []byte) {
		tmp := filepath.Join(landingDir, "."+n+".part")
		if err := os.WriteFile(tmp, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(tmp, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(landingDir, n)); err != nil {
			t.Fatal(err)
		}
	}
	put(name+".osm.pbf", data)
	if marker != nil {
		time.Sleep(10 * time.Millisecond) // the marker is never older than the snapshot
		put(name+".osm.pbf.complete", marker)
	}
}

func completionFor(name string, data []byte) []byte {
	return []byte(fmt.Sprintf(`{"format": "karta-delivery/1", "file": "%s.osm.pbf", "sha256": "%s", "size_bytes": %d}`+"\r\n",
		name, strings.ToUpper(digestOf(data)), len(data)))
}

// --- status -----------------------------------------------------------------------

type intakeView struct {
	Intake struct {
		Enabled            bool             `json:"enabled"`
		AutoActivate       bool             `json:"auto_activate"`
		OpenAuthorizations map[string]int64 `json:"open_authorizations"`
		Watcher            struct {
			State *intake.State `json:"state"`
		} `json:"watcher"`
	} `json:"intake"`
	Submissions []struct {
		submission
		Source string `json:"source"`
	} `json:"submissions"`
}

func intakeStatus(t *testing.T) intakeView {
	t.Helper()
	r := op(t, http.MethodGet, "/v1/operator/status", secret(t, "operator_monitor_token"), nil)
	expectStatus(t, r, http.StatusOK)
	var v intakeView
	r.json(t, &v)
	return v
}

// waitDigest waits for the newest submission of that digest to finish.
func waitDigest(t *testing.T, digest string, timeout time.Duration) (submission, string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *submission
	for time.Now().Before(deadline) {
		for _, s := range intakeStatus(t).Submissions {
			if s.SHA256 != nil && *s.SHA256 == digest {
				c := s.submission
				last = &c
				if terminal[s.State] {
					return s.submission, s.Source
				}
				break
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	t.Fatalf("no finished submission of %s within %s; last %+v", digest[:12], timeout, last)
	return submission{}, ""
}

type intakeRecord struct {
	Authorization struct {
		ID           int64      `json:"id"`
		SHA256       string     `json:"sha256"`
		Channel      string     `json:"channel"`
		CreatedBy    string     `json:"created_by"`
		RevokedAt    *time.Time `json:"revoked_at"`
		RevokeReason *string    `json:"revoke_reason"`
	} `json:"authorization"`
}

func intakeRecords(t *testing.T, token string) []intakeRecord {
	t.Helper()
	r := op(t, http.MethodGet, "/v1/operator/intake/authorizations", token, nil)
	expectStatus(t, r, http.StatusOK)
	var l struct {
		Records []intakeRecord `json:"records"`
	}
	r.json(t, &l)
	return l.Records
}

func openRecords(rs []intakeRecord) int {
	n := 0
	for _, r := range rs {
		if r.Authorization.RevokedAt == nil {
			n++
		}
	}
	return n
}

func intakeAuthorize(t *testing.T, token string, data []byte, extra map[string]any) response {
	t.Helper()
	body := map[string]any{"sha256": digestOf(data), "size_bytes": len(data), "region_id": "fixture", "ttl_seconds": 3600,
		"name": "manual-check", "reason": "integration check"}
	for k, v := range extra {
		if v == nil {
			delete(body, k)
		} else {
			body[k] = v
		}
	}
	return op(t, http.MethodPost, "/v1/operator/intake/authorizations", token, body)
}

// -------------------------------------------------------------------------------------

func TestIntake(t *testing.T) {
	if intakeDir == "" || landingDir == "" {
		t.Fatal("the Stage 5 directories are not set; run `make test-integration`")
	}
	admin, monitor := secret(t, "operator_token"), secret(t, "operator_monitor_token")
	s := &intakeSuite{}
	stopWatcher(t)
	resetAll(t)
	cleanDir(t, intakeDir)
	cleanDir(t, landingDir)
	t.Cleanup(func() {
		stopWatcher(t)
		s.cleanup(t)
		cleanDir(t, intakeDir)
		cleanDir(t, landingDir)
		resetAll(t)
	})
	s.registerWatch(t)
	alice := s.registerPerson(t, "test-alice")
	bob := s.registerPerson(t, "test-bob")
	watchTok := secret(t, "intake_watch_token")

	dataA := readRepo(t, snapA)
	submit(t, "a", dataA, nil, "")
	if sub := waitSubmission(t, "a", 90*time.Second); sub.State != "published" {
		t.Fatalf("A: %+v", sub)
	}

	t.Run("the intake is off unless configured", func(t *testing.T) {
		if v := intakeStatus(t); v.Intake.Enabled {
			t.Errorf("enabled by default: %+v", v.Intake)
		}
		r := intakeAuthorize(t, alice, variant(t, at("2026-02-01T12:00:00Z"), nil, nil), nil)
		if code, _ := r.errorCode(t); r.status != http.StatusConflict || code != "intake_disabled" {
			t.Errorf("intake authorization while disabled: %d %s", r.status, r.body)
		}
	})

	intakeOn := map[string]string{"KARTA_TEST_INTAKE_DIR": "/data/intake"}
	restartPublisher(t, intakeOn)

	t.Run("intake credentials reach nothing but their own authorizations", func(t *testing.T) {
		for _, tok := range []string{alice, watchTok} {
			for _, c := range []struct{ method, path string }{
				{http.MethodGet, "/v1/operator/status"}, {http.MethodGet, "/v1/operator/audit"},
				{http.MethodPost, "/v1/operator/authorizations"}, {http.MethodPost, "/v1/operator/rollback"},
				{http.MethodPost, "/v1/operator/cleanup"}, {http.MethodPost, "/v1/operator/online/pause"},
				{http.MethodPost, "/v1/operator/intake/resume"}, {http.MethodPost, "/v1/operator/authorizations/" + digestOf(dataA) + "/revoke"},
			} {
				var body any
				if c.method == http.MethodPost {
					body = map[string]any{"reason": "x", "sha256": digestOf(dataA)}
				}
				if r := op(t, c.method, c.path, tok, body); r.status != http.StatusForbidden {
					t.Errorf("%s %s with an intake credential: %d", c.method, c.path, r.status)
				}
			}
		}
		if r := op(t, http.MethodGet, "/v1/operator/intake/authorizations", monitor, nil); r.status != http.StatusForbidden {
			t.Errorf("the monitor credential lists intake authorizations: %d", r.status)
		}
		x := variant(t, at("2026-02-02T00:00:00Z"), nil, nil)
		for name, c := range map[string]struct {
			extra  map[string]any
			status int
			code   string
		}{
			"another region":            {map[string]any{"region_id": "tehran-chitgar"}, http.StatusConflict, "region_mismatch"},
			"no size":                   {map[string]any{"size_bytes": nil}, http.StatusBadRequest, ""},
			"beyond the cap":            {map[string]any{"ttl_seconds": 400 * 24 * 3600}, http.StatusBadRequest, ""},
			"larger than the input cap": {map[string]any{"size_bytes": 1 << 40}, http.StatusConflict, "too_large"},
		} {
			r := intakeAuthorize(t, alice, x, c.extra)
			if r.status != c.status || (c.code != "" && r.reasonCode(t) != c.code) {
				t.Errorf("%s: %d %s", name, r.status, r.body)
			}
		}
		// Ownership: bob cannot close alice's authorization; an operator
		// revoke (publish scope) closes every open row of the digest.
		r := intakeAuthorize(t, alice, x, nil)
		expectStatus(t, r, http.StatusCreated)
		var a struct {
			Authorization struct {
				ID int64 `json:"id"`
			} `json:"authorization"`
		}
		r.json(t, &a)
		if r := op(t, http.MethodPost, fmt.Sprintf("/v1/operator/intake/authorizations/%d/close", a.Authorization.ID), bob, map[string]any{"reason": "not mine"}); r.status != http.StatusNotFound {
			t.Errorf("bob closed alice's authorization: %d %s", r.status, r.body)
		}
		expectStatus(t, intakeAuthorize(t, bob, x, nil), http.StatusCreated)
		r = op(t, http.MethodPost, "/v1/operator/authorizations/"+digestOf(x)+"/revoke", admin, map[string]any{"reason": "stop button"})
		expectStatus(t, r, http.StatusOK)
		if n := openRecords(intakeRecords(t, alice)) + openRecords(intakeRecords(t, bob)); n != 0 {
			t.Errorf("%d intake authorizations survive the operator's revoke", n)
		}
	})

	dataC := variant(t, at("2026-03-01T00:00:00Z"), nil, nil)
	t.Run("the command needs an independent expectation and refuses a mismatch before authorizing", func(t *testing.T) {
		f := commandFile(t, "c", dataC)
		before := len(intakeRecords(t, alice))
		if _, code, stderr := runSubmit(t, nil, "test-alice", f); code != 2 {
			t.Errorf("no expectation: exit %d %s", code, stderr)
		}
		res, code, stderr := runSubmit(t, nil, "test-alice", f, "--expect-sha256", strings.Repeat("0", 64))
		if code != 3 || res.Result.AuthorizationID != 0 {
			t.Errorf("wrong digest: exit %d %+v %s", code, res, stderr)
		}
		if after := len(intakeRecords(t, alice)); after != before {
			t.Errorf("a refused expectation created an authorization (%d -> %d)", before, after)
		}
	})

	t.Run("the command publishes a complete file through the common path", func(t *testing.T) {
		f := commandFile(t, "c", dataC)
		res, code, stderr := runSubmit(t, nil, "test-alice", f, "--expect-sha256", digestOf(dataC), "--expect-size", strconv.Itoa(len(dataC)),
			"--reason", "outage delivery by alice")
		if code != 0 || res.Result.Submission == nil || res.Result.Submission.State != "published" {
			t.Fatalf("exit %d %+v\n%s", code, res, stderr)
		}
		var own *intakeRecord
		for _, r := range intakeRecords(t, alice) {
			if r.Authorization.ID == res.Result.AuthorizationID {
				own = &r
			}
		}
		if own == nil || own.Authorization.Channel != "intake_submit" || own.Authorization.CreatedBy != "test-alice" ||
			own.Authorization.SHA256 != digestOf(dataC) {
			t.Errorf("authorization %d: %+v", res.Result.AuthorizationID, own)
		}
		sub, src := waitDigest(t, digestOf(dataC), 10*time.Second)
		if src != "intake" || status(t).activeID() != sub.release() {
			t.Errorf("submission %+v source %s", sub, src)
		}
		// The intake closes its authorization once the submission is final.
		if n := openRecords(intakeRecords(t, alice)); n != 0 {
			t.Errorf("%d open authorizations left", n)
		}
	})

	startWatcher(t, nil)
	waitWatcher(t, "a passing preflight", 30*time.Second, func(s *intake.State) bool { return s.LastScanAt != nil && s.Preflight.OK })

	dataD := variant(t, at("2026-04-01T00:00:00Z"), nil, nil)
	t.Run("the watcher delivers only complete landing deliveries", func(t *testing.T) {
		// A truncated copy with a self-made checksum is never a completion signal.
		bad := variant(t, at("2026-03-15T00:00:00Z"), nil, nil)
		cut := bad[:blobBoundary(t, bad)]
		land(t, "cut", cut, nil)
		sum := sha256.Sum256(cut)
		if err := os.WriteFile(filepath.Join(landingDir, "cut.osm.pbf.sha256"), []byte(hex.EncodeToString(sum[:])+"  cut.osm.pbf\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// A marker computed from the source copy refuses the truncated one.
		land(t, "short", cut, completionFor("short", bad))
		waitWatcher(t, "refusals", 30*time.Second, func(s *intake.State) bool {
			c, ok1 := landingEntry("cut")(s)
			sh, ok2 := landingEntry("short")(s)
			return ok1 && ok2 && c.State == intake.WaitingForCompletion && sh.State == intake.StateRefused && sh.Code == "size_mismatch"
		})
		if v, ok := publisherMetric(t, "karta_intake_landing_refused"); !ok || v < 1 {
			t.Errorf("karta_intake_landing_refused %v %v", v, ok)
		}
		if n := len(intakeRecords(t, watchTok)); n != 0 {
			t.Errorf("refused deliveries created %d authorizations", n)
		}
		// The complete delivery.
		land(t, "d", dataD, completionFor("d", dataD))
		sub, src := waitDigest(t, digestOf(dataD), 90*time.Second)
		if sub.State != "published" || src != "intake" || status(t).activeID() != sub.release() {
			t.Fatalf("D: %+v %s", sub, src)
		}
		recs := intakeRecords(t, watchTok)
		if len(recs) != 1 || recs[0].Authorization.Channel != "intake_watch" || recs[0].Authorization.CreatedBy != s.watchName {
			t.Errorf("watch authorizations %+v", recs)
		}
		if v, ok := publisherMetric(t, "karta_intake_preflight_ok"); !ok || v != 1 {
			t.Errorf("karta_intake_preflight_ok %v %v", v, ok)
		}
	})
	relD := status(t).activeID()

	t.Run("an insecure landing area or a foreign file delivers nothing", func(t *testing.T) {
		dataK := variant(t, at("2026-04-15T00:00:00Z"), nil, nil)
		if err := os.Chmod(landingDir, 0o777); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(landingDir, 0o755) }()
		st := waitWatcher(t, "a failing preflight", 30*time.Second, func(s *intake.State) bool { return !s.Preflight.OK })
		found := false
		for _, p := range st.Preflight.Problems {
			found = found || p.Code == intake.ProblemWorldWritable
		}
		if !found {
			t.Errorf("preflight problems %+v", st.Preflight.Problems)
		}
		land(t, "k", dataK, completionFor("k", dataK))
		time.Sleep(4 * time.Second)
		if v, ok := publisherMetric(t, "karta_intake_preflight_ok"); !ok || v != 0 {
			t.Errorf("karta_intake_preflight_ok %v %v", v, ok)
		}
		for _, sub := range intakeStatus(t).Submissions {
			if sub.SHA256 != nil && *sub.SHA256 == digestOf(dataK) {
				t.Fatalf("delivered from an insecure landing area: %+v", sub)
			}
		}
		for _, n := range []string{"k.osm.pbf", "k.osm.pbf.complete"} {
			_ = os.Remove(filepath.Join(landingDir, n))
		}
		if err := os.Chmod(landingDir, 0o755); err != nil {
			t.Fatal(err)
		}
		waitWatcher(t, "a passing preflight", 30*time.Second, func(s *intake.State) bool { return s.Preflight.OK })
		// A delivery owned by another account than the landing owner (needs
		// root to create; probed on a hidden name the watcher ignores).
		probe := filepath.Join(landingDir, ".chown-probe")
		if err := os.WriteFile(probe, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		err := os.Lchown(probe, 12345, 12345)
		_ = os.Remove(probe)
		if err != nil {
			t.Logf("not root: the foreign-owner case is covered by the unit tests only (%v)", err)
			return
		}
		for _, f := range []struct {
			name string
			b    []byte
		}{{"foreign.osm.pbf", dataK}, {"foreign.osm.pbf.complete", completionFor("foreign", dataK)}} {
			tmp := filepath.Join(landingDir, "."+f.name+".part")
			if err := os.WriteFile(tmp, f.b, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Lchown(tmp, 12345, 12345); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, filepath.Join(landingDir, f.name)); err != nil {
				t.Fatal(err)
			}
		}
		waitWatcher(t, "a refused foreign delivery", 30*time.Second, func(s *intake.State) bool {
			e, ok := landingEntry("foreign")(s)
			return ok && e.State == intake.StateRefused && e.Code == intake.CodeWrongOwner
		})
		for _, n := range []string{"foreign.osm.pbf", "foreign.osm.pbf.complete"} {
			_ = os.Remove(filepath.Join(landingDir, n))
		}
		if n := len(intakeRecords(t, watchTok)); n != 1 {
			t.Errorf("%d watch authorizations, want only D's", n)
		}
	})

	dataE := variant(t, at("2026-05-01T00:00:00Z"), nil, nil)
	dataF := variant(t, at("2026-06-01T00:00:00Z"), nil, nil)
	t.Run("a rollback pauses watcher activation, not a deliberate command delivery", func(t *testing.T) {
		// Map and search requests continue throughout (rollback, intake
		// builds and the switch), pinned requests stay consistent.
		l := startLoad(4)
		defer func() { recordMeasurement(t, "stage5-intake-rollback-load", l.finish(t, "intake rollback")) }()
		relC := ""
		for _, sub := range intakeStatus(t).Submissions {
			if sub.SHA256 != nil && *sub.SHA256 == digestOf(dataC) {
				relC = sub.release()
			}
		}
		r := op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"release_id": relC, "reason": "bad intake data"})
		expectStatus(t, r, http.StatusOK)
		if v := intakeStatus(t); v.Intake.AutoActivate {
			t.Fatalf("the watcher is not paused after a rollback: %+v", v.Intake)
		}
		land(t, "e", dataE, completionFor("e", dataE))
		sub, _ := waitDigest(t, digestOf(dataE), 90*time.Second)
		if sub.State != "ready" || sub.code() != "intake_activation_paused" || status(t).activeID() != relC {
			t.Errorf("E while paused: %+v (active %s)", sub, status(t).activeID())
		}
		res, code, stderr := runSubmit(t, nil, "test-alice", commandFile(t, "f", dataF), "--expect-sha256", digestOf(dataF))
		if code != 0 || res.Result.Submission == nil || res.Result.Submission.State != "published" {
			t.Errorf("a command delivery during the pause: exit %d %+v %s", code, res, stderr)
		}
		if r := op(t, http.MethodPost, "/v1/operator/intake/resume", monitor, map[string]any{"reason": "x"}); r.status != http.StatusForbidden {
			t.Errorf("monitor resumed the watcher: %d", r.status)
		}
		expectStatus(t, op(t, http.MethodPost, "/v1/operator/intake/resume", admin, map[string]any{"reason": "checked"}), http.StatusOK)
		if v := intakeStatus(t); !v.Intake.AutoActivate {
			t.Errorf("not resumed: %+v", v.Intake)
		}
		if a := lastAudit(t, "intake_resume", "checked"); a.Outcome != "succeeded" {
			t.Errorf("resume audit %+v", a)
		}
	})
	_ = relD

	dataG := variant(t, at("2026-07-01T00:00:00Z"), nil, nil)
	t.Run("a revocation during the build stops activation; a new authorization activates without a rebuild", func(t *testing.T) {
		slow := map[string]string{"KARTA_TEST_INTAKE_DIR": "/data/intake", "KARTA_TEST_OSM2PGSQL": "/usr/local/bin/slow-osm2pgsql"}
		restartPublisher(t, slow)
		active := status(t).activeID()
		if _, code, stderr := runSubmit(t, nil, "test-alice", commandFile(t, "g", dataG), "--expect-sha256", digestOf(dataG), "--no-wait"); code != 0 {
			t.Fatalf("exit %d %s", code, stderr)
		}
		waitJobPhase(t, "building", 60*time.Second)
		r := op(t, http.MethodPost, "/v1/operator/authorizations/"+digestOf(dataG)+"/revoke", admin, map[string]any{"reason": "revoked mid-build"})
		expectStatus(t, r, http.StatusOK)
		sub, _ := waitDigest(t, digestOf(dataG), 120*time.Second)
		if sub.State != "rejected" || sub.code() != "authorization_revoked" || status(t).activeID() != active {
			t.Fatalf("G after the revoke: %+v (active %s, was %s)", sub, status(t).activeID(), active)
		}
		ready := status(t).release(sub.release())
		if sub.release() == "" || ready == nil || ready.State != "ready" {
			t.Fatalf("the built release is not kept ready: %+v %+v", sub, ready)
		}
		dbs := len(databases(t))
		restartPublisher(t, intakeOn) // fast again: a rebuild would be visible as a new database
		authorize(t, dataG, "re-authorized after review")
		deadline := time.Now().Add(90 * time.Second)
		for status(t).activeID() != sub.release() && time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
		}
		if got := status(t).activeID(); got != sub.release() {
			t.Fatalf("the ready release was not activated: active %s, want %s", got, sub.release())
		}
		if n := len(databases(t)); n != dbs {
			t.Errorf("re-authorizing rebuilt the release: %d databases, were %d", n, dbs)
		}
	})

	dataG2 := variant(t, at("2026-07-15T00:00:00Z"), nil, nil)
	t.Run("an authorization that expires during the build stops activation", func(t *testing.T) {
		restartPublisher(t, map[string]string{"KARTA_TEST_INTAKE_DIR": "/data/intake", "KARTA_TEST_INTAKE_MAX_AGE": "1m",
			"KARTA_TEST_OSM2PGSQL": "/usr/local/bin/slow-osm2pgsql"})
		defer restartPublisher(t, intakeOn)
		active := status(t).activeID()
		if _, _, code := compose(t, "exec", "-T", "publisher", "touch", "/tmp/karta-hold-import"); code != 0 {
			t.Fatal("cannot hold the import")
		}
		res, code, stderr := runSubmit(t, nil, "test-alice", commandFile(t, "g2", dataG2), "--expect-sha256", digestOf(dataG2), "--no-wait")
		if code != 0 {
			t.Fatalf("exit %d %s", code, stderr)
		}
		var expires time.Time
		r := op(t, http.MethodGet, "/v1/operator/intake/authorizations", alice, nil)
		var l struct {
			Records []struct {
				Authorization struct {
					ID        int64     `json:"id"`
					ExpiresAt time.Time `json:"expires_at"`
				} `json:"authorization"`
			} `json:"records"`
		}
		r.json(t, &l)
		for _, rec := range l.Records {
			if rec.Authorization.ID == res.Result.AuthorizationID {
				expires = rec.Authorization.ExpiresAt
			}
		}
		if expires.IsZero() || time.Until(expires) > 61*time.Second {
			t.Fatalf("the authorization's lifetime is not capped at 1m: expires %s", expires)
		}
		waitJobPhase(t, "building", 60*time.Second)
		time.Sleep(time.Until(expires) + 2*time.Second)
		compose(t, "exec", "-T", "publisher", "rm", "-f", "/tmp/karta-hold-import")
		sub, _ := waitDigest(t, digestOf(dataG2), 120*time.Second)
		if sub.State != "rejected" || sub.code() != "authorization_expired" || status(t).activeID() != active {
			t.Fatalf("G2 after expiry: %+v (active %s, was %s)", sub, status(t).activeID(), active)
		}
		if rel := status(t).release(sub.release()); rel == nil || rel.State != "ready" {
			t.Errorf("the built release is not kept ready: %+v", rel)
		}
	})

	t.Run("a removed credential is refused at once, without a publisher restart", func(t *testing.T) {
		start, _ := publisherMetric(t, "karta_publisher_start_time_seconds")
		expectStatus(t, op(t, http.MethodGet, "/v1/operator/intake/authorizations", bob, nil), http.StatusOK)
		credentialScript(t, "remove", "test-bob")
		s.removeAt = s.removeAt[:len(s.removeAt)-1] // removed here
		if r := op(t, http.MethodGet, "/v1/operator/intake/authorizations", bob, nil); r.status != http.StatusUnauthorized {
			t.Errorf("a removed credential still works: %d", r.status)
		}
		// A credentials file that does not parse refuses everything.
		tokens := filepath.Join(repoRoot, "secrets", "operator_tokens")
		good, err := os.ReadFile(tokens)
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(tokens, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString("broken line\n")
		f.Close()
		r := op(t, http.MethodGet, "/v1/operator/status", monitor, nil)
		if code, _ := r.errorCode(t); r.status != http.StatusServiceUnavailable || code != "credentials_unavailable" {
			t.Errorf("a broken credentials file: %d %s", r.status, r.body)
		}
		if err := os.WriteFile(tokens, good, 0o644); err != nil { // in place: same inode
			t.Fatal(err)
		}
		expectStatus(t, op(t, http.MethodGet, "/v1/operator/status", monitor, nil), http.StatusOK)
		if now, _ := publisherMetric(t, "karta_publisher_start_time_seconds"); now != start {
			t.Errorf("the publisher restarted (%v -> %v)", start, now)
		}
	})

	dataH := variant(t, at("2026-08-01T00:00:00Z"), nil, nil)
	t.Run("a flooded inbox does not stall the intake", func(t *testing.T) {
		for i := 0; i < 1001; i++ {
			if err := os.WriteFile(filepath.Join(inboxDir, fmt.Sprintf("junk-%04d.osm.pbf", i)), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		defer cleanDir(t, inboxDir)
		land(t, "h", dataH, completionFor("h", dataH))
		if sub, _ := waitDigest(t, digestOf(dataH), 90*time.Second); sub.State != "published" {
			t.Fatalf("H with a flooded inbox: %+v", sub)
		}
	})

	t.Run("duplicate and older deliveries are refused by the common checks", func(t *testing.T) {
		active := status(t).activeID()
		var firstID int64
		for _, sub := range intakeStatus(t).Submissions {
			if sub.SHA256 != nil && *sub.SHA256 == digestOf(dataD) && (firstID == 0 || sub.ID < firstID) {
				firstID = sub.ID
			}
		}
		land(t, "d-again", dataD, completionFor("d-again", dataD))
		deadline := time.Now().Add(60 * time.Second)
		var again *submission
		for again == nil && time.Now().Before(deadline) {
			for _, sub := range intakeStatus(t).Submissions {
				if sub.SHA256 != nil && *sub.SHA256 == digestOf(dataD) && sub.ID > firstID && terminal[sub.State] {
					c := sub.submission
					again = &c
				}
			}
			time.Sleep(400 * time.Millisecond)
		}
		if again == nil || (again.State != "duplicate" && again.State != "rejected") || status(t).activeID() != active {
			t.Errorf("D again: %+v (active %s, was %s)", again, status(t).activeID(), active)
		}
		older := variant(t, at("2026-02-20T00:00:00Z"), nil, nil)
		res, code, stderr := runSubmit(t, nil, "test-alice", commandFile(t, "older", older), "--expect-sha256", digestOf(older))
		if code != 4 || res.Result.Submission == nil || res.Result.Submission.State != "rejected" || status(t).activeID() != active {
			t.Errorf("older data: exit %d %+v %s", code, res, stderr)
		}
	})

	dataI := variant(t, at("2026-09-01T00:00:00Z"), nil, nil)
	t.Run("a command that crashed after authorizing is cleaned up by the next run", func(t *testing.T) {
		f := commandFile(t, "i", dataI)
		if _, code, _ := runSubmit(t, []string{"KARTA_TEST_INTAKE_FAILPOINTS=intake.after_authorize"}, "test-alice", f,
			"--expect-sha256", digestOf(dataI)); code != 99 {
			t.Fatalf("the failpoint did not stop the command: exit %d", code)
		}
		if n := openRecords(intakeRecords(t, alice)); n != 1 {
			t.Fatalf("%d open authorizations after the crash", n)
		}
		res, code, stderr := runSubmit(t, nil, "test-alice", f, "--expect-sha256", digestOf(dataI))
		if code != 0 || res.Result.Submission == nil || res.Result.Submission.State != "published" {
			t.Fatalf("the rerun: exit %d %+v %s", code, res, stderr)
		}
		if n := openRecords(intakeRecords(t, alice)); n != 0 {
			t.Errorf("the orphaned authorization is still open (%d)", n)
		}
	})

	dataJ := variant(t, at("2026-09-15T00:00:00Z"), nil, nil)
	t.Run("a watcher that crashed before the marker completes the handoff on restart", func(t *testing.T) {
		startWatcher(t, map[string]string{"KARTA_TEST_INTAKE_FAILPOINTS": "intake.before_marker"})
		land(t, "j", dataJ, completionFor("j", dataJ))
		if code := watcherExit(t, 60*time.Second); code != 99 {
			t.Fatalf("the watcher exited %d", code)
		}
		// The handed-off copy is changed after it was hashed and authorized:
		// recovery must not complete it.
		tampered := 0
		entries, _ := os.ReadDir(intakeDir)
		for _, e := range entries {
			if n := e.Name(); strings.HasSuffix(n, ".osm.pbf") && !strings.HasPrefix(n, ".") {
				f, err := os.OpenFile(filepath.Join(intakeDir, n), os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = f.Write([]byte{0})
				f.Close()
				tampered++
			}
		}
		if tampered != 1 {
			t.Fatalf("%d visible handoff snapshots after the crash, want 1", tampered)
		}
		startWatcher(t, nil)
		if sub, _ := waitDigest(t, digestOf(dataJ), 90*time.Second); sub.State != "published" {
			t.Fatalf("J after the restart: %+v", sub)
		}
		for _, sub := range intakeStatus(t).Submissions {
			if sub.SHA256 != nil && *sub.SHA256 != digestOf(dataJ) && strings.HasPrefix(sub.Name, "fixture-w") && sub.State == "published" &&
				sub.release() == status(t).activeID() {
				t.Errorf("the tampered copy was published: %+v", sub)
			}
		}
		st := waitWatcher(t, "the handoff recorded", 30*time.Second, func(s *intake.State) bool {
			e, ok := landingEntry("j")(s)
			return !ok || e.State == intake.StateDelivered
		})
		for _, c := range st.Consumed {
			if c.Name == "j" && c.Outcome == "" {
				t.Errorf("consumed %+v", c)
			}
		}
	})

	t.Run("a cleanly stopped watcher says so", func(t *testing.T) {
		stopWatcher(t)
		st := waitWatcher(t, "the clean stop", 20*time.Second, func(s *intake.State) bool { return s.StoppedAt != nil })
		if v, ok := publisherMetric(t, "karta_intake_watcher_stopped"); !ok || v != 1 {
			t.Errorf("karta_intake_watcher_stopped %v %v (%v)", v, ok, st.StoppedAt)
		}
	})
}
