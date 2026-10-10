//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The content policy (the region file's min_counts, searches and tiles) is
// bound to a release. Thresholds are not part of a release id, so a
// policy-only edit of the region file keeps a ready release: it must have
// passed the policy in force before any forward activation, and a
// resubmission evaluates it again without a rebuild. This is the Iran
// procedure (draft region with empty checks, import with --no-activate, set
// the checks, activate) on the fixture.

type cliImport struct {
	ReleaseID string `json:"release_id"`
	Outcome   struct {
		State      string `json:"state"`
		Code       string `json:"reason_code"`
		Reason     string `json:"reason"`
		Validation *struct {
			Passed       bool   `json:"passed"`
			PolicySHA256 string `json:"policy_sha256"`
			CountsFrom   string `json:"counts_from"`
			Checks       []struct {
				Name   string `json:"name"`
				Passed bool   `json:"passed"`
			} `json:"checks"`
		} `json:"validation"`
	} `json:"outcome"`
	Report *struct {
		ValidationPolicy *struct {
			SHA256 string `json:"sha256"`
		} `json:"validation_policy"`
	} `json:"report"`
}

type policyRelease struct {
	ID               string  `json:"release_id"`
	State            string  `json:"state"`
	Passed           *string `json:"validation_policy_sha256"`
	PolicyCurrent    bool    `json:"validation_policy_current"`
	RollbackEligible bool    `json:"rollback_eligible"`
	Validation       *struct {
		PolicySHA256 string   `json:"policy_sha256"`
		Passed       bool     `json:"passed"`
		Kind         string   `json:"kind"`
		Actor        string   `json:"actor"`
		Source       string   `json:"source"`
		Failed       []string `json:"failed"`
	} `json:"validation"`
}

type policyStatus struct {
	Region struct {
		Policy string `json:"validation_policy_sha256"`
	} `json:"region"`
	Active *struct {
		ID string `json:"release_id"`
	} `json:"active"`
	Releases []policyRelease `json:"releases"`
	Job      *struct {
		Phase string `json:"phase"`
	} `json:"job"`
}

func policyView(t *testing.T) policyStatus {
	t.Helper()
	r := op(t, http.MethodGet, "/v1/operator/status", secret(t, "operator_monitor_token"), nil)
	expectStatus(t, r, http.StatusOK)
	var s policyStatus
	r.json(t, &s)
	return s
}

