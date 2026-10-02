//go:build integration

// Stage 4 operations tests (tier A: committed fixtures only): the
// whole-publication deadline and the cancellation of every process an import
// started, graceful shutdown during an import, and the bounded command-line
// import. Run by `make test-integration` against the same isolated stack.
package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	promBase       = env("KARTA_TEST_PROMETHEUS_URL", "http://127.0.0.1:18090")
	apiMetricsBase = env("KARTA_TEST_METRICS_URL", "http://127.0.0.1:18083")
)

// containerProcesses lists the command lines of every process running in
// the publisher container, as the host sees them (docker top), so nothing
// inside the container has to cooperate.
func containerProcesses(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("docker", "top", publisherContainer(t), "-o", "pid,args").CombinedOutput()
	if err != nil {
		t.Fatalf("docker top: %v %s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 0 {
		lines = lines[1:] // header
	}
	return lines
}

// strayImportProcesses returns the processes an import may leave behind.
func strayImportProcesses(t *testing.T) []string {
	t.Helper()
	var stray []string
	for _, p := range containerProcesses(t) {
		for _, s := range []string{"osm2pgsql", "sleep 3600", "forking-osm2pgsql", "slow-osm2pgsql"} {
			if strings.Contains(p, s) {
				stray = append(stray, p)
				break
			}
		}
	}
	return stray
}

func waitProcess(t *testing.T, what string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, p := range containerProcesses(t) {
			if strings.Contains(p, what) {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no %q process in the publisher within %s: %v", what, timeout, containerProcesses(t))
}

func noStrayProcesses(t *testing.T) {
	t.Helper()
	// The group kill is immediate; allow the init a moment to reap.
	deadline := time.Now().Add(5 * time.Second)
	for {
		stray := strayImportProcesses(t)
		if len(stray) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes left by the import: %q", stray)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func publisherExec(t *testing.T, args ...string) {
	t.Helper()
	if _, stderr, code := compose(t, append([]string{"exec", "-T", "publisher"}, args...)...); code != 0 {
		t.Fatalf("exec %v: %s", args, stderr)
	}
}

// candidateName waits for a build's candidate database and returns its name.
func candidateName(t *testing.T) string {
	t.Helper()
	waitCandidate(t)
	for db := range databases(t) {
		if strings.HasPrefix(db, "karta_c") {
			return db
		}
	}
	t.Fatal("candidate vanished")
	return ""
}

// timeoutAudited checks the submission's audit record carries the code.
func timeoutAudited(t *testing.T, name string) {
	t.Helper()
	r := op(t, http.MethodGet, "/v1/operator/audit?limit=200", secret(t, "operator_monitor_token"), nil)
	expectStatus(t, r, http.StatusOK)
	var a struct {
		Entries []struct {
			Action  string         `json:"action"`
			Target  string         `json:"target"`
			Outcome string         `json:"outcome"`
			Detail  map[string]any `json:"detail"`
		} `json:"entries"`
	}
	r.json(t, &a)
	for _, e := range a.Entries {
		if e.Action == "submission" && e.Target == name {
			if e.Outcome != "failed" || e.Detail["reason_code"] != "publication_timeout" {
				t.Fatalf("audit record %+v", e)
			}
			return
		}
	}
	t.Fatalf("no audit record for submission %s", name)
}

func touchMarker(t *testing.T, name string) {
	t.Helper()
	now := time.Now()
	if err := os.Chtimes(filepath.Join(inboxDir, name+".osm.pbf.ready"), now, now); err != nil {
		t.Fatal(err)
	}
}

// asRole connects to a database as a Karta role with its password from
// ./secrets (the superuser helper connects as postgres).
func asRole(t *testing.T, user, secretFile, database string) *pgx.Conn {
	t.Helper()
	pw, err := os.ReadFile(filepath.Join(repoRoot, "secrets", secretFile))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig("host=127.0.0.1 port=" + dbPort + " user=" + user + " dbname=" + database + " sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Password = strings.TrimSpace(string(pw))
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// waitPrometheus waits until Prometheus answers as ready.
func waitPrometheus(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := client.Get(promBase + "/-/ready"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatal("prometheus did not become ready")
}

// promQuery runs a PromQL query and returns the result's series.
func promQuery(t *testing.T, q string) []map[string]any {
	t.Helper()
	resp, err := client.Get(promBase + "/api/v1/query?" + url.Values{"query": {q}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r struct {
		Status string `json:"status"`
		Data   struct {
			Result []map[string]any `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || r.Status != "success" {
		t.Fatalf("query %s: %v %s", q, err, r.Status)
	}
	return r.Data.Result
}

// firing returns the names of the alerts firing in Prometheus.
func firing(t *testing.T) map[string]bool {
	t.Helper()
	resp, err := client.Get(promBase + "/api/v1/alerts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r struct {
		Data struct {
			Alerts []struct {
				Labels map[string]string `json:"labels"`
				State  string            `json:"state"`
			} `json:"alerts"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, a := range r.Data.Alerts {
		if a.State == "firing" {
			out[a.Labels["alertname"]] = true
		}
	}
	return out
}

func metricsStatus(t *testing.T, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, apiMetricsBase+"/metrics", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// script runs a repository script against the test project and returns its
// combined output and exit code.
func script(t *testing.T, name string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("./scripts/"+name, args...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "COMPOSE="+composeCmd)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return string(out), code
}

// containerStart identifies a container's current process (id and start
// time), to prove it was not restarted.
func containerStart(t *testing.T, service string) string {
	t.Helper()
	id, _, _ := compose(t, "ps", "-q", service)
	out, err := exec.Command("docker", "inspect", "-f", "{{.Id}} {{.State.StartedAt}}", strings.TrimSpace(id)).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func publishVariant(t *testing.T, name, ts string) string {
	t.Helper()
	data := variant(t, at(ts), nil, nil)
	authorize(t, data, "integration test: "+name)
	submit(t, name, data, nil, "")
	s := waitSubmission(t, name, 90*time.Second)
	if s.State != "published" {
		t.Fatalf("%s: %+v", name, s)
	}
	waitManifest(t, s.release())
	return s.release()
}

func TestOperations(t *testing.T) {
	if inboxDir == "" {
		t.Fatal("KARTA_INBOX_HOST_DIR is not set; run `make test-integration`")
	}
	resetAll(t)
	t.Cleanup(func() { resetAll(t) })
	submit(t, "a", readRepo(t, snapA), nil, "")
	if s := waitSubmission(t, "a", 90*time.Second); s.State != "published" {
		t.Fatalf("A: %+v", s)
	}
	active := activeFromRegistry(t)
	waitManifest(t, active)

	t.Run("a hung import is stopped at the publication deadline with every process it started", func(t *testing.T) {
		t.Cleanup(func() { restartPublisher(t, nil) })
		restartPublisher(t, map[string]string{"KARTA_TEST_OSM2PGSQL": "/usr/local/bin/forking-osm2pgsql", "KARTA_TEST_PUBLISH_TIMEOUT": "20s"})
		publisherExec(t, "touch", "/tmp/karta-fork-hang")
		data := variant(t, at("2026-05-01T00:00:00Z"), nil, nil)
		authorize(t, data, "integration test: hung import")
		traffic := startLoad(2)
		start := time.Now()
		submit(t, "hung", data, nil, "")
		waitJobPhase(t, "building", 30*time.Second)
		// The wrapper hangs, and its child holds osm2pgsql's output pipe.
		waitProcess(t, "sleep 3600", 30*time.Second)
		s := waitSubmission(t, "hung", 90*time.Second)
		elapsed := time.Since(start)
		if s.State != "failed" || s.code() != "publication_timeout" {
			t.Fatalf("submission %+v", s)
		}
		if elapsed > 50*time.Second {
			t.Errorf("stopping a hung import took %s with a 20s deadline", elapsed)
		}
		for _, want := range []string{"KARTA_PUBLISH_TIMEOUT (20s)", "while building", "not retried automatically", "touch the ready marker"} {
			if !strings.Contains(strOr(s.Reason), want) {
				t.Errorf("reason lacks %q: %s", want, strOr(s.Reason))
			}
		}
		noStrayProcesses(t)
		if n := candidates(t); n != 0 {
			t.Errorf("%d candidate database(s) left", n)
		}
		if got := activeFromRegistry(t); got != active {
			t.Fatalf("active %s, want %s", got, active)
		}
		if r := status(t).release(s.release()); r == nil || r.State != "failed" || !strings.Contains(strOr(r.FailureReason), "publication timed out") {
			t.Errorf("the failed build's release record: %+v", r)
		}
		timeoutAudited(t, "hung")
		// Not retried automatically: several scans later it is still the one
		// failed attempt and nothing is building.
		time.Sleep(4 * time.Second)
		if again := waitSubmission(t, "hung", 5*time.Second); again.ID != s.ID || again.Attempts != 1 || again.State != "failed" || status(t).Job != nil {
			t.Fatalf("after more scans: %+v, job %+v", again, status(t).Job)
		}
		traffic.finish(t, "during the hung import")
		recordMeasurement(t, "stage4_hung_import_stopped_after_seconds", elapsed.Seconds())
		assertRegistryConsistent(t)

		t.Run("submitted again, it publishes although a process it started keeps the output open", func(t *testing.T) {
			publisherExec(t, "rm", "-f", "/tmp/karta-fork-hang")
			start := time.Now()
			touchMarker(t, "hung")
			s2 := waitNewSubmission(t, "hung", s.ID, 120*time.Second)
			if s2.State != "published" {
				t.Fatalf("resubmission %+v", s2)
			}
			recordMeasurement(t, "stage4_publication_with_stray_child_seconds", time.Since(start).Seconds())
			noStrayProcesses(t)
			active = s2.release()
			waitManifest(t, active)
			assertRegistryConsistent(t)
		})
	})

	t.Run("a blocked post-import SQL statement is cancelled at the publication deadline", func(t *testing.T) {
		t.Cleanup(func() { restartPublisher(t, nil) })
		restartPublisher(t, map[string]string{"KARTA_TEST_OSM2PGSQL": "/usr/local/bin/slow-osm2pgsql", "KARTA_TEST_PUBLISH_TIMEOUT": "35s"})
		data := variant(t, at("2026-05-02T00:00:00Z"), nil, nil)
		authorize(t, data, "integration test: blocked SQL")
		submit(t, "blocked-sql", data, nil, "")
		waitJobPhase(t, "building", 30*time.Second)
		cand := candidateName(t)
		// While osm2pgsql is still delayed, hold an exclusive lock on the
		// table the post-import SQL reads first (020_clip.sql).
		// (As the importer role: only the importer's own sessions use a
		// candidate, and it may end them when it drops the candidate.)
		ctx := context.Background()
		lock := asRole(t, "karta_importer", "db_importer_password", cand)
		defer lock.Close(ctx)
		deadline := time.Now().Add(15 * time.Second)
		for {
			var ok bool
			if err := lock.QueryRow(ctx, `SELECT to_regclass('karta.release_params') IS NOT NULL`).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the candidate's setup did not run")
			}
			time.Sleep(100 * time.Millisecond)
		}
		tx, err := lock.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `LOCK TABLE karta.release_params IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		// The post-import SQL waits for the lock until the deadline.
		pg := superuser(t, "postgres")
		defer pg.Close(ctx)
		blocked := false
		for until := time.Now().Add(40 * time.Second); time.Now().Before(until) && !blocked; time.Sleep(250 * time.Millisecond) {
			if err := pg.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = $1 AND wait_event_type = 'Lock'
                AND application_name = 'karta-importer')`, cand).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
		}
		if !blocked {
			t.Fatal("the post-import SQL never waited for the lock")
		}
		s := waitSubmission(t, "blocked-sql", 90*time.Second)
		if s.State != "failed" || s.code() != "publication_timeout" || !strings.Contains(strOr(s.Reason), "KARTA_PUBLISH_TIMEOUT (35s)") {
			t.Fatalf("submission %+v", s)
		}
		_ = tx.Rollback(ctx) // the candidate was dropped WITH (FORCE): this session is gone
		if n := candidates(t); n != 0 {
			t.Errorf("%d candidate database(s) left", n)
		}
		var waiting int
		if err := pg.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1`, cand).Scan(&waiting); err != nil || waiting != 0 {
			t.Errorf("sessions left on the dropped candidate: %d %v", waiting, err)
		}
		if got := activeFromRegistry(t); got != active {
			t.Fatalf("active %s, want %s", got, active)
		}
		expectStatus(t, get(t, "/v1/releases/"+active+lakeTile), http.StatusOK)
		assertRegistryConsistent(t)
	})

	t.Run("a shutdown during an import records it interrupted, and the next start retries it", func(t *testing.T) {
		t.Cleanup(func() { restartPublisher(t, nil) })
		restartPublisher(t, map[string]string{"KARTA_TEST_OSM2PGSQL": "/usr/local/bin/slow-osm2pgsql"})
		data := variant(t, at("2026-05-03T00:00:00Z"), nil, nil)
		authorize(t, data, "integration test: shutdown during import")
		submit(t, "shutdown", data, nil, "")
		waitJobPhase(t, "building", 30*time.Second)
		waitCandidate(t)
		waitProcess(t, "slow-osm2pgsql", 10*time.Second)
		start := time.Now()
		if _, stderr, code := compose(t, "stop", "publisher"); code != 0 {
			t.Fatalf("stop: %s", stderr)
		}
		if code := waitPublisherExit(t, 60*time.Second); code != 0 {
			t.Fatalf("publisher exit %d after SIGTERM, want a clean 0", code)
		}
		if d := time.Since(start); d > 20*time.Second {
			t.Errorf("graceful stop took %s (Docker would have killed it at 30s)", d)
		}
		ctx := context.Background()
		reg := superuser(t, "karta_registry")
		defer reg.Close(ctx)
		var state, code string
		if err := reg.QueryRow(ctx, `SELECT state, coalesce(reason_code, '') FROM registry.submissions WHERE name = 'shutdown' ORDER BY id DESC LIMIT 1`).
			Scan(&state, &code); err != nil {
			t.Fatal(err)
		}
		if state != "interrupted" || code != "interrupted" {
			t.Fatalf("after a shutdown during osm2pgsql: %s/%s, want interrupted (retried), not a final failure", state, code)
		}
		if n := candidates(t); n != 0 {
			t.Errorf("%d candidate database(s) left after the shutdown", n)
		}
		restartPublisher(t, nil)
		s := waitSubmission(t, "shutdown", 90*time.Second)
		if s.State != "published" || s.Attempts != 2 {
			t.Fatalf("after restart: %+v", s)
		}
		active = s.release()
		waitManifest(t, active)
		assertRegistryConsistent(t)
	})

	t.Run("the command-line import has the same deadline and exit code 8", func(t *testing.T) {
		data := variant(t, at("2026-05-04T00:00:00Z"), nil, nil)
		local := filepath.Join(repoRoot, "data", "local", "stage4-cli-timeout.osm.pbf")
		if err := os.WriteFile(local, data, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(local) })
		authorize(t, data, "integration test: command-line deadline")
		out, errOut, code := compose(t, "run", "--rm", "-T", "-e", "KARTA_OSM2PGSQL=/usr/local/bin/slow-osm2pgsql", "-e", "KARTA_PUBLISH_TIMEOUT=5s",
			"importer", "--snapshot", "/data/local/stage4-cli-timeout.osm.pbf", "--region", "/config/regions/fixture.json")
		if code != 8 || !strings.Contains(out, `"reason_code": "publication_timeout"`) {
			t.Fatalf("exit %d, want 8\n%s\n%s", code, out, errOut)
		}
		if n := candidates(t); n != 0 {
			t.Errorf("%d candidate database(s) left", n)
		}
		if got := activeFromRegistry(t); got != active {
			t.Fatalf("active %s, want %s", got, active)
		}
		assertRegistryConsistent(t)
	})

	t.Run("database passwords rotate without a restart or a failed request; a rotated monitoring token reaches the metrics listener", func(t *testing.T) {
		apiBefore, pubBefore := containerStart(t, "api"), containerStart(t, "publisher")
		oldAPI := secret(t, "db_api_password")
		traffic := startLoad(2)
		for _, role := range []string{"api", "importer"} {
			if out, code := script(t, "rotate-db-password.sh", role); code != 0 {
				t.Fatalf("rotate %s: %s", role, out)
			}
		}
		if secret(t, "db_api_password") == oldAPI {
			t.Fatal("the api password file did not change")
		}
		// New connections use the new passwords: the publisher connects anew
		// for a build, and the API opens a pool for the new release.
		active = publishVariant(t, "after-rotation", "2026-05-05T00:00:00Z")
		traffic.finish(t, "during password rotation")
		if _, err := pgx.Connect(context.Background(), "host=127.0.0.1 port="+dbPort+" user=karta_api dbname=karta_registry sslmode=disable password="+oldAPI); err == nil ||
			!strings.Contains(err.Error(), "28P01") {
			t.Errorf("the old api password still logs in: %v", err)
		}
		if containerStart(t, "api") != apiBefore || containerStart(t, "publisher") != pubBefore {
			t.Error("a service was restarted by the password rotation")
		}

		// The monitoring token: secrets/metrics_tokens is rewritten in place
		// and the API's metrics listener picks it up without a restart.
		oldMon := secret(t, "operator_monitor_token")
		if err := os.Remove(filepath.Join(repoRoot, "secrets", "operator_monitor_token")); err != nil {
			t.Fatal(err)
		}
		gen := exec.Command("./scripts/gen-secrets.sh")
		gen.Dir = repoRoot
		if out, err := gen.CombinedOutput(); err != nil {
			t.Fatalf("gen-secrets: %v %s", err, out)
		}
		newMon := secret(t, "operator_monitor_token")
		if got := metricsStatus(t, newMon); got != 200 {
			t.Errorf("the new monitoring token: %d", got)
		}
		if got := metricsStatus(t, oldMon); got != 401 {
			t.Errorf("the old monitoring token: %d", got)
		}
		if containerStart(t, "api") != apiBefore {
			t.Error("the API was restarted by the token rotation")
		}
		// The publisher reads its credentials at start (make
		// rotate-operator-tokens recreates it).
		restartPublisher(t, nil)
		status(t)
		if r := op(t, http.MethodGet, "/v1/operator/status", newMon, nil); r.status != 200 {
			t.Errorf("the publisher refuses the new monitoring token: %d", r.status)
		}
		if r := op(t, http.MethodGet, "/v1/operator/status", oldMon, nil); r.status != 401 {
			t.Errorf("the publisher accepts the revoked monitoring token: %d", r.status)
		}
	})

	t.Run("a backup restores the registry, every retained release, the audit history, the anti-replay state and the outbox", func(t *testing.T) {
		ctx := context.Background()
		st := status(t)
		retained := map[string]string{}
		for _, r := range st.Releases {
			if r.State == "active" || r.State == "ready" || r.State == "retired" {
				retained[r.ID] = r.State
			}
		}
		if len(retained) < 3 {
			t.Fatalf("want an active and retained releases to back up, have %v", retained)
		}
		// A verified manifest serial (the online anti-replay floor) and a
		// fetcher state that verified a later, undelivered serial.
		reg := superuser(t, "karta_registry")
		if _, err := reg.Exec(ctx, `INSERT INTO registry.source_state (region_id, last_serial, envelope_sha256, snapshot_sha256, snapshot_size,
            data_timestamp, issued_at, expires_at, key_id) VALUES ('fixture', 42, repeat('a', 64), repeat('b', 64), 1, now(), now(), now() + interval '1 day', 'test')
            ON CONFLICT (region_id) DO UPDATE SET last_serial = 42`); err != nil {
			t.Fatal(err)
		}
		reg.Close(ctx)
		stateDir := filepath.Join(onlineDir, ".fetcher")
		t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "state.json"),
			[]byte(`{"version":1,"updated_at":"2026-10-01T00:00:00Z","source":"test","consecutive_failures":0,"highest_serial":45}`), 0o644); err != nil {
			t.Fatal(err)
		}

		dest := t.TempDir()
		start := time.Now()
		out, code := script(t, "backup.sh", dest)
		if code != 0 {
			t.Fatalf("backup: %s", out)
		}
		recordMeasurement(t, "stage4_backup_seconds", time.Since(start).Seconds())
		entries, _ := os.ReadDir(dest)
		if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "karta-") {
			t.Fatalf("backup directory: %v", entries)
		}
		dir := filepath.Join(dest, entries[0].Name())
		for _, f := range []string{"MANIFEST", "SHA256SUMS", "db/base.tar.gz", "db/pg_wal.tar.gz", "db/backup_manifest",
			"registry.before.json", "registry.after.json", "online.tar", "config/regions/fixture.json"} {
			if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
				t.Errorf("backup lacks %s", f)
			}
		}
		if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
			t.Errorf("backup directory mode %v", fi.Mode().Perm())
		}
		// No secret value is in the backup's readable files (the database
		// holds only password verifiers).
		var plain []byte
		_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() && !strings.HasSuffix(p, ".gz") && !strings.HasSuffix(p, ".tar") {
				b, _ := os.ReadFile(p)
				plain = append(plain, b...)
			}
			return nil
		})
		for _, sec := range []string{"db_superuser_password", "db_importer_password", "db_api_password", "operator_token", "operator_monitor_token"} {
			if strings.Contains(string(plain), secret(t, sec)) {
				t.Errorf("the backup contains the value of %s", sec)
			}
		}
		var after struct {
			ActiveRelease string           `json:"active_release_id"`
			SourceSerials map[string]int64 `json:"source_serials"`
			AuditMaxID    int64            `json:"audit_max_id"`
		}
		b, _ := os.ReadFile(filepath.Join(dir, "registry.after.json"))
		if err := json.Unmarshal(b, &after); err != nil || after.ActiveRelease != active || after.SourceSerials["fixture"] != 42 {
			t.Fatalf("registry summary %s: %v", b, err)
		}

		// A publication after the backup is not in it: the recovery point
		// is the backup. Its inbox files are removed, as an operator would
		// before restoring (they would be submitted again).
		lost := publishVariant(t, "after-backup", "2026-05-06T00:00:00Z")
		for _, ext := range []string{".osm.pbf", ".osm.pbf.ready"} {
			_ = os.Remove(filepath.Join(inboxDir, "after-backup"+ext))
		}

		// The disaster: the database and the outbox are gone.
		compose(t, "stop", "api", "publisher")
		compose(t, "rm", "-s", "-f", "db")
		if out, err := exec.Command("docker", "volume", "rm", "karta-test_pgdata").CombinedOutput(); err != nil {
			t.Fatalf("remove the database volume: %v %s", err, out)
		}
		_ = os.RemoveAll(stateDir)

		t.Setenv("KARTA_RESTORE_REPORTS", t.TempDir())
		start = time.Now()
		out, code = script(t, "restore.sh", dir)
		if code != 0 {
			t.Fatalf("restore: %s", out)
		}
		recordMeasurement(t, "stage4_restore_seconds", time.Since(start).Seconds())
		t.Logf("restore:\n%s", out)
		waitReady(t, true, "ready")
		waitPublisher(t)
		waitManifest(t, active)
		st = status(t)
		for id, state := range retained {
			if r := st.release(id); r == nil || r.State != state {
				t.Errorf("release %s: %+v, backed up as %s", id, r, state)
			}
		}
		if st.release(lost) != nil {
			t.Errorf("the release published after the backup is present")
		}
		reg = superuser(t, "karta_registry")
		defer reg.Close(ctx)
		var serial int64
		var auto bool
		if err := reg.QueryRow(ctx, `SELECT last_serial FROM registry.source_state WHERE region_id = 'fixture'`).Scan(&serial); err != nil || serial != 42 {
			t.Errorf("anti-replay serial %d %v", serial, err)
		}
		if err := reg.QueryRow(ctx, `SELECT auto_activate FROM registry.online_policy WHERE region_id = 'fixture'`).Scan(&auto); err != nil || auto {
			t.Errorf("online activation not paused after the restore: %v %v", auto, err)
		}
		var restores, maxID int64
		if err := reg.QueryRow(ctx, `SELECT count(*) FILTER (WHERE action = 'restore' AND target = $1), max(id) FROM registry.audit`, active).
			Scan(&restores, &maxID); err != nil || restores != 1 || maxID <= after.AuditMaxID {
			t.Errorf("audit after the restore: %d restore records, max id %d (backup %d), %v", restores, maxID, after.AuditMaxID, err)
		}
		if b, err := os.ReadFile(filepath.Join(stateDir, "state.json")); err != nil || !strings.Contains(string(b), `"highest_serial":45`) {
			t.Errorf("fetcher state not restored: %s %v", b, err)
		}
		if !strings.Contains(out, "verified serial 45, above the registry's 42") {
			t.Errorf("the undelivered verified serial is not reported:\n%s", out)
		}
		// Read-only serving and every credential.
		rel := asRole(t, "karta_importer", "db_importer_password", "karta_"+active)
		if _, err := rel.Exec(ctx, `CREATE TABLE karta.x (i int)`); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("the restored release database accepts writes: %v", err)
		}
		rel.Close(ctx)
		expectStatus(t, get(t, "/v1/releases/"+active+lakeTile), http.StatusOK)
		if r := op(t, http.MethodPost, "/v1/operator/cleanup", secret(t, "operator_token"), map[string]any{"reason": "after restore", "dry_run": true}); r.status != 200 {
			t.Errorf("operator token after the restore: %d", r.status)
		}
		if got := metricsStatus(t, secret(t, "operator_monitor_token")); got != 200 {
			t.Errorf("monitoring token after the restore: %d", got)
		}
		assertRegistryConsistent(t)

		t.Run("a failed upgrade is undone by restoring the backup taken before it", func(t *testing.T) {
			dest := t.TempDir()
			if out, code := script(t, "backup.sh", dest); code != 0 {
				t.Fatalf("backup: %s", out)
			}
			entries, _ := os.ReadDir(dest)
			pre := filepath.Join(dest, entries[0].Name())
			// The "upgrade" left the registry at a schema this build cannot
			// run: the publisher refuses it, the API keeps serving.
			reg := superuser(t, "karta_registry")
			if _, err := reg.Exec(ctx, `INSERT INTO registry.schema_migrations (version) VALUES (99)`); err != nil {
				t.Fatal(err)
			}
			reg.Close(ctx)
			if _, stderr, code := compose(t, "up", "-d", "--no-deps", "--force-recreate", "publisher"); code != 0 {
				t.Fatalf("recreate publisher: %s", stderr)
			}
			deadline := time.Now().Add(60 * time.Second)
			for {
				logs, _, _ := compose(t, "logs", "--no-color", "publisher")
				if strings.Contains(logs, "newer than this build supports") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the publisher accepted a newer registry:\n%s", logs)
				}
				time.Sleep(time.Second)
			}
			expectStatus(t, get(t, "/v1/releases/"+active+lakeTile), http.StatusOK)

			// Refused before anything changes: a restore over a database
			// without --replace, a damaged file, and a damaged base backup
			// whose SHA256SUMS was rewritten to match.
			if out, code := script(t, "restore.sh", pre); code == 0 || !strings.Contains(out, "holds a database; add --replace") {
				t.Errorf("restore over a database without --replace: %d %s", code, out)
			}
			damaged := filepath.Join(t.TempDir(), "damaged")
			if out, err := exec.Command("cp", "-a", pre, damaged).CombinedOutput(); err != nil {
				t.Fatalf("copy backup: %v %s", err, out)
			}
			base := filepath.Join(damaged, "db", "base.tar.gz")
			b, err := os.ReadFile(base)
			if err != nil {
				t.Fatal(err)
			}
			b[len(b)/2] ^= 0xff
			if err := os.WriteFile(base, b, 0o600); err != nil {
				t.Fatal(err)
			}
			if out, code := script(t, "restore.sh", damaged, "--replace"); code == 0 || !strings.Contains(out, "do not match SHA256SUMS") {
				t.Errorf("restore of a damaged backup: %d %s", code, out)
			}
			if out, err := exec.Command("sh", "-c", `cd "$1" && find . -type f ! -name SHA256SUMS | sort | xargs sha256sum > SHA256SUMS`, "sh", damaged).CombinedOutput(); err != nil {
				t.Fatalf("rewrite SHA256SUMS: %v %s", err, out)
			}
			if out, code := script(t, "restore.sh", damaged, "--replace"); code == 0 || !strings.Contains(out, "pg_verifybackup refused") {
				t.Errorf("restore of a damaged base backup: %d %s", code, out)
			}
			expectStatus(t, get(t, "/v1/releases/"+active+lakeTile), http.StatusOK)

			if out, code := script(t, "restore.sh", pre, "--replace"); code != 0 {
				t.Fatalf("restore: %s", out)
			}
			waitReady(t, true, "ready")
			waitPublisher(t)
			waitManifest(t, active)
			assertRegistryConsistent(t)
		})
	})

	// Last: it stops the database.
	t.Run("the monitoring profile scrapes every target with the monitoring credential; alerts fire and clear", func(t *testing.T) {
		// The API's metrics listener: the monitoring credential only.
		monitor := secret(t, "operator_monitor_token")
		for token, want := range map[string]int{"": 401, strings.Repeat("0", 64): 401, monitor: 200, secret(t, "operator_token"): 401} {
			if got := metricsStatus(t, token); got != want {
				t.Errorf("API metrics with token %.6s: %d, want %d", token, got, want)
			}
		}
		if r := get(t, "/metrics"); r.status != http.StatusNotFound {
			t.Errorf("the public listener answers /metrics with %d", r.status)
		}

		role := exec.Command("./scripts/create-monitor-role.sh")
		role.Dir = repoRoot
		role.Env = append(os.Environ(), "COMPOSE="+composeCmd)
		if out, err := role.CombinedOutput(); err != nil {
			t.Fatalf("monitor role: %v %s", err, out)
		}
		if _, stderr, code := compose(t, "--profile", "monitoring", "up", "-d", "postgres-exporter", "prometheus"); code != 0 {
			t.Fatalf("monitoring profile: %s", stderr)
		}
		t.Cleanup(func() {
			compose(t, "--profile", "monitoring", "rm", "-s", "-f", "-v", "postgres-exporter", "prometheus")
		})
		waitPrometheus(t)

		// Every target up, with the credential it needs.
		deadline := time.Now().Add(120 * time.Second)
		for {
			up := map[string]bool{}
			for _, r := range promQuery(t, `up`) {
				m := r["metric"].(map[string]any)
				v := r["value"].([]any)
				if v[1] == "1" {
					up[m["job"].(string)] = true
				}
			}
			if up["karta-api"] && up["karta-publisher"] && up["karta-postgres"] {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("targets up: %v", up)
			}
			time.Sleep(2 * time.Second)
		}
		// The rules load and evaluate without errors.
		resp, err := client.Get(promBase + "/api/v1/rules")
		if err != nil {
			t.Fatal(err)
		}
		var rules struct {
			Data struct {
				Groups []struct {
					Name  string `json:"name"`
					Rules []struct {
						Name      string `json:"name"`
						Health    string `json:"health"`
						LastError string `json:"lastError"`
					} `json:"rules"`
				} `json:"groups"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&rules); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		n := 0
		for _, g := range rules.Data.Groups {
			for _, r := range g.Rules {
				n++
				if r.LastError != "" || r.Health == "err" {
					t.Errorf("rule %s/%s: %s %s", g.Name, r.Name, r.Health, r.LastError)
				}
			}
		}
		if n < 30 {
			t.Errorf("only %d rules loaded", n)
		}
		// The series the rules read exist in a running stack (online and host
		// series aside: online updates are off here, and node_exporter and
		// cAdvisor are the operator's host agents).
		for _, name := range []string{"karta_api_ready", "karta_api_readiness_info", "karta_api_requests_total",
			"karta_api_request_duration_seconds_bucket", "karta_api_start_time_seconds", "karta_active_release",
			"karta_publication_timeouts_total", "karta_publication_timeout_seconds", "karta_submissions",
			"karta_release_storage_bytes", "karta_release_storage_budget_bytes", "karta_publisher_start_time_seconds",
			"pg_up", "pg_wal_size_bytes", "pg_settings_max_wal_size_bytes",
			"karta:guard_disk_free_ratio", "karta:guard_publication_overrun_seconds"} {
			if len(promQuery(t, `count(`+name+`)`)) == 0 {
				t.Errorf("no %s series", name)
			}
		}

		// A database outage: the API reports it and the alerts fire; when the
		// database returns they clear.
		if _, stderr, code := compose(t, "stop", "db"); code != 0 {
			t.Fatalf("stop db: %s", stderr)
		}
		dbStopped := true
		defer func() {
			if dbStopped {
				compose(t, "start", "db")
			}
		}()
		start := time.Now()
		for want := []string{"KartaAPINotReady", "KartaPostgresDown"}; ; time.Sleep(5 * time.Second) {
			f := firing(t)
			if f[want[0]] && f[want[1]] {
				recordMeasurement(t, "stage4_alerts_fired_after_db_stop_seconds", time.Since(start).Seconds())
				break
			}
			if time.Since(start) > 5*time.Minute {
				t.Fatalf("firing after the database stopped: %v", f)
			}
		}
		if _, stderr, code := compose(t, "start", "db"); code != 0 {
			t.Fatalf("start db: %s", stderr)
		}
		dbStopped = false
		waitReady(t, true, "ready")
		start = time.Now()
		for ; ; time.Sleep(5 * time.Second) {
			f := firing(t)
			if !f["KartaAPINotReady"] && !f["KartaPostgresDown"] {
				recordMeasurement(t, "stage4_alerts_cleared_after_db_start_seconds", time.Since(start).Seconds())
				break
			}
			if time.Since(start) > 3*time.Minute {
				t.Fatalf("still firing after the database returned: %v", f)
			}
		}
		waitPublisher(t)
	})
}
