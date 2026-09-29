// Package registry is Karta's control plane: the list of releases, their
// state and the single active-release pointer. It lives in its own database,
// separate from release databases. Stage 1 bootstraps exactly one active
// release; Stage 2 adds staged candidates, atomic switching and rollback on
// top of the same tables.
package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// States of a release. Stage 1 uses importing -> validating -> ready ->
// active, or -> failed. `retired` is reserved for Stage 2.
const (
	StateImporting  = "importing"
	StateValidating = "validating"
	StateReady      = "ready"
	StateActive     = "active"
	StateFailed     = "failed"
)

// ReaderRole may read the registry and all release databases.
const ReaderRole = "karta_reader"

// importLockKey serialises imports across processes (pg advisory lock).
const importLockKey int64 = 0x6b61727461 // "karta"

const migration = `
CREATE SCHEMA IF NOT EXISTS registry;
CREATE TABLE IF NOT EXISTS registry.releases (
    release_id text PRIMARY KEY CHECK (release_id ~ '^r[0-9a-f]{24}$'),
    database_name text NOT NULL UNIQUE,
    state text NOT NULL CHECK (state IN ('importing', 'validating', 'ready', 'active', 'failed', 'retired')),
    region_id text NOT NULL,
    source_sha256 text NOT NULL,
    schema_revision text NOT NULL,
    style_revision text NOT NULL,
    data_timestamp timestamptz,
    failure_reason text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS registry.active_release (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    release_id text NOT NULL REFERENCES registry.releases (release_id),
    activated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS registry.events (
    id bigserial PRIMARY KEY,
    at timestamptz NOT NULL DEFAULT now(),
    release_id text,
    event text NOT NULL,
    detail jsonb
);
GRANT USAGE ON SCHEMA registry TO karta_reader;
GRANT SELECT ON registry.releases, registry.active_release TO karta_reader;
`

// Release is one registry row.
type Release struct {
	ID            string
	Database      string
	State         string
	RegionID      string
	SourceSHA256  string
	FailureReason *string
}

// Migrate creates the registry schema if needed (idempotent).
func Migrate(ctx context.Context, conn *pgx.Conn) error {
	_, err := conn.Exec(ctx, migration)
	return err
}

// LockImports takes the session-level import lock, waiting at most timeout.
func LockImports(ctx context.Context, conn *pgx.Conn, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, importLockKey).Scan(&ok); err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("another import holds the import lock")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Active returns the active release, or nil if there is none. A registry
// that has not been migrated yet also has no active release.
func Active(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (*Release, error) {
	var r Release
	err := q.QueryRow(ctx, `
SELECT r.release_id, r.database_name, r.state, r.region_id, r.source_sha256, r.failure_reason
FROM registry.active_release a JOIN registry.releases r USING (release_id)`).Scan(
		&r.ID, &r.Database, &r.State, &r.RegionID, &r.SourceSHA256, &r.FailureReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Get returns a release by id, or nil.
func Get(ctx context.Context, conn *pgx.Conn, id string) (*Release, error) {
	var r Release
	err := conn.QueryRow(ctx, `
SELECT release_id, database_name, state, region_id, source_sha256, failure_reason
FROM registry.releases WHERE release_id = $1`, id).Scan(
		&r.ID, &r.Database, &r.State, &r.RegionID, &r.SourceSHA256, &r.FailureReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Begin records a release entering import, replacing a previous failed or
// interrupted attempt for the same id.
func Begin(ctx context.Context, conn *pgx.Conn, r Release, schemaRev, styleRev string) error {
	_, err := conn.Exec(ctx, `
INSERT INTO registry.releases (release_id, database_name, state, region_id, source_sha256, schema_revision, style_revision)
VALUES ($1, $2, 'importing', $3, $4, $5, $6)
ON CONFLICT (release_id) DO UPDATE SET state = 'importing', failure_reason = NULL, updated_at = now()`,
		r.ID, r.Database, r.RegionID, r.SourceSHA256, schemaRev, styleRev)
	if err == nil {
		err = Event(ctx, conn, r.ID, "import_started", nil)
	}
	return err
}

// SetState moves a release to a new state.
func SetState(ctx context.Context, conn *pgx.Conn, id, state string, dataTimestamp *time.Time) error {
	tag, err := conn.Exec(ctx, `
UPDATE registry.releases SET state = $2, data_timestamp = COALESCE($3, data_timestamp), updated_at = now()
WHERE release_id = $1`, id, state, dataTimestamp)
	if err == nil && tag.RowsAffected() != 1 {
		err = fmt.Errorf("release %s not found in registry", id)
	}
	return err
}

// Fail marks a release failed with a reason.
func Fail(ctx context.Context, conn *pgx.Conn, id, reason string) error {
	_, err := conn.Exec(ctx, `
UPDATE registry.releases SET state = 'failed', failure_reason = $2, updated_at = now() WHERE release_id = $1`, id, reason)
	if err == nil {
		err = Event(ctx, conn, id, "import_failed", map[string]string{"reason": reason})
	}
	return err
}

// ActivateFirst makes a ready release active when no release is active yet.
// It is the Stage 1 bootstrap; replacing an active release is Stage 2.
func ActivateFirst(ctx context.Context, conn *pgx.Conn, id string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `INSERT INTO registry.active_release (release_id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("another release is already active")
	}
	tag, err = tx.Exec(ctx, `
UPDATE registry.releases SET state = 'active', updated_at = now() WHERE release_id = $1 AND state = 'ready'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("release %s is not ready", id)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO registry.events (release_id, event) VALUES ($1, 'activated')`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Event appends an audit event.
func Event(ctx context.Context, conn *pgx.Conn, id, event string, detail any) error {
	_, err := conn.Exec(ctx, `INSERT INTO registry.events (release_id, event, detail) VALUES ($1, $2, $3)`, id, event, detail)
	return err
}
