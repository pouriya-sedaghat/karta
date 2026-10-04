package registry

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests need PostgreSQL: KARTA_TEST_PG_DSN names a superuser
// connection (make test-integration sets it to the test stack's cluster).
// Each test migrates a scratch database and drops it afterwards.

func scratchRegistry(t *testing.T) (*pgxpool.Pool, *pgx.Conn, string) {
	t.Helper()
	pool, admin, name := scratchDB(t)
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool, admin, name
}

// scratchDB is an empty scratch database, dropped afterwards.
func scratchDB(t *testing.T) (*pgxpool.Pool, *pgx.Conn, string) {
	t.Helper()
	dsn := os.Getenv("KARTA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("KARTA_TEST_PG_DSN is not set (make test-integration sets it): these tests need PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("karta_regtest_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})
	return pool, admin, name
}

// digestLockWaiters counts sessions in database db waiting for a digest lock.
func digestLockWaiters(t *testing.T, admin *pgx.Conn, db string) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks l JOIN pg_database d ON d.oid = l.database
WHERE l.locktype = 'advisory' AND NOT l.granted AND l.classid::bigint = $1 AND l.objsubid = 2 AND d.datname = $2`,
		int64(digestLockKey), db).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// holdIntake starts an intake authorization and holds its transaction right
// after its block check passed; release lets it finish.
func holdIntake(t *testing.T, pool *pgxpool.Pool, r IntakeRequest) (release func(), done <-chan error) {
	t.Helper()
	paused, resume := make(chan struct{}), make(chan struct{})
	var once, released sync.Once
	afterIntakeBlockCheck = func() {
		once.Do(func() {
			close(paused)
			<-resume
		})
	}
	release = func() { released.Do(func() { close(resume) }) }
	// Also on a failure: the held transaction must end before the pool
	// closes (cleanups run last-registered first).
	t.Cleanup(func() {
		release()
		afterIntakeBlockCheck = nil
	})
	res := make(chan error, 1)
	go func() {
		_, _, err := AuthorizeIntake(context.Background(), pool, r)
		res <- err
	}()
	select {
	case <-paused:
	case err := <-res:
		t.Fatalf("the intake authorization finished without reaching its block check: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the intake authorization did not reach its block check")
	}
	return release, res
}

// waitBlocked waits until op is either finished (it was not serialized with
// the held intake transaction) or waiting for the digest lock.
func waitBlocked(t *testing.T, admin *pgx.Conn, db string, done <-chan error, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("%s finished (%v) while an intake authorization of the digest that had passed its block check was still open: "+
				"it is not serialized with it", what, err)
		default:
		}
		if digestLockWaiters(t, admin, db) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s neither finished nor waits for the digest lock", what)
}

func openRows(t *testing.T, pool *pgxpool.Pool, region, digest string) map[string]int {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT channel FROM registry.authorizations
WHERE region_id = $1 AND sha256 = $2 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())`, region, digest)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var ch string
		if err := rows.Scan(&ch); err != nil {
			t.Fatal(err)
		}
		out[ch]++
	}
	return out
}

func blocked(t *testing.T, pool *pgxpool.Pool, region, digest string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM registry.intake_blocks WHERE region_id = $1 AND sha256 = $2`,
		region, digest).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func intakeRequest(digest, name string) IntakeRequest {
	return IntakeRequest{RegionID: "iran", SHA256: digest, SizeBytes: 1000, ExpiresAt: time.Now().Add(time.Hour), Channel: ChannelIntakeWatch,
		Name: name, CreatedBy: "local-intake", Reason: "landing delivery", RequestID: "r1", MaxOpen: 2}
}

// An operator's revoke that arrives while an intake authorization of the
// digest has already checked for a block (and found none) is serialized
// after it: it waits, then closes the row the intake committed. The intake
// never ends up with an effective authorization after the revoke.
func TestDBRevokeIsSerializedWithAnIntakeAuthorization(t *testing.T) {
	pool, admin, db := scratchRegistry(t)
	digest := strings.Repeat("ab", 32)
	release, intakeDone := holdIntake(t, pool, intakeRequest(digest, "iran-w20261004T000000Z-abababababab"))
	revokeDone := make(chan error, 1)
	go func() {
		_, err := Revoke(context.Background(), pool, "iran", digest, "operator", "stop this digest", "r2")
		revokeDone <- err
	}()
	waitBlocked(t, admin, db, revokeDone, "the operator's revoke")
	release()
	if err := <-intakeDone; err != nil {
		t.Fatalf("the intake authorization: %v", err)
	}
	if err := <-revokeDone; err != nil {
		t.Fatalf("the revoke: %v", err)
	}
	if open := openRows(t, pool, "iran", digest); len(open) != 0 {
		t.Fatalf("authorizations still effective after the revoke: %v", open)
	}
	if !blocked(t, pool, "iran", digest) {
		t.Fatal("the revoke recorded no intake block")
	}
	a, err := Authorized(context.Background(), pool, "iran", digest, 1000,
		AuthScope{IntakeName: "iran-w20261004T000000Z-abababababab", IntakeChannel: ChannelIntakeWatch})
	if err != nil || a != nil {
		t.Fatalf("still authorized after the revoke: %+v %v", a, err)
	}
	// Later intake requests are refused until an operator authorizes it.
	if _, _, err := AuthorizeIntake(context.Background(), pool, intakeRequest(digest, "iran-w20261004T000001Z-abababababab")); err == nil ||
		!strings.Contains(err.Error(), ErrIntakeBlocked.Error()) {
		t.Fatalf("an intake authorization after the revoke: %v", err)
	}
}

// An operator's re-authorization of the digest is serialized with a held
// intake authorization too, and lifts the block for later intake requests.
func TestDBOperatorReauthorizationIsSerializedWithAnIntakeAuthorization(t *testing.T) {
	pool, admin, db := scratchRegistry(t)
	ctx := context.Background()
	digest := strings.Repeat("cd", 32)
	if _, err := Revoke(ctx, pool, "iran", digest, "operator", "stop", "r0"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := AuthorizeIntake(ctx, pool, intakeRequest(digest, "iran-w20261004T000000Z-cdcdcdcdcdcd")); err == nil {
		t.Fatal("a blocked digest was authorized by the intake")
	}
	size := int64(1000)
	if _, _, err := Authorize(ctx, pool, Authorization{RegionID: "iran", SHA256: digest, SizeBytes: &size, Reason: "reviewed", CreatedBy: "operator"},
		"r1"); err != nil {
		t.Fatal(err)
	}
	if blocked(t, pool, "iran", digest) {
		t.Fatal("the operator's authorization did not lift the block")
	}
	other := strings.Repeat("ef", 32)
	release, intakeDone := holdIntake(t, pool, intakeRequest(other, "iran-w20261004T000000Z-efefefefefef"))
	authDone := make(chan error, 1)
	go func() {
		_, _, err := Authorize(ctx, pool, Authorization{RegionID: "iran", SHA256: other, SizeBytes: &size, Reason: "reviewed", CreatedBy: "operator"}, "r2")
		authDone <- err
	}()
	waitBlocked(t, admin, db, authDone, "the operator's authorization")
	release()
	if err := <-intakeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-authDone; err != nil {
		t.Fatal(err)
	}
	if open := openRows(t, pool, "iran", other); open[ChannelOperator] != 1 || open[ChannelIntakeWatch] != 1 {
		t.Fatalf("open authorizations %v, want one operator and one intake row", open)
	}
}
