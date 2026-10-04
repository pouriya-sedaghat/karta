package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// submitRequest is an intake_submit authorization request of a credential.
func submitRequest(digest, name, by string) IntakeRequest {
	r := intakeRequest(digest, name)
	r.Channel, r.CreatedBy, r.MaxOpen = ChannelIntakeSubmit, by, 100
	return r
}

func commandName(digest string, hhmmss int) string {
	return fmt.Sprintf("iran-c20261004T%06dZ-%s", hhmmss, digest[:12])
}

// rowsNamed returns "creator:open|closed" for every authorization carrying
// name.
func rowsNamed(t *testing.T, pool *pgxpool.Pool, name string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT created_by, revoked_at IS NULL FROM registry.authorizations WHERE intake_name = $1 ORDER BY id`, name)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var by string
		var open bool
		if err := rows.Scan(&by, &open); err != nil {
			t.Fatal(err)
		}
		st := "closed"
		if open {
			st = "open"
		}
		out = append(out, by+":"+st)
	}
	return out
}

func expectTaken(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, ErrIntakeNameTaken) {
		t.Fatalf("%s: %v, want ErrIntakeNameTaken", what, err)
	}
}

// A handoff name identifies one authorization for good: no credential can
// authorize a name any authorization (open or closed, however old) or intake
// submission already carries, and a refusal changes nothing.
func TestDBIntakeHandoffNamesAreNeverReused(t *testing.T) {
	pool, _, _ := scratchRegistry(t)
	ctx := context.Background()
	d := strings.Repeat("1a", 32)
	name := commandName(d, 50000)
	a, created, err := AuthorizeIntake(ctx, pool, submitRequest(d, name, "alice"))
	if err != nil || !created {
		t.Fatal(created, err)
	}
	if _, err := CloseIntakeAuthorization(ctx, pool, a.ID, "alice", "the submission is final: published", "r1"); err != nil {
		t.Fatal(err)
	}

	t.Run("another credential, the first authorization closed", func(t *testing.T) {
		_, _, err := AuthorizeIntake(ctx, pool, submitRequest(d, name, "bob"))
		expectTaken(t, err, "bob")
		if got := rowsNamed(t, pool, name); len(got) != 1 || got[0] != "alice:closed" {
			t.Fatalf("rows named %s: %v", name, got)
		}
		recs, err := ListIntakeAuthorizations(ctx, pool, "iran", "bob", 50)
		if err != nil || len(recs) != 0 {
			t.Fatalf("bob's records: %+v %v", recs, err)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM registry.audit WHERE actor = 'bob' AND action = 'intake_authorize'
AND outcome = 'rejected' AND detail->>'name' = $1 AND detail->>'refused' LIKE '%already used%'`, name).Scan(&n); err != nil || n != 1 {
			t.Fatalf("audit of the refusal: %d %v", n, err)
		}
	})
	t.Run("the same credential, closed", func(t *testing.T) {
		_, _, err := AuthorizeIntake(ctx, pool, submitRequest(d, name, "alice"))
		expectTaken(t, err, "alice again")
	})
	t.Run("another credential, the first authorization open; a refusal changes nothing", func(t *testing.T) {
		d2 := strings.Repeat("2b", 32)
		n2 := commandName(d2, 50001)
		a2, _, err := AuthorizeIntake(ctx, pool, submitRequest(d2, n2, "alice"))
		if err != nil {
			t.Fatal(err)
		}
		b2, _, err := AuthorizeIntake(ctx, pool, submitRequest(d2, n2+"-1", "bob"))
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = AuthorizeIntake(ctx, pool, submitRequest(d2, n2, "bob"))
		expectTaken(t, err, "bob asking for alice's open name")
		if got := rowsNamed(t, pool, n2); len(got) != 1 || got[0] != "alice:open" {
			t.Fatalf("rows named %s: %v", n2, got)
		}
		// Bob's own open row of the digest is not superseded by the refused
		// request.
		if got := rowsNamed(t, pool, n2+"-1"); len(got) != 1 || got[0] != "bob:open" {
			t.Fatalf("bob's own row: %v (id %d)", got, b2.ID)
		}
		// Alice repeating her own open request is still idempotent.
		again, created, err := AuthorizeIntake(ctx, pool, submitRequest(d2, n2, "alice"))
		if err != nil || created || again.ID != a2.ID {
			t.Fatalf("alice repeating: %+v %v %v", again, created, err)
		}
	})
	t.Run("a name the publisher already recorded a submission under", func(t *testing.T) {
		d3 := strings.Repeat("3c", 32)
		n3 := commandName(d3, 50002)
		if _, _, err := ClaimSubmission(ctx, pool, Submission{Source: "intake", Name: n3, Fingerprint: "fp-" + n3, RegionID: "iran"}, nil); err != nil {
			t.Fatal(err)
		}
		_, _, err := AuthorizeIntake(ctx, pool, submitRequest(d3, n3, "carol"))
		expectTaken(t, err, "a name with a submission")
	})
	t.Run("outside the API's listing window", func(t *testing.T) {
		// Alice made 55 handoffs; the first is the oldest, closed over a
		// week ago, so her listing (at most 50 recent records) omits it.
		var first string
		for i := 0; i < 55; i++ {
			di := fmt.Sprintf("%064x", 0x5500+i)
			ni := commandName(di, 60000+i)
			if i == 0 {
				first = ni
			}
			a, _, err := AuthorizeIntake(ctx, pool, submitRequest(di, ni, "alice"))
			if err != nil {
				t.Fatal(i, err)
			}
			if _, err := CloseIntakeAuthorization(ctx, pool, a.ID, "alice", "final", "r"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.Exec(ctx, `UPDATE registry.authorizations SET created_at = now() - interval '9 days', revoked_at = now() - interval '8 days'
WHERE intake_name = $1`, first); err != nil {
			t.Fatal(err)
		}
		recs, err := ListIntakeAuthorizations(ctx, pool, "iran", "alice", 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if *r.Authorization.IntakeName == first {
				t.Fatalf("setup: %s is still listed", first)
			}
		}
		di := fmt.Sprintf("%064x", 0x5500)
		for _, by := range []string{"bob", "alice"} {
			_, _, err := AuthorizeIntake(ctx, pool, submitRequest(di, first, by))
			expectTaken(t, err, by+" asking for a name outside the listing")
		}
	})
	t.Run("the database refuses a duplicate written past the checks", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO registry.authorizations (region_id, sha256, size_bytes, reason, created_by, expires_at, channel, intake_name)
VALUES ('iran', $1, 1000, 'past the checks', 'mallory', now() + interval '1 hour', 'intake_submit', $2)`, d, name)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != intakeNameIndex {
			t.Fatalf("a duplicate handoff name was stored: %v", err)
		}
	})
}

// A submission recorded under a name before its authorization existed (by a
// writer past the checks, or racing them) is not that authorization's
// handoff: the record shows no outcome until its own handoff's submission.
func TestDBIntakeRecordsShowOnlyTheirOwnHandoffsSubmission(t *testing.T) {
	pool, _, _ := scratchRegistry(t)
	ctx := context.Background()
	d := strings.Repeat("4d", 32)
	name := commandName(d, 70000)
	early, _, err := ClaimSubmission(ctx, pool, Submission{Source: "intake", Name: name, Fingerprint: "fp-early", RegionID: "iran"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := UpdateSubmission(ctx, pool, early.ID, SubmissionUpdate{State: SubPublished, ReleaseID: "r000000000000000000000001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE registry.submissions SET created_at = now() - interval '1 minute' WHERE id = $1`, early.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO registry.authorizations (region_id, sha256, size_bytes, reason, created_by, expires_at, channel, intake_name)
VALUES ('iran', $1, 1000, 'racing the submission', 'alice', now() + interval '1 hour', 'intake_submit', $2)`, d, name); err != nil {
		t.Fatal(err)
	}
	recs, err := ListIntakeAuthorizations(ctx, pool, "iran", "alice", 50)
	if err != nil || len(recs) != 1 {
		t.Fatalf("%+v %v", recs, err)
	}
	if recs[0].Submission != nil {
		t.Fatalf("the record shows a submission recorded before it existed: %+v", recs[0].Submission)
	}
	own, _, err := ClaimSubmission(ctx, pool, Submission{Source: "intake", Name: name, Fingerprint: "fp-own", RegionID: "iran"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	recs, err = ListIntakeAuthorizations(ctx, pool, "iran", "alice", 50)
	if err != nil || len(recs) != 1 || recs[0].Submission == nil || recs[0].Submission.ID != own.ID {
		t.Fatalf("its own submission: %+v %v", recs, err)
	}
}

// Upgrading a version 4 registry whose authorizations already share a name:
// all of them are closed, all but the first are renamed to a name no handoff
// carries, the change is audited, and the index holds from then on.
func TestDBMigrationClosesAuthorizationsSharingAName(t *testing.T) {
	pool, _, _ := scratchDB(t)
	ctx := context.Background()
	if err := migrateTo(ctx, pool, 4); err != nil {
		t.Fatal(err)
	}
	d := strings.Repeat("5e", 32)
	shared, single := commandName(d, 80000), commandName(d, 80001)
	ids := map[string]int64{}
	for _, r := range []struct{ by, name, closed string }{
		{"alice", shared, ""}, {"bob", shared, ""}, {"carol", shared, "superseded"}, {"dave", single, ""},
	} {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO registry.authorizations (region_id, sha256, size_bytes, reason, created_by, expires_at, channel,
    intake_name, revoked_at, revoked_by, revoke_reason)
VALUES ('iran', $1, 1000, 'v4', $2, now() + interval '1 hour', 'intake_submit', $3, CASE WHEN $4 = '' THEN NULL ELSE now() END,
    NULLIF($4, '')::text, NULLIF($4, '')) RETURNING id`, d, r.by, r.name, r.closed).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[r.by] = id
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	type row struct {
		name   string
		open   bool
		reason string
	}
	get := func(by string) row {
		var r row
		if err := pool.QueryRow(ctx, `SELECT intake_name, revoked_at IS NULL, COALESCE(revoke_reason, '') FROM registry.authorizations WHERE id = $1`,
			ids[by]).Scan(&r.name, &r.open, &r.reason); err != nil {
			t.Fatal(err)
		}
		return r
	}
	want := map[string]row{
		"alice": {shared, false, "handoff name shared with another authorization"},
		"bob":   {fmt.Sprintf("%s~shared-%d", shared, ids["bob"]), false, "handoff name shared with another authorization"},
		"carol": {fmt.Sprintf("%s~shared-%d", shared, ids["carol"]), false, "superseded"},
		"dave":  {single, true, ""},
	}
	for by, w := range want {
		if got := get(by); got != w {
			t.Errorf("%s: %+v, want %+v", by, got, w)
		}
		if _, ok := ParseHandoffName(get(by).name); ok != (by == "alice" || by == "dave") {
			t.Errorf("%s: %q parses as a handoff name: %v", by, get(by).name, ok)
		}
	}
	var detail string
	if err := pool.QueryRow(ctx, `SELECT detail->>'authorization_ids' FROM registry.audit WHERE action = 'intake_name_deduplicate' AND target = $1`,
		shared).Scan(&detail); err != nil || detail != fmt.Sprintf("[%d, %d, %d]", ids["alice"], ids["bob"], ids["carol"]) {
		t.Errorf("audit: %q %v", detail, err)
	}
	_, _, err := AuthorizeIntake(ctx, pool, submitRequest(d, shared, "erin"))
	expectTaken(t, err, "the shared name after the upgrade")
}

// The publisher's scope binds an intake row to the channel its handoff's name
// says wrote it: a submit row naming a watcher handoff (written past the API
// and past the name index, since the watcher's own row is absent) admits
// nothing.
func TestDBScopeBindsAHandoffToItsChannel(t *testing.T) {
	pool, _, _ := scratchRegistry(t)
	ctx := context.Background()
	d := strings.Repeat("6f", 32)
	watcherName := "iran-w20261004T090000Z-" + d[:12]
	if _, err := pool.Exec(ctx, `INSERT INTO registry.authorizations (region_id, sha256, size_bytes, reason, created_by, expires_at, channel, intake_name)
VALUES ('iran', $1, 1000, 'a submit row naming a watcher handoff', 'mallory', now() + interval '1 hour', 'intake_submit', $2)`, d, watcherName); err != nil {
		t.Fatal(err)
	}
	a, err := Authorized(ctx, pool, "iran", d, 1000, AuthScope{IntakeName: watcherName, IntakeChannel: ChannelIntakeWatch})
	if err != nil || a != nil {
		t.Fatalf("a submit row admitted a watcher handoff: %+v %v", a, err)
	}
}
