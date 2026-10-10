package registry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// policyA and policyB stand for two content policies (digests).
var (
	policyA = strings.Repeat("a1", 32)
	policyB = strings.Repeat("b2", 32)
)

func validation(policy string, passed bool, kind string) Validation {
	v := Validation{PolicySHA256: policy, Policy: json.RawMessage(`{"revision":"test"}`), Passed: passed, Kind: kind, At: time.Now().UTC(),
		Actor: "operator", Source: "operator_api", Checks: 3}
	if !passed {
		v.Failed = []string{"min_count_roads: 10 rows, minimum 99"}
	}
	return v
}

// readyRelease records a built release as ready, validated under policy.
func readyRelease(t *testing.T, pool *pgxpool.Pool, id, policy string) {
	t.Helper()
	ctx := context.Background()
	if err := BeginImport(ctx, pool, Release{ID: id, Database: "karta_" + id, RegionID: "iran", SourceSHA256: strings.Repeat("cd", 32),
		SchemaRevision: "schema", StyleRevision: "style"}, "cli", "cli"); err != nil {
		t.Fatal(err)
	}
	if err := SetValidating(ctx, pool, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	v := validation(policy, true, ValidationImport)
	v.Source = "cli"
	if err := MarkReady(ctx, pool, id, 1, 1000, map[string]int64{"roads": 10}, v); err != nil {
		t.Fatal(err)
	}
}

func passedPolicy(t *testing.T, pool *pgxpool.Pool, id string) (string, *Validation) {
	t.Helper()
	r, err := Get(context.Background(), pool, id)
	if err != nil || r == nil {
		t.Fatalf("release %s: %v %v", id, r, err)
	}
	if r.ValidationPolicySHA256 == nil {
		return "", r.Validation
	}
	return *r.ValidationPolicySHA256, r.Validation
}

// A release records the content policy it passed: its build's, then any
// revalidation's. A failure under the policy it passed withdraws it; a
// failure under another policy leaves it; each evaluation is audited.
func TestDBReleasesRecordTheirValidationPolicy(t *testing.T) {
	pool, _, _ := scratchDB(t)
	ctx := context.Background()
	// A release built before version 6 has no recorded policy after the
	// upgrade: it must be revalidated before a forward activation.
	if err := migrateTo(ctx, pool, 5); err != nil {
		t.Fatal(err)
	}
	// A version 5 registry as a deployment holds it: an active release and
	// a ready release that was never activated, with every column set.
	old, served := "r"+strings.Repeat("0", 23)+"1", "r"+strings.Repeat("0", 23)+"4"
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`
INSERT INTO registry.releases (release_id, database_name, state, region_id, source_sha256, source_size, schema_revision, style_revision,
    schema_major, data_timestamp, database_bytes, counts, activated_at, created_at, updated_at)
VALUES ($1, $2, 'ready', 'iran', $3, 123456789, 's', 'y', 1, '2026-09-01T00:00:00Z', 987654321, '{"roads": 10, "pois": 7}', NULL,
        '2026-09-02T00:00:00Z', '2026-09-03T00:00:00Z'),
       ($4, $5, 'active', 'iran', $6, 1234, 's', 'y', 1, '2026-08-01T00:00:00Z', 5678, '{"roads": 9, "pois": 7}', '2026-08-03T00:00:00Z',
        '2026-08-02T00:00:00Z', '2026-08-03T00:00:00Z')`,
			[]any{old, "karta_" + old, strings.Repeat("ef", 32), served, "karta_" + served, strings.Repeat("ab", 32)}},
		{`INSERT INTO registry.active_release (singleton, release_id, activated_at, activated_by) VALUES (true, $1, '2026-08-03T00:00:00Z', 'operator')`,
			[]any{served}},
		{`INSERT INTO registry.audit (actor, source, action, target, outcome) VALUES ('cli', 'cli', 'import_validated', $1, 'succeeded')`,
			[]any{old}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	// Everything but the two new columns, as text: the rows (all columns,
	// updated_at included), the active pointer and the audit log.
	state := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `
SELECT (SELECT jsonb_agg(to_jsonb(r) - 'validation_policy_sha256' - 'validation' ORDER BY release_id) FROM registry.releases r)::text
    || (SELECT jsonb_agg(to_jsonb(a)) FROM registry.active_release a)::text
    || (SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM registry.audit a)::text`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := state()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if after := state(); after != before {
		t.Fatalf("the upgrade to version 6 changed the registry:\nbefore %s\nafter  %s", before, after)
	}
	var version int
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM registry.schema_migrations`).Scan(&version); err != nil || version != 6 {
		t.Fatalf("schema version %d %v", version, err)
	}
	for _, id := range []string{old, served} {
		if p, v := passedPolicy(t, pool, id); p != "" || v != nil {
			t.Fatalf("release %s, built before version 6, has a policy: %q %+v", id, p, v)
		}
	}
	if r, err := Get(ctx, pool, old); err != nil || r.State != StateReady || r.ActivatedAt != nil || r.Counts["roads"] != 10 {
		t.Fatalf("the ready release after the upgrade: %+v %v", r, err)
	}
	// Migrating again changes nothing.
	again := state()
	if err := Migrate(ctx, pool); err != nil || state() != again {
		t.Fatalf("a second migration changed the registry: %v", err)
	}

	id := "r" + strings.Repeat("0", 23) + "2"
	readyRelease(t, pool, id, policyA)
	if p, v := passedPolicy(t, pool, id); p != policyA || v == nil || !v.Passed || v.Kind != ValidationImport || v.Checks != 3 {
		t.Fatalf("after its build: %q %+v", p, v)
	}
	// A ready release needs a passed validation.
	if err := MarkReady(ctx, pool, "r"+strings.Repeat("0", 23)+"3", 1, 1, nil, validation(policyA, false, ValidationImport)); err == nil {
		t.Fatal("a failed validation marked a release ready")
	}

	steps := []struct {
		name   string
		v      Validation
		passed string
	}{
		{"fails another policy: keeps the one it passed", validation(policyB, false, ValidationRevalidation), policyA},
		{"passes the new policy", validation(policyB, true, ValidationRevalidation), policyB},
		{"fails the policy it passed: withdrawn", validation(policyB, false, ValidationRevalidation), ""},
		{"passes it again", validation(policyB, true, ValidationRevalidation), policyB},
	}
	for _, st := range steps {
		if err := RecordValidation(ctx, pool, id, st.v, "test: "+st.name, "req-1"); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		p, v := passedPolicy(t, pool, id)
		if p != st.passed || v == nil || v.Passed != st.v.Passed || v.PolicySHA256 != st.v.PolicySHA256 || v.Kind != ValidationRevalidation {
			t.Fatalf("%s: passed policy %q, latest %+v", st.name, p, v)
		}
	}
	var audited, failed int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE outcome = 'failed' AND detail->'failed' IS NOT NULL)
FROM registry.audit WHERE action = 'release_revalidated' AND target = $1 AND detail->>'policy_sha256' IS NOT NULL AND request_id = 'req-1'`, id).Scan(&audited, &failed); err != nil {
		t.Fatal(err)
	}
	if audited != len(steps) || failed != 2 {
		t.Fatalf("audit records: %d (%d failed), want %d (2 failed)", audited, failed, len(steps))
	}

	// Only built releases (ready, retired or active) are evaluated.
	if err := Fail(ctx, pool, id, "test", "cli", "cli"); err != nil {
		t.Fatal(err)
	}
	if err := RecordValidation(ctx, pool, id, validation(policyB, true, ValidationRevalidation), "x", ""); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("a failed release was revalidated: %v", err)
	}
	// Building it again starts without any policy.
	if err := BeginImport(ctx, pool, Release{ID: id, Database: "karta_" + id, RegionID: "iran", SourceSHA256: strings.Repeat("cd", 32),
		SchemaRevision: "schema", StyleRevision: "style"}, "cli", "cli"); err != nil {
		t.Fatal(err)
	}
	if p, v := passedPolicy(t, pool, id); p != "" || v != nil {
		t.Fatalf("a rebuild kept the earlier validation: %q %+v", p, v)
	}
}