func (s policyStatus) release(t *testing.T, id string) policyRelease {
	t.Helper()
	for _, r := range s.Releases {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no release %s in the status", id)
	return policyRelease{}
}

func (s policyStatus) activeID() string {
	if s.Active == nil {
		return ""
	}
	return s.Active.ID
}

// auditCount counts registry audit records of an action and outcome on a
// target.
func auditCount(t *testing.T, action, outcome, target string) int {
	t.Helper()
	ctx := context.Background()
	reg := superuser(t, "karta_registry")
	defer reg.Close(ctx)
	var n int
	if err := reg.QueryRow(ctx, `SELECT count(*) FROM registry.audit WHERE action = $1 AND outcome = $2 AND target = $3`, action, outcome, target).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func releaseDatabases(t *testing.T) int {
	t.Helper()
	n := 0
	for name := range databases(t) {
		if strings.HasPrefix(name, "karta_r") {
			n++
		}
	}
	return n
}

func TestReadyReleasePolicy(t *testing.T) {
	regionDir := os.Getenv("KARTA_TEST_REGION_DIR")
	if regionDir == "" {
		t.Fatal("KARTA_TEST_REGION_DIR is not set; run `make test-integration`")
	}
	admin := secret(t, "operator_token")
	var doc map[string]any
	if err := json.Unmarshal(readRepo(t, "config/regions/fixture.json"), &doc); err != nil {
		t.Fatal(err)
	}
	strict := doc["validation"].(map[string]any)
	empty := map[string]any{"min_counts": map[string]any{}, "search": []any{}, "tiles": []any{}}
	failing := map[string]any{}
	for k, v := range strict {
		failing[k] = v
	}
	minCounts := map[string]any{}
	for k, v := range strict["min_counts"].(map[string]any) {
		minCounts[k] = v
	}
	minCounts["roads"] = 1_000_000_000
	failing["min_counts"] = minCounts
	// writeRegionAs writes the fixture region with these checks, edited by
	// edit (if any) after they are set.
	writeRegionAs := func(t *testing.T, validation map[string]any, edit func(map[string]any)) string {
		t.Helper()
		d := map[string]any{}
		for k, v := range doc {
			d[k] = v
		}
		d["validation"] = validation
		if edit != nil {
			edit(d)
		}
		b, _ := json.MarshalIndent(d, "", "  ")
		p := filepath.Join(regionDir, "fixture.json")
		if err := os.WriteFile(p+".tmp", b, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p+".tmp", 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(p+".tmp", p); err != nil {
			t.Fatal(err)
		}
		return policyView(t).Region.Policy
	}
	writeRegion := func(t *testing.T, validation map[string]any) string {
		t.Helper()
		return writeRegionAs(t, validation, nil)
	}
	// Edits of the region identity alone (the checks unchanged): the name,
	// the box, the view centre and the view zoom.
	view := doc["view"].(map[string]any)
	center := view["center"].([]any)
	bbox := doc["bbox"].([]any)
	identityEdits := []struct {
		name string
		edit func(map[string]any)
	}{
		{"name", func(d map[string]any) { d["name"] = doc["name"].(string) + " (renamed)" }},
		{"bbox", func(d map[string]any) { d["bbox"] = []any{bbox[0].(float64) - 1e-6, bbox[1], bbox[2], bbox[3]} }},
		{"view centre", func(d map[string]any) {
			d["view"] = map[string]any{"center": []any{center[0].(float64) + 1e-6, center[1]}, "zoom": view["zoom"]}
		}},
		{"view zoom", func(d map[string]any) {
			d["view"] = map[string]any{"center": center, "zoom": view["zoom"].(float64) - 1}
		}},
	}
	importFixture := func(t *testing.T, snapshot string, env []string, flags ...string) (cliImport, int, string) {
		t.Helper()
		args := []string{"run", "--rm", "-T"}
		for _, e := range env {
			args = append(args, "-e", e)
		}
		args = append(args, "-v", regionDir+":/config/test-regions:ro", "importer", "--snapshot", snapshot,
			"--region", "/config/test-regions/fixture.json")
		out, errOut, code := compose(t, append(args, flags...)...)
		var res cliImport
		if strings.TrimSpace(out) != "" {
			if err := json.Unmarshal([]byte(out), &res); err != nil {
				t.Fatalf("import output is not JSON: %v\n%s\n%s", err, out, errOut)
			}
		}
		return res, code, errOut
	}
	const fixture = "/data/testdata/fixture/karta-fixture.osm"
	usePolicyFile := map[string]string{"KARTA_TEST_REGION_FILE": "/config/test-regions/fixture.json"}

	resetAll(t)
	t.Cleanup(func() { resetAll(t) })
	writeRegion(t, empty)
	restartPublisher(t, usePolicyFile)
	pEmpty := policyView(t).Region.Policy

	var rel string
	t.Run("a release built under the empty draft policy is bound to it", func(t *testing.T) {
		res, code, stderr := importFixture(t, fixture, nil, "--no-activate")
		if code != 0 || res.Outcome.State != "ready" || res.Outcome.Code != "manual_activation" || res.Report == nil ||
			res.Report.ValidationPolicy == nil || res.Report.ValidationPolicy.SHA256 != pEmpty {
			t.Fatalf("exit %d %+v\n%s", code, res, stderr)
		}
		rel = res.ReleaseID
		r := policyView(t).release(t, rel)
		if r.State != "ready" || r.Passed == nil || *r.Passed != pEmpty || !r.PolicyCurrent || r.Validation == nil ||
			r.Validation.Kind != "import" || !r.Validation.Passed {
			t.Fatalf("release %+v", r)
		}
	})
	if rel == "" {
		t.FailNow()
	}

	var pFailing string
	t.Run("a policy-only edit keeps the release and blocks its activation", func(t *testing.T) {
		pFailing = writeRegion(t, failing)
		s := policyView(t)
		if pFailing == pEmpty || s.release(t, rel).PolicyCurrent {
			t.Fatalf("policy %s (was %s), release %+v", pFailing, pEmpty, s.release(t, rel))
		}
		r := op(t, http.MethodPost, "/v1/operator/releases/"+rel+"/activate", admin, map[string]any{"reason": "checks were set after the import"})
		if r.status != http.StatusConflict || r.reasonCode(t) != "validation_required" {
			t.Fatalf("activation under a policy it never passed: %d %s", r.status, r.body)
		}
		if s := policyView(t); s.activeID() != "" || s.release(t, rel).State != "ready" {
			t.Fatalf("after the refused activation: active %q, %+v", s.activeID(), s.release(t, rel))
		}
		// Not a back door either: a rollback only returns to a release that
		// was active before.
		r = op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"reason": "rollback to an unvalidated release", "release_id": rel})
		if code, _ := r.errorCode(t); r.status != http.StatusConflict || code != "release_not_eligible" {
			t.Fatalf("rollback to a release never activated: %d %s", r.status, r.body)
		}
	})

	t.Run("a failed revalidation is recorded and changes nothing else", func(t *testing.T) {
		r := op(t, http.MethodPost, "/v1/operator/releases/"+rel+"/revalidate", admin, map[string]any{"reason": "checks set from the measured import"})
		expectStatus(t, r, http.StatusOK)
		var res struct {
			Result struct {
				Passed       bool   `json:"passed"`
				PolicySHA256 string `json:"policy_sha256"`
				CountsFrom   string `json:"counts_from"`
				Checks       []struct {
					Name   string `json:"name"`
					Passed bool   `json:"passed"`
				} `json:"checks"`
			} `json:"result"`
		}
		r.json(t, &res)
		failed := []string{}
		for _, c := range res.Result.Checks {
			if !c.Passed {
				failed = append(failed, c.Name)
			}
		}
		if res.Result.Passed || res.Result.PolicySHA256 != pFailing || res.Result.CountsFrom != "registry" ||
			strings.Join(failed, ",") != "min_count_roads" || len(res.Result.Checks) < 5 {
			t.Fatalf("revalidation %+v", res.Result)
		}
		got := policyView(t).release(t, rel)
		// It still passed only the empty policy; the failure is recorded,
		// attributed and audited.
		if got.Passed == nil || *got.Passed != pEmpty || got.PolicyCurrent || got.Validation == nil || got.Validation.Passed ||
			got.Validation.Kind != "revalidation" || got.Validation.Actor != "operator" || got.Validation.PolicySHA256 != pFailing ||
			len(got.Validation.Failed) != 1 || got.State != "ready" {
			t.Fatalf("after the failed revalidation: %+v %+v", got, got.Validation)
		}
		if n := auditCount(t, "release_revalidated", "failed", rel); n != 1 {
			t.Fatalf("%d failed revalidations audited", n)
		}
		r = op(t, http.MethodPost, "/v1/operator/releases/"+rel+"/activate", admin, map[string]any{"reason": "try anyway"})
		if r.status != http.StatusConflict || r.reasonCode(t) != "validation_required" {
			t.Fatalf("activation after a failed revalidation: %d %s", r.status, r.body)
		}
	})

	t.Run("one evaluation of a release at a time across processes; one cut short by the deadline records nothing", func(t *testing.T) {
		ctx := context.Background()
		const deadline = 20 * time.Second
		restartPublisher(t, map[string]string{"KARTA_TEST_REGION_FILE": "/config/test-regions/fixture.json",
			"KARTA_TEST_OPERATOR_REQUEST_TIMEOUT": deadline.String()})
		defer restartPublisher(t, usePolicyFile)
		before := policyView(t).release(t, rel)
		records := func() int {
			return auditCount(t, "release_revalidated", "succeeded", rel) + auditCount(t, "release_revalidated", "failed", rel)
		}
		recorded, interruptedBefore, refusedBefore := records(), auditCount(t, "revalidate", "failed", rel), auditCount(t, "revalidate", "rejected", rel)

		// An exclusive lock on the release's metadata makes the evaluation
		// wait in the database, as a slow query would.
		blocker := superuser(t, "karta_"+rel)
		defer blocker.Close(ctx)
		tx, err := blocker.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `LOCK TABLE karta.release_info IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		pg := superuser(t, "postgres")
		defer pg.Close(ctx)
		waiting := func() int {
			var n int
			if err := pg.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND wait_event_type = 'Lock'`,
				"karta_"+rel).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}

		type reply struct {
			status int
			body   string
			took   time.Duration
		}
		first := make(chan reply, 1)
		start := time.Now()
		go func() {
			b, _ := json.Marshal(map[string]any{"reason": "an evaluation that outlasts the request deadline"})
			req, _ := http.NewRequest(http.MethodPost, opBase+"/v1/operator/releases/"+rel+"/revalidate", bytes.NewReader(b))
			req.Header.Set("Authorization", "Bearer "+admin)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				first <- reply{0, err.Error(), time.Since(start)}
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			first <- reply{resp.StatusCode, string(body), time.Since(start)}
		}()
		for waiting() == 0 {
			if time.Since(start) > deadline/2 {
				t.Fatal("the evaluation never waited in the release database")
			}
			time.Sleep(100 * time.Millisecond)
		}

		// A second evaluation through the API is refused at once.
		t0 := time.Now()
		r := op(t, http.MethodPost, "/v1/operator/releases/"+rel+"/revalidate", admin, map[string]any{"reason": "a second evaluation"})
		if code, _ := r.errorCode(t); r.status != http.StatusConflict || code != "revalidation_in_progress" || r.header.Get("Retry-After") == "" ||
			time.Since(t0) > 5*time.Second {
			t.Fatalf("a second evaluation: %d %s after %v", r.status, r.body, time.Since(t0))
		}
		t.Logf("a second evaluation through the API: %d revalidation_in_progress after %v", r.status, time.Since(t0).Round(time.Millisecond))
		// So is one from another process: a command-line resubmission is
		// interrupted, without a build.
		dbs := releaseDatabases(t)
		t0 = time.Now()
		res, code, stderr := importFixture(t, fixture, nil)
		if code != 7 || res.ReleaseID != rel || res.Outcome.State != "interrupted" || res.Outcome.Code != "revalidation_in_progress" ||
			releaseDatabases(t) != dbs {
			t.Fatalf("a resubmission during the evaluation: exit %d %+v\n%s", code, res, stderr)
		}
		t.Logf("a command-line resubmission: exit %d, %s/%s after %v (container start included)", code, res.Outcome.State, res.Outcome.Code,
			time.Since(t0).Round(time.Millisecond))
		if time.Since(start) > deadline-2*time.Second {
			t.Fatalf("the concurrent attempts took too long (%v) to be checked before the deadline", time.Since(start))
		}

		// The first ends at the request deadline, as a timeout.
		var got reply
		select {
		case got = <-first:
		case <-time.After(2 * deadline):
			t.Fatal("no answer to the first evaluation")
		}
		if got.status != http.StatusServiceUnavailable || !strings.Contains(got.body, `"timeout"`) || got.took < deadline || got.took > deadline+5*time.Second {
			t.Fatalf("the evaluation that outlasted the deadline: %d %s after %v", got.status, got.body, got.took)
		}
		t.Logf("the evaluation waiting in the database: %d timeout after %v (deadline %v)", got.status, got.took.Round(time.Millisecond), deadline)
		// The server stopped its statement too, although the lock it waits
		// for is still held.
		for waiting() != 0 {
			if time.Since(start) > deadline+5*time.Second {
				t.Fatal("the evaluation's statement still runs on the server after the deadline")
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("its statement was gone from the server %v after the request started", time.Since(start).Round(100*time.Millisecond))
		// Nothing was recorded with the release; the attempt is audited.
		after := policyView(t).release(t, rel)
		if records() != recorded || !samePolicy(before.Passed, after.Passed) || after.Validation == nil || before.Validation == nil ||
			!sameValidation(before, after) {
			t.Fatalf("the release changed: %+v %+v, was %+v %+v", after, after.Validation, before, before.Validation)
		}
		if n := auditCount(t, "revalidate", "failed", rel); n != interruptedBefore+1 {
			t.Fatalf("%d interrupted evaluations audited, want %d", n, interruptedBefore+1)
		}
		if n := auditCount(t, "revalidate", "rejected", rel); n != refusedBefore+1 {
			t.Fatalf("%d refused evaluations audited, want %d", n, refusedBefore+1)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}

		// The lock went with the evaluation, and with a process that dies
		// holding it: an evaluation runs again.
		if _, code, stderr := importFixture(t, fixture, []string{"KARTA_FAILPOINTS=revalidate.after_lock"}); code != 99 {
			t.Fatalf("a resubmission stopped holding the lock: exit %d\n%s", code, stderr)
		}
		for i := 0; ; i++ {
			r = op(t, http.MethodPost, "/v1/operator/releases/"+rel+"/revalidate", admin, map[string]any{"reason": "after the stopped process"})
			if code, _ := r.errorCode(t); r.status == http.StatusConflict && code == "revalidation_in_progress" && i < 20 {
				time.Sleep(250 * time.Millisecond)
				continue
			}
			break
		}
		expectStatus(t, r, http.StatusOK)
		var done struct {
			Result struct {
				Duration *float64 `json:"duration_seconds"`
			} `json:"result"`
		}
		r.json(t, &done)
		if records() != recorded+1 || done.Result.Duration == nil || *done.Result.Duration <= 0 || *done.Result.Duration > deadline.Seconds() {
			t.Fatalf("%d evaluations recorded (want %d), duration %v", records(), recorded+1, done.Result.Duration)
		}
		t.Logf("after a process was killed holding the lock: evaluated in %.3f s", *done.Result.Duration)
	})

	t.Run("an evaluation the client abandons records nothing, is audited and releases its lock", func(t *testing.T) {
		ctx := context.Background()
		before := policyView(t).release(t, rel)
		records := func() int {
			return auditCount(t, "release_revalidated", "succeeded", rel) + auditCount(t, "release_revalidated", "failed", rel)
		}
		recorded := records()

		// The evaluation waits in the database, as in the deadline test,
		// but here the client gives up long before the request deadline
		// (the publisher's default, 60 s).
		blocker := superuser(t, "karta_"+rel)
		defer blocker.Close(ctx)
		tx, err := blocker.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `LOCK TABLE karta.release_info IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		pg := superuser(t, "postgres")
		defer pg.Close(ctx)
		waiting := func() int {
			var n int
			if err := pg.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND wait_event_type = 'Lock'`,
				"karta_"+rel).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		// The evaluation's lock (registry.LockRevalidation's key).
		locked := func() int {
			var n int
			if err := pg.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objsubid = 2
    AND classid = x'6b617276'::int::oid AND objid = hashtext($1)::oid AND granted`, rel).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}

		requestID := fmt.Sprintf("client-cancel-%d", time.Now().UnixNano())
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		answered := make(chan error, 1)
		go func() {
			b, _ := json.Marshal(map[string]any{"reason": "an evaluation the client abandons"})
			req, _ := http.NewRequestWithContext(cctx, http.MethodPost, opBase+"/v1/operator/releases/"+rel+"/revalidate", bytes.NewReader(b))
			req.Header.Set("Authorization", "Bearer "+admin)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Request-ID", requestID)
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				err = fmt.Errorf("answered %d before the client gave up", resp.StatusCode)
			}
			answered <- err
		}()
		start := time.Now()
		for waiting() == 0 {
			if time.Since(start) > 15*time.Second {
				t.Fatal("the evaluation never waited in the release database")
			}
			time.Sleep(100 * time.Millisecond)
		}
		if n := locked(); n != 1 {
			t.Fatalf("the evaluation holds %d revalidation locks, want 1", n)
		}
		cancel()
		cancelled := time.Now()
		if err := <-answered; !errors.Is(err, context.Canceled) {
			t.Fatalf("the client: %v", err)
		}
		// The publisher sees the disconnect: its statement stops on the
		// server and its lock is released, well before the deadline.
		for waiting() != 0 || locked() != 0 {
			if time.Since(cancelled) > 10*time.Second {
				t.Fatalf("10 s after the client gave up: %d statements waiting, %d locks held", waiting(), locked())
			}
			time.Sleep(100 * time.Millisecond)
		}
		stopped := time.Since(cancelled)
		// The attempt is audited, although its request context was
		// cancelled before the audit record was written.
		reg := superuser(t, "karta_registry")
		defer reg.Close(ctx)
		var outcome, detail string
		for {
			err := reg.QueryRow(ctx, `SELECT outcome, detail->>'error' FROM registry.audit WHERE action = 'revalidate' AND target = $1 AND request_id = $2`,
				rel, requestID).Scan(&outcome, &detail)
			if err == nil {
				break
			}
			if !errors.Is(err, pgx.ErrNoRows) || time.Since(cancelled) > 15*time.Second {
				t.Fatalf("no audit record of the abandoned evaluation (request %s): %v", requestID, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if outcome != "failed" || !strings.Contains(detail, "interrupted before a result was recorded") || !strings.Contains(detail, "context canceled") {
			t.Fatalf("audit record: %s %q", outcome, detail)
		}
		// No result was recorded with the release.
		var results int
		if err := reg.QueryRow(ctx, `SELECT count(*) FROM registry.audit WHERE action = 'release_revalidated' AND request_id = $1`,
			requestID).Scan(&results); err != nil {
			t.Fatal(err)
		}
		after := policyView(t).release(t, rel)
		if results != 0 || records() != recorded || !samePolicy(before.Passed, after.Passed) || after.Validation == nil || before.Validation == nil ||
			!sameValidation(before, after) {
			t.Fatalf("the release changed: %d results of the request, %d recorded (was %d), %+v %+v", results, records(), recorded, after, after.Validation)
		}
		t.Logf("client gave up after %v; statement and lock gone %v later; audited %s: %s", cancelled.Sub(start).Round(time.Millisecond),
			stopped.Round(100*time.Millisecond), outcome, detail)
	})

	t.Run("a resubmission revalidates without a rebuild and never activates a failing release", func(t *testing.T) {
		dbs := releaseDatabases(t)
		res, code, stderr := importFixture(t, fixture, nil)
		if code != 5 || res.Outcome.State != "failed" || res.Outcome.Code != "validation_failed" || res.ReleaseID != rel ||
			res.Outcome.Validation == nil || res.Outcome.Validation.Passed || !strings.Contains(res.Outcome.Reason, "no rebuild") {
			t.Fatalf("exit %d %+v\n%s", code, res, stderr)
		}
		if s := policyView(t); s.activeID() != "" || s.release(t, rel).State != "ready" {
			t.Fatalf("a failing release was activated: %+v", s)
		}
		if n := auditCount(t, "import_started", "succeeded", rel); n != 1 || releaseDatabases(t) != dbs {
			t.Fatalf("rebuilt: %d imports started, %d release databases (was %d)", n, releaseDatabases(t), dbs)
		}
	})

	var pStrict string
	t.Run("with the measured checks, a no-activate resubmission revalidates and stays ready, even over the storage budget", func(t *testing.T) {
		pStrict = writeRegion(t, strict)
		dbs := releaseDatabases(t)
		// A budget of 1 MiB refuses any new build; reusing the built
		// release needs no room and is not refused.
		res, code, stderr := importFixture(t, fixture, []string{"KARTA_RELEASE_STORAGE_BUDGET_MB=1"}, "--no-activate")
		if code != 0 || res.Outcome.State != "ready" || res.Outcome.Code != "manual_activation" || res.ReleaseID != rel ||
			res.Outcome.Validation == nil || !res.Outcome.Validation.Passed || res.Outcome.Validation.PolicySHA256 != pStrict {
			t.Fatalf("exit %d %+v\n%s", code, res, stderr)
		}
		s := policyView(t)
		got := s.release(t, rel)
		// Validated, never activated by it (--no-activate).
		if s.activeID() != "" || got.State != "ready" || !got.PolicyCurrent || got.Passed == nil || *got.Passed != pStrict ||
			got.Validation == nil || got.Validation.Kind != "revalidation" || got.Validation.Source != "cli" {
			t.Fatalf("after the resubmission: active %q %+v %+v", s.activeID(), got, got.Validation)
		}
		if n := auditCount(t, "import_started", "succeeded", rel); n != 1 || releaseDatabases(t) != dbs {
			t.Fatalf("rebuilt: %d imports started, %d release databases (was %d)", n, releaseDatabases(t), dbs)
		}
		// A new build is still held to the same budget.
		_, code, stderr = importFixture(t, "/data/testdata/fixture/snapshots/karta-fixture-a.osm.pbf", []string{"KARTA_RELEASE_STORAGE_BUDGET_MB=1"}, "--no-activate")
		if code != 6 || !strings.Contains(stderr, "insufficient_storage") || releaseDatabases(t) != dbs || candidates(t) != 0 {
			t.Fatalf("a new build over the budget: exit %d, %d release databases, %d candidates\n%s", code, releaseDatabases(t), candidates(t), stderr)
		}
	})

	t.Run("an edit of only the region's name, box or view makes the pass not current and blocks activation", func(t *testing.T) {
		for _, e := range identityEdits {
			p := writeRegionAs(t, strict, e.edit)
			s := policyView(t)
			got := s.release(t, rel)
			if p == pStrict || got.PolicyCurrent || got.Passed == nil || *got.Passed != pStrict || got.State != "ready" {
				t.Fatalf("%s edited: policy %s (was %s), release %+v", e.name, p, pStrict, got)
			}
			r := op(t, http.MethodPost, "/v1/operator/releases/"+rel+"/activate", admin, map[string]any{"reason": "the " + e.name + " changed"})
			if r.status != http.StatusConflict || r.reasonCode(t) != "validation_required" {
				t.Fatalf("activation after the %s changed: %d %s", e.name, r.status, r.body)
			}
			r = op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"reason": "the " + e.name + " changed", "release_id": rel})
			if code, _ := r.errorCode(t); r.status != http.StatusConflict || code != "release_not_eligible" {
				t.Fatalf("rollback to the never-activated release after the %s changed: %d %s", e.name, r.status, r.body)
			}
			// Nor can it be revalidated against a file describing another
			// region: it is a new build.
			r = op(t, http.MethodPost, "/v1/operator/releases/"+rel+"/revalidate", admin, map[string]any{"reason": "the " + e.name + " changed"})
			if code, _ := r.errorCode(t); r.status != http.StatusConflict || code != "release_not_revalidatable" {
				t.Fatalf("revalidation after the %s changed: %d %s", e.name, r.status, r.body)
			}
			if s := policyView(t); s.activeID() != "" || s.release(t, rel).State != "ready" {
				t.Fatalf("after the %s changed: active %q, %+v", e.name, s.activeID(), s.release(t, rel))
			}
		}
		// The identity it was evaluated for, restored: current again,
		// without a revalidation.
		if p := writeRegion(t, strict); p != pStrict || !policyView(t).release(t, rel).PolicyCurrent {
			t.Fatalf("restored: policy %s, want %s", p, pStrict)
		}
	})

	t.Run("the operator activates it once it has passed the policy in force", func(t *testing.T) {
		r := op(t, http.MethodPost, "/v1/operator/releases/"+rel+"/activate", admin, map[string]any{"reason": "passed the measured checks"})
		expectStatus(t, r, http.StatusOK)
		if s := policyView(t); s.activeID() != rel {
			t.Fatalf("active %q, want %s", s.activeID(), rel)
		}
		waitManifest(t, rel)
		if n, want := auditCount(t, "activate", "rejected", rel), 2+len(identityEdits); n != want {
			t.Errorf("%d refused activations audited, want %d", n, want)
		}
	})

	t.Run("a rollback to a release served before is exempt from a changed policy, a roll-forward is not", func(t *testing.T) {
		res, code, stderr := importFixture(t, "/data/testdata/fixture/snapshots/karta-fixture-b.osm.pbf", nil, "--no-activate")
		if code != 0 || res.Outcome.State != "ready" || res.ReleaseID == "" || res.ReleaseID == rel {
			t.Fatalf("exit %d %+v\n%s", code, res, stderr)
		}
		newer := res.ReleaseID
		r := op(t, http.MethodPost, "/v1/operator/releases/"+newer+"/activate", admin, map[string]any{"reason": "newer data, passed the checks"})
		expectStatus(t, r, http.StatusOK)
		writeRegion(t, failing)
		s := policyView(t)
		if s.activeID() != newer || s.release(t, rel).State != "retired" || s.release(t, rel).PolicyCurrent || s.release(t, newer).PolicyCurrent {
			t.Fatalf("before the rollback: active %q, %+v, %+v", s.activeID(), s.release(t, rel), s.release(t, newer))
		}
		// Going back to the release served before: not held up by the
		// policy changed since.
		r = op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"reason": "back to the release served before", "release_id": rel})
		expectStatus(t, r, http.StatusOK)
		if s := policyView(t); s.activeID() != rel {
			t.Fatalf("active %q after the rollback, want %s", s.activeID(), rel)
		}
		// Going forward again to a release served before is an activation,
		// held to the policy in force.
		r = op(t, http.MethodPost, "/v1/operator/releases/"+newer+"/activate", admin, map[string]any{"reason": "forward again"})
		if r.status != http.StatusConflict || r.reasonCode(t) != "validation_required" {
			t.Fatalf("roll-forward under a policy it never passed: %d %s", r.status, r.body)
		}
		if s := policyView(t); s.activeID() != rel || s.release(t, newer).State != "retired" {
			t.Fatalf("after the refused roll-forward: active %q, %+v", s.activeID(), s.release(t, newer))
		}

		// The same with only the region's name edited (the checks it
		// passed restored): the way back stays open, the way forward not.
		writeRegion(t, strict)
		r = op(t, http.MethodPost, "/v1/operator/releases/"+newer+"/activate", admin, map[string]any{"reason": "forward, checks restored"})
		expectStatus(t, r, http.StatusOK)
		writeRegionAs(t, strict, identityEdits[0].edit)
		r = op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"reason": "back, region renamed", "release_id": rel})
		expectStatus(t, r, http.StatusOK)
		r = op(t, http.MethodPost, "/v1/operator/releases/"+newer+"/activate", admin, map[string]any{"reason": "forward, region renamed"})
		if r.status != http.StatusConflict || r.reasonCode(t) != "validation_required" {
			t.Fatalf("roll-forward after the region was renamed: %d %s", r.status, r.body)
		}
		if s := policyView(t); s.activeID() != rel || s.release(t, rel).PolicyCurrent || s.release(t, newer).PolicyCurrent {
			t.Fatalf("after the rename: active %q, %+v, %+v", s.activeID(), s.release(t, rel), s.release(t, newer))
		}
	})

	t.Run("a policy changed during an automatic publication's build stops its activation", func(t *testing.T) {
		resetAll(t)
		writeRegion(t, strict)
		slow := map[string]string{"KARTA_TEST_REGION_FILE": "/config/test-regions/fixture.json", "KARTA_TEST_OSM2PGSQL": "/usr/local/bin/slow-osm2pgsql"}
		restartPublisher(t, slow)
		submit(t, "policy-b", readRepo(t, snapB), nil, "")
		waitJobPhase(t, "building", 60*time.Second)
		pFailing := writeRegion(t, failing)
		s := waitSubmission(t, "policy-b", 120*time.Second)
		if s.State != "ready" || s.code() != "validation_required" || s.release() == "" {
			t.Fatalf("submission %+v (%s)", s, strOr(s.Reason))
		}
		v := policyView(t)
		got := v.release(t, s.release())
		// Built and validated under the policy at the start of the build,
		// not activated under the one at the switch.
		if v.activeID() != "" || got.State != "ready" || got.Passed == nil || *got.Passed == pFailing || got.PolicyCurrent {
			t.Fatalf("active %q, release %+v", v.activeID(), got)
		}
		// Restoring the policy it passed makes it activatable again,
		// without a revalidation.
		writeRegion(t, strict)
		r := op(t, http.MethodPost, "/v1/operator/releases/"+s.release()+"/activate", admin, map[string]any{"reason": "policy restored"})
		expectStatus(t, r, http.StatusOK)
	})

	t.Run("a region name edited during an automatic publication's build stops its activation", func(t *testing.T) {
		resetAll(t)
		pBuilt := writeRegion(t, strict)
		restartPublisher(t, map[string]string{"KARTA_TEST_REGION_FILE": "/config/test-regions/fixture.json", "KARTA_TEST_OSM2PGSQL": "/usr/local/bin/slow-osm2pgsql"})
		submit(t, "identity-b", readRepo(t, snapB), nil, "")
		waitJobPhase(t, "building", 60*time.Second)
		// The checks stay as they are: only the name changes.
		pRenamed := writeRegionAs(t, strict, identityEdits[0].edit)
		s := waitSubmission(t, "identity-b", 120*time.Second)
		if s.State != "ready" || s.code() != "validation_required" || s.release() == "" {
			t.Fatalf("submission %+v (%s)", s, strOr(s.Reason))
		}
		v := policyView(t)
		got := v.release(t, s.release())
		// Built and validated for the name at the start of the build, not
		// activated under another one.
		if pRenamed == pBuilt || v.activeID() != "" || got.State != "ready" || got.Passed == nil || *got.Passed != pBuilt || got.PolicyCurrent {
			t.Fatalf("active %q, release %+v", v.activeID(), got)
		}
		writeRegion(t, strict)
		r := op(t, http.MethodPost, "/v1/operator/releases/"+s.release()+"/activate", admin, map[string]any{"reason": "name restored"})
		expectStatus(t, r, http.StatusOK)
	})

	t.Run("a rollback never activates a release that was never active, so it cannot bypass the row-count gate", func(t *testing.T) {
		resetAll(t)
		restartPublisher(t, usePolicyFile)
		// A: fixture A with three more named points of interest (nodes come
		// first, sorted by id), so that B, the newer snapshot B with as many
		// features as fixture A, keeps fewer rows than A.
		base := string(readRepo(t, "testdata/fixture/karta-fixture.osm"))
		var extra strings.Builder
		for i := 0; i < 3; i++ {
			fmt.Fprintf(&extra, "  <node id=\"%d\" lat=\"0.0110000\" lon=\"%.7f\">\n    <tag k=\"amenity\" v=\"pharmacy\"/>\n"+
				"    <tag k=\"name\" v=\"Extra Pharmacy %d\"/>\n  </node>\n", 590+i, 0.005+0.0005*float64(i), i)
		}
		cut := strings.Index(base, "  <way ")
		bigger := []byte(base[:cut] + extra.String() + base[cut:])
		if err := os.WriteFile(filepath.Join(regionDir, "bigger-a.osm"), bigger, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(regionDir, "bigger-a.osm"), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(bigger)
		pinA := func(d map[string]any) {
			src := map[string]any{}
			for k, v := range doc["source"].(map[string]any) {
				src[k] = v
			}
			src["allowed_sha256"] = append(append([]any{}, src["allowed_sha256"].([]any)...), hex.EncodeToString(sum[:]))
			d["source"] = src
		}
		// The same checks with a tight relative gate: not part of the policy.
		tight := map[string]any{}
		for k, v := range strict {
			tight[k] = v
		}
		tight["max_drop_fraction"] = 0.05
		if p := writeRegionAs(t, strict, pinA); p != pStrict {
			t.Fatalf("pinning a digest changed the policy: %s, want %s", p, pStrict)
		}
		resA, code, stderr := importFixture(t, "/config/test-regions/bigger-a.osm", nil)
		if code != 0 || resA.Outcome.State != "published" {
			t.Fatalf("A: exit %d %+v\n%s", code, resA, stderr)
		}
		a := resA.ReleaseID
		resB, code, stderr := importFixture(t, "/data/testdata/fixture/snapshots/karta-fixture-b.osm.pbf", nil, "--no-activate")
		if code != 0 || resB.Outcome.State != "ready" || resB.ReleaseID == "" {
			t.Fatalf("B: exit %d %+v\n%s", code, resB, stderr)
		}
		b := resB.ReleaseID
		if p := writeRegionAs(t, tight, pinA); p != pStrict {
			t.Fatalf("max_drop_fraction changed the policy: %s, want %s", p, pStrict)
		}
		s := policyView(t)
		if s.activeID() != a || s.release(t, b).State != "ready" || !s.release(t, b).PolicyCurrent {
			t.Fatalf("before the switches: active %q, B %+v", s.activeID(), s.release(t, b))
		}
		// B passed the policy in force but would lose too many rows
		// relative to A: an activation refuses it ...
		r := op(t, http.MethodPost, "/v1/operator/releases/"+b+"/activate", admin, map[string]any{"reason": "B loses rows"})
		if r.status != http.StatusConflict || r.reasonCode(t) != "excessive_data_loss" {
			t.Fatalf("activation of B: %d %s", r.status, r.body)
		}
		// ... and a rollback, which would not apply that gate, refuses a
		// release that was never active.
		r = op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"reason": "B by rollback", "release_id": b})
		if code, _ := r.errorCode(t); r.status != http.StatusConflict || code != "release_not_eligible" || !strings.Contains(string(r.body), "activate it instead") {
			t.Fatalf("rollback to B, never active: %d %s", r.status, r.body)
		}
		if s := policyView(t); s.activeID() != a || s.release(t, b).State != "ready" || s.release(t, b).RollbackEligible {
			t.Fatalf("after the refused rollback: active %q, B %+v", s.activeID(), s.release(t, b))
		}

		// The emergency way back stays open: with B served once (under the
		// lenient gate), a rollback returns to the older A (which the
		// forward rule refuses to activate), and then to B again although
		// activating B is still refused for its row counts.
		writeRegionAs(t, strict, pinA)
		r = op(t, http.MethodPost, "/v1/operator/releases/"+b+"/activate", admin, map[string]any{"reason": "B under the lenient gate"})
		expectStatus(t, r, http.StatusOK)
		writeRegionAs(t, tight, pinA)
		r = op(t, http.MethodPost, "/v1/operator/releases/"+a+"/activate", admin, map[string]any{"reason": "forward to the older A"})
		if r.status != http.StatusConflict || r.reasonCode(t) != "older_than_active" {
			t.Fatalf("activation of the older A: %d %s", r.status, r.body)
		}
		r = op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"reason": "back to A", "release_id": a})
		expectStatus(t, r, http.StatusOK)
		r = op(t, http.MethodPost, "/v1/operator/releases/"+b+"/activate", admin, map[string]any{"reason": "forward to B again"})
		if r.status != http.StatusConflict || r.reasonCode(t) != "excessive_data_loss" {
			t.Fatalf("roll-forward to B: %d %s", r.status, r.body)
		}
		if !policyView(t).release(t, b).RollbackEligible {
			t.Fatal("B, active before, is not reported rollback-eligible")
		}
		r = op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"reason": "back to B", "release_id": b})
		expectStatus(t, r, http.StatusOK)
		if s := policyView(t); s.activeID() != b {
			t.Fatalf("after the rollback to B: active %q", s.activeID())
		}
	})
}

func samePolicy(a, b *string) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// sameValidation compares the latest evaluations of two views of a release.
func sameValidation(a, b policyRelease) bool {
	x, y := a.Validation, b.Validation
	return x.PolicySHA256 == y.PolicySHA256 && x.Passed == y.Passed && x.Kind == y.Kind && x.Actor == y.Actor &&
		x.Source == y.Source && strings.Join(x.Failed, "\n") == strings.Join(y.Failed, "\n")
}