// lockWaiters counts sessions of database db waiting for a lock.
func lockWaiters(t *testing.T, admin *pgx.Conn, db string) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND wait_event_type = 'Lock'`, db).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitForLockWaiter(t *testing.T, admin *pgx.Conn, db, what string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("%s finished (%v) instead of waiting for the release row", what, err)
		default:
		}
		if lockWaiters(t, admin, db) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s does not wait for the release row", what)
}

// requirePolicy is the shape of the publisher's policy gate: the target
// must have passed policy.
func requirePolicy(policy string) func(active, target *Release) error {
	return func(_, target *Release) error {
		if target.ValidationPolicySHA256 == nil || *target.ValidationPolicySHA256 != policy {
			return errors.New("validation_required")
		}
		return nil
	}
}

func activate(pool *pgxpool.Pool, id string, allow func(active, target *Release) error, beforeCommit func()) error {
	_, err := Activate(context.Background(), pool, ActivateRequest{Target: id, CheckExpected: true, Action: "activate", Actor: "operator",
		Source: "operator_api", Reason: "test", PinGrace: time.Minute, Allow: allow, BeforeCommit: beforeCommit}, 30*time.Second)
	return err
}

// An activation and a validation record of the same release are serialized
// on the release row: an activation that arrives while an evaluation is
// being recorded waits for it and decides on its outcome, never on the
// record it replaces.
func TestDBActivationWaitsForAValidationBeingRecorded(t *testing.T) {
	for _, c := range []struct {
		name      string
		before    string     // the policy the release passed before
		record    Validation // recorded concurrently
		activates bool       // under policyB
	}{
		{"a revalidation passing the policy in force", policyA, validation(policyB, true, ValidationRevalidation), true},
		{"a revalidation failing the policy it had passed", policyB, validation(policyB, false, ValidationRevalidation), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, admin, db := scratchRegistry(t)
			id := "r" + strings.Repeat("0", 23) + "4"
			readyRelease(t, pool, id, c.before)
			held, resume := make(chan struct{}), make(chan struct{})
			var once, released sync.Once
			beforeValidationCommit = func() { once.Do(func() { close(held); <-resume }) }
			release := func() { released.Do(func() { close(resume) }) }
			t.Cleanup(func() { release(); beforeValidationCommit = nil })
			recorded := make(chan error, 1)
			go func() { recorded <- RecordValidation(context.Background(), pool, id, c.record, "concurrent", "") }()
			<-held
			activated := make(chan error, 1)
			go func() { activated <- activate(pool, id, requirePolicy(policyB), nil) }()
			waitForLockWaiter(t, admin, db, "the activation", activated)
			release()
			if err := <-recorded; err != nil {
				t.Fatal(err)
			}
			err := <-activated
			if (err == nil) != c.activates {
				t.Fatalf("activation: %v, want activated=%v", err, c.activates)
			}
			r, _ := Get(context.Background(), pool, id)
			if (r.State == StateActive) != c.activates {
				t.Fatalf("state %s", r.State)
			}
		})
	}
}

// The other order: a validation record that arrives while an activation
// that already passed the gate is committing waits for it; the activation
// stands (it was decided on the record valid at its switch) and the record
// lands afterwards, audited after it.
func TestDBValidationRecordWaitsForAnActivation(t *testing.T) {
	pool, admin, db := scratchRegistry(t)
	ctx := context.Background()
	id := "r" + strings.Repeat("0", 23) + "5"
	readyRelease(t, pool, id, policyA)
	held, resume := make(chan struct{}), make(chan struct{})
	var once, released sync.Once
	release := func() { released.Do(func() { close(resume) }) }
	t.Cleanup(release)
	activated := make(chan error, 1)
	go func() {
		activated <- activate(pool, id, requirePolicy(policyA), func() { once.Do(func() { close(held); <-resume }) })
	}()
	<-held
	recorded := make(chan error, 1)
	go func() {
		recorded <- RecordValidation(ctx, pool, id, validation(policyA, false, ValidationRevalidation), "concurrent", "")
	}()
	waitForLockWaiter(t, admin, db, "the validation record", recorded)
	release()
	if err := <-activated; err != nil {
		t.Fatal(err)
	}
	if err := <-recorded; err != nil {
		t.Fatal(err)
	}
	r, _ := Get(ctx, pool, id)
	if r.State != StateActive || r.ValidationPolicySHA256 != nil || r.Validation == nil || r.Validation.Passed {
		t.Fatalf("after both: %s %v %+v", r.State, r.ValidationPolicySHA256, r.Validation)
	}
	var order []string
	rows, err := pool.Query(ctx, `SELECT action FROM registry.audit WHERE target = $1 AND action IN ('activate', 'release_revalidated') ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	order, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || strings.Join(order, ",") != "activate,release_revalidated" {
		t.Fatalf("audit order %v %v", order, err)
	}
}

// One evaluation of a release at a time, across processes (each session
// stands for one): a second is refused at once as a busy error, another
// release is not held up, the outcome is recorded on the session holding
// the lock, and the lock goes with an unlock or with the session that held
// it, so a process that dies during an evaluation does not wedge the
// release. The lock does not hold up an activation: the gate decides on the
// recorded outcome.
func TestDBOneRevalidationOfAReleaseAtATime(t *testing.T) {
	pool, _, _ := scratchRegistry(t)
	ctx := context.Background()
	session := func() *pgx.Conn {
		t.Helper()
		c, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close(context.Background()) })
		return c
	}
	r1, r2 := "r"+strings.Repeat("0", 23)+"1", "r"+strings.Repeat("0", 23)+"2"
	readyRelease(t, pool, r1, policyA)
	a, b := session(), session()
	if err := LockRevalidation(ctx, a, r1); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := LockRevalidation(ctx, b, r1); !errors.Is(err, ErrRevalidating) || !errors.Is(err, ErrBusy) || time.Since(start) > 2*time.Second {
		t.Fatalf("a second evaluation of %s: %v after %v", r1, err, time.Since(start))
	}
	if err := LockRevalidation(ctx, b, r2); err != nil {
		t.Fatalf("another release: %v", err)
	}
	if err := RecordValidation(ctx, a, r1, validation(policyB, true, ValidationRevalidation), "test", "req-lock"); err != nil {
		t.Fatal(err)
	}
	if p, _ := passedPolicy(t, pool, r1); p != policyB {
		t.Fatalf("recorded on the locked session: passed %q", p)
	}
	if err := UnlockRevalidation(ctx, a, r1); err != nil {
		t.Fatal(err)
	}
	if err := LockRevalidation(ctx, b, r1); err != nil {
		t.Fatalf("after the unlock: %v", err)
	}
	// b ends holding both locks, as a process killed mid-evaluation would.
	if err := b.Close(ctx); err != nil {
		t.Fatal(err)
	}
	c := session()
	for _, id := range []string{r1, r2} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := LockRevalidation(ctx, c, id)
			if err == nil {
				break
			}
			if !errors.Is(err, ErrRevalidating) || time.Now().After(deadline) {
				t.Fatalf("%s after its holder's session ended: %v", id, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	res, err := Activate(ctx, pool, ActivateRequest{Target: r1, Action: "activate", Actor: "operator", Source: "operator_api"}, time.Second)
	if err != nil || !res.Changed {
		t.Fatalf("activation while an evaluation holds the lock: %+v %v", res, err)
	}
}
