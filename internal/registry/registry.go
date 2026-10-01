// Package registry is Karta's control plane: the list of releases, their
// state and the single active-release pointer, the publication submissions,
// operator digest authorizations and the audit log. It lives in its own
// database, separate from release databases.
//
// Release states (docs/adr/0003-stage2-publication.md):
//
//	importing -> validating -> ready -> active -> retired -> removing -> removed
//	         \-> failed                 ^   |       |
//	                        ready/retired --(activate, rollback)
//
// Only a single transaction changes the active pointer (Activate), under a
// transaction-scoped advisory lock and a compare-and-swap on the expected
// active release, and it writes the audit record in the same transaction.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Release states.
const (
	StateImporting  = "importing"
	StateValidating = "validating"
	StateReady      = "ready"
	StateActive     = "active"
	StateRetired    = "retired"
	StateFailed     = "failed"
	StateRemoving   = "removing"
	StateRemoved    = "removed"
)

// ReaderRole may read the registry and all release databases.
const ReaderRole = "karta_reader"

// Advisory lock keys.
const (
	// importLockKey serialises candidate builds across processes.
	importLockKey int64 = 0x6b61727461 // "karta"
	// pointerLockKey serialises changes of the active pointer (transaction scope).
	pointerLockKey int64 = 0x6b61727470 // "kartp"
	// migrationLockKey serialises schema migrations (transaction scope).
	migrationLockKey int64 = 0x6b6172746d // "kartm"
)

// Querier is satisfied by connections, pools and transactions.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TxBeginner starts transactions (connections and pools).
type TxBeginner interface {
	Querier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// migrations are applied in order; each runs once, in a transaction, and is
// recorded in registry.schema_migrations. Version 1 is the Stage 1 schema,
// written idempotently so a Stage 1 registry (which has no migrations table)
// is recognised and upgraded in place.
var migrations = []string{
	1: `
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
`,
	2: `
ALTER TABLE registry.releases DROP CONSTRAINT IF EXISTS releases_state_check;
ALTER TABLE registry.releases ADD CONSTRAINT releases_state_check
    CHECK (state IN ('importing', 'validating', 'ready', 'active', 'retired', 'failed', 'removing', 'removed'));
ALTER TABLE registry.releases
    ADD COLUMN IF NOT EXISTS source_size bigint,
    ADD COLUMN IF NOT EXISTS schema_major integer,
    ADD COLUMN IF NOT EXISTS database_bytes bigint,
    ADD COLUMN IF NOT EXISTS counts jsonb,
    ADD COLUMN IF NOT EXISTS submission_id bigint,
    ADD COLUMN IF NOT EXISTS activated_at timestamptz,
    ADD COLUMN IF NOT EXISTS deactivated_at timestamptz,
    ADD COLUMN IF NOT EXISTS pinned_until timestamptz,
    ADD COLUMN IF NOT EXISTS removed_at timestamptz;
UPDATE registry.releases r SET activated_at = a.activated_at
FROM registry.active_release a WHERE r.release_id = a.release_id AND r.activated_at IS NULL;
ALTER TABLE registry.active_release ADD COLUMN IF NOT EXISTS activated_by text;

CREATE TABLE registry.submissions (
    id bigserial PRIMARY KEY,
    source text NOT NULL CHECK (source IN ('inbox', 'cli')),
    name text NOT NULL,
    fingerprint text NOT NULL UNIQUE,
    region_id text NOT NULL,
    marker_sha256 text,
    sha256 text,
    size_bytes bigint,
    data_timestamp timestamptz,
    state text NOT NULL CHECK (state IN ('processing', 'published', 'ready', 'duplicate', 'rejected', 'failed', 'interrupted')),
    reason_code text,
    reason text,
    release_id text,
    attempts integer NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);

CREATE TABLE registry.authorizations (
    id bigserial PRIMARY KEY,
    region_id text NOT NULL,
    sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    size_bytes bigint CHECK (size_bytes > 0),
    reason text NOT NULL,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    revoked_at timestamptz,
    revoked_by text,
    revoke_reason text
);
CREATE UNIQUE INDEX authorizations_one_open ON registry.authorizations (region_id, sha256) WHERE revoked_at IS NULL;

CREATE TABLE registry.audit (
    id bigserial PRIMARY KEY,
    at timestamptz NOT NULL DEFAULT now(),
    actor text NOT NULL,
    source text NOT NULL CHECK (source IN ('operator_api', 'inbox', 'cli', 'system')),
    action text NOT NULL,
    target text,
    outcome text NOT NULL CHECK (outcome IN ('succeeded', 'rejected', 'failed', 'denied', 'noop')),
    reason text,
    request_id text,
    detail jsonb
);
CREATE INDEX audit_at ON registry.audit (at);
CREATE FUNCTION registry.audit_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'registry.audit is append-only';
END $$;
CREATE TRIGGER audit_append_only BEFORE UPDATE OR DELETE ON registry.audit
    FOR EACH ROW EXECUTE FUNCTION registry.audit_append_only();
CREATE TRIGGER audit_no_truncate BEFORE TRUNCATE ON registry.audit
    FOR EACH STATEMENT EXECUTE FUNCTION registry.audit_append_only();

-- Stage 1 events become the first audit records; registry.events is kept
-- unchanged and no longer written.
INSERT INTO registry.audit (at, actor, source, action, target, outcome, reason, detail)
SELECT at, 'stage1-importer', 'cli', event, release_id,
       CASE event WHEN 'import_failed' THEN 'failed' ELSE 'succeeded' END, detail->>'reason', detail
FROM registry.events ORDER BY id;

GRANT SELECT ON registry.schema_migrations TO karta_reader;
`,
	3: `
-- Stage 3: online deliveries are a third submission source and audit source.
ALTER TABLE registry.submissions DROP CONSTRAINT IF EXISTS submissions_source_check;
ALTER TABLE registry.submissions ADD CONSTRAINT submissions_source_check CHECK (source IN ('inbox', 'cli', 'online'));
ALTER TABLE registry.submissions
    ADD COLUMN IF NOT EXISTS manifest_serial bigint,
    ADD COLUMN IF NOT EXISTS manifest_sha256 text;
ALTER TABLE registry.audit DROP CONSTRAINT IF EXISTS audit_source_check;
ALTER TABLE registry.audit ADD CONSTRAINT audit_source_check
    CHECK (source IN ('operator_api', 'inbox', 'cli', 'system', 'online'));

-- The newest signed manifest the publisher verified, per region: a delivery
-- with a lower serial (a replay) or another manifest under the same serial
-- (a conflicting claim) is refused.
CREATE TABLE registry.source_state (
    region_id text PRIMARY KEY,
    last_serial bigint NOT NULL CHECK (last_serial > 0),
    envelope_sha256 text NOT NULL CHECK (envelope_sha256 ~ '^[0-9a-f]{64}$'),
    snapshot_sha256 text NOT NULL CHECK (snapshot_sha256 ~ '^[0-9a-f]{64}$'),
    snapshot_size bigint NOT NULL CHECK (snapshot_size > 0),
    data_timestamp timestamptz NOT NULL,
    issued_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    key_id text NOT NULL,
    submission_id bigint,
    verified_at timestamptz NOT NULL DEFAULT now()
);

-- Operator policy for online deliveries: automatic activation can be
-- paused (an operator, or automatically by a rollback), and one failed
-- delivery can be queued for another attempt.
CREATE TABLE registry.online_policy (
    region_id text PRIMARY KEY,
    auto_activate boolean NOT NULL DEFAULT true,
    changed_at timestamptz,
    changed_by text,
    change_reason text,
    retry_fingerprint text,
    retry_requested_at timestamptz
);

-- What the public manifest may say about freshness: the update mode and
-- the configured staleness threshold. Nothing about the upstream source.
CREATE TABLE registry.freshness (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    update_mode text NOT NULL CHECK (update_mode IN ('manual', 'online')),
    stale_after_seconds bigint CHECK (stale_after_seconds > 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT ON registry.freshness TO karta_reader;
`,
}

// SchemaVersion is the registry schema this build writes.
const SchemaVersion = 3

// Migrate brings the registry schema to SchemaVersion (idempotent; safe to
// run concurrently from several processes).
func Migrate(ctx context.Context, db TxBeginner) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
CREATE SCHEMA IF NOT EXISTS registry;
CREATE TABLE IF NOT EXISTS registry.schema_migrations (
    version integer PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
)`); err != nil {
		return err
	}
	var current int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version), 0) FROM registry.schema_migrations`).Scan(&current); err != nil {
		return err
	}
	if current > SchemaVersion {
		return fmt.Errorf("registry schema version %d is newer than this build supports (%d)", current, SchemaVersion)
	}
	for v := current + 1; v <= SchemaVersion; v++ {
		if _, err := tx.Exec(ctx, migrations[v]); err != nil {
			return fmt.Errorf("registry migration %d: %w", v, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO registry.schema_migrations (version) VALUES ($1)`, v); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ReadSchemaVersion returns the applied schema version: 0 for an empty
// registry, 1 for a Stage 1 registry that was never migrated by Stage 2.
func ReadSchemaVersion(ctx context.Context, q Querier) (int, error) {
	var hasMigrations, hasReleases bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('registry.schema_migrations') IS NOT NULL, to_regclass('registry.releases') IS NOT NULL`).Scan(&hasMigrations, &hasReleases); err != nil {
		return 0, err
	}
	if !hasMigrations {
		if hasReleases {
			return 1, nil
		}
		return 0, nil
	}
	var v int
	err := q.QueryRow(ctx, `SELECT COALESCE(max(version), 0) FROM registry.schema_migrations`).Scan(&v)
	return v, err
}

// LockImports takes the session-level build lock, waiting at most timeout.
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
			return ErrBusy
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// UnlockImports releases the build lock.
func UnlockImports(ctx context.Context, conn *pgx.Conn) error {
	_, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, importLockKey)
	return err
}

// Errors of pointer changes.
var (
	ErrBusy          = errors.New("another publication operation holds the lock")
	ErrActiveChanged = errors.New("the active release changed")
	ErrNotEligible   = errors.New("release is not eligible")
	ErrNotFound      = errors.New("release not found")
)

// Release is one registry row.
type Release struct {
	ID             string           `json:"release_id"`
	Database       string           `json:"database"`
	State          string           `json:"state"`
	RegionID       string           `json:"region_id"`
	SourceSHA256   string           `json:"source_sha256"`
	SourceSize     *int64           `json:"source_size_bytes"`
	SchemaRevision string           `json:"schema_revision"`
	StyleRevision  string           `json:"style_revision"`
	SchemaMajor    *int             `json:"schema_major"`
	DataTimestamp  *time.Time       `json:"data_timestamp"`
	FailureReason  *string          `json:"failure_reason"`
	DatabaseBytes  *int64           `json:"database_bytes"`
	Counts         map[string]int64 `json:"counts,omitempty"`
	SubmissionID   *int64           `json:"submission_id"`
	ActivatedAt    *time.Time       `json:"activated_at"`
	DeactivatedAt  *time.Time       `json:"deactivated_at"`
	PinnedUntil    *time.Time       `json:"pinned_until"`
	RemovedAt      *time.Time       `json:"removed_at"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

const releaseColumns = `release_id, database_name, state, region_id, source_sha256, source_size, schema_revision,
    style_revision, schema_major, data_timestamp, failure_reason, database_bytes, counts, submission_id,
    activated_at, deactivated_at, pinned_until, removed_at, created_at, updated_at`

func scanRelease(row pgx.Row) (*Release, error) {
	var r Release
	var counts []byte
	err := row.Scan(&r.ID, &r.Database, &r.State, &r.RegionID, &r.SourceSHA256, &r.SourceSize, &r.SchemaRevision,
		&r.StyleRevision, &r.SchemaMajor, &r.DataTimestamp, &r.FailureReason, &r.DatabaseBytes, &counts, &r.SubmissionID,
		&r.ActivatedAt, &r.DeactivatedAt, &r.PinnedUntil, &r.RemovedAt, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if len(counts) > 0 {
		if err := json.Unmarshal(counts, &r.Counts); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

// Get returns a release by id, or nil.
func Get(ctx context.Context, q Querier, id string) (*Release, error) {
	r, err := scanRelease(q.QueryRow(ctx, `SELECT `+releaseColumns+` FROM registry.releases WHERE release_id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// Active returns the active release, or nil.
func Active(ctx context.Context, q Querier) (*Release, error) {
	r, err := scanRelease(q.QueryRow(ctx, `SELECT `+releaseColumns+`
FROM registry.releases WHERE release_id = (SELECT release_id FROM registry.active_release)`))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// List returns releases, newest first, at most limit.
func List(ctx context.Context, q Querier, limit int) ([]Release, error) {
	rows, err := q.Query(ctx, `SELECT `+releaseColumns+` FROM registry.releases ORDER BY created_at DESC, release_id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ServedRelease is what the API needs to decide whether it may serve a
// release: the active one, or a retired one whose pin grace has not ended.
type ServedRelease struct {
	ID          string
	Database    string
	State       string
	ActivatedAt *time.Time
	PinnedUntil *time.Time
}

// Serving reads the active pointer and every release that is or was
// published, for the API. It works on a Stage 1 (version 1) registry too,
// where only the active release is known.
func Serving(ctx context.Context, q Querier) (active *ServedRelease, all []ServedRelease, err error) {
	v, err := ReadSchemaVersion(ctx, q)
	if err != nil || v == 0 {
		return nil, nil, err
	}
	if v == 1 {
		var s ServedRelease
		err := q.QueryRow(ctx, `
SELECT r.release_id, r.database_name, r.state, a.activated_at
FROM registry.active_release a JOIN registry.releases r USING (release_id)`).Scan(&s.ID, &s.Database, &s.State, &s.ActivatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, err
		}
		return &s, []ServedRelease{s}, nil
	}
	rows, err := q.Query(ctx, `
SELECT r.release_id, r.database_name, r.state, COALESCE(r.activated_at, a.activated_at), r.pinned_until,
       a.release_id IS NOT NULL
FROM registry.releases r LEFT JOIN registry.active_release a USING (release_id)
WHERE r.state IN ('active', 'retired', 'removing', 'removed')`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s ServedRelease
		var isActive bool
		if err := rows.Scan(&s.ID, &s.Database, &s.State, &s.ActivatedAt, &s.PinnedUntil, &isActive); err != nil {
			return nil, nil, err
		}
		all = append(all, s)
		if isActive {
			c := s
			active = &c
		}
	}
	return active, all, rows.Err()
}

// BeginImport records a release entering import, replacing a previous
// failed, interrupted or removed attempt for the same id.
func BeginImport(ctx context.Context, q Querier, r Release, actor, source string) error {
	tag, err := q.Exec(ctx, `
INSERT INTO registry.releases (release_id, database_name, state, region_id, source_sha256, source_size, schema_revision,
    style_revision, submission_id)
VALUES ($1, $2, 'importing', $3, $4, $5, $6, $7, $8)
ON CONFLICT (release_id) DO UPDATE SET state = 'importing', failure_reason = NULL, source_size = EXCLUDED.source_size,
    submission_id = EXCLUDED.submission_id, activated_at = NULL, deactivated_at = NULL, pinned_until = NULL,
    removed_at = NULL, database_bytes = NULL, counts = NULL, updated_at = now()
WHERE registry.releases.state IN ('failed', 'removed', 'importing', 'validating')`,
		r.ID, r.Database, r.RegionID, r.SourceSHA256, r.SourceSize, r.SchemaRevision, r.StyleRevision, r.SubmissionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: release %s already exists and is not a failed or removed attempt", ErrNotEligible, r.ID)
	}
	return Audit(ctx, q, AuditEntry{Actor: actor, Source: source, Action: "import_started", Target: r.ID, Outcome: OutcomeSucceeded})
}

// SetValidating records the end of the data import.
func SetValidating(ctx context.Context, q Querier, id string, dataTimestamp time.Time) error {
	return oneRow(q.Exec(ctx, `
UPDATE registry.releases SET state = 'validating', data_timestamp = $2, updated_at = now()
WHERE release_id = $1 AND state = 'importing'`, id, dataTimestamp))
}

// MarkReady records a validated, frozen release database.
func MarkReady(ctx context.Context, q Querier, id string, schemaMajor int, dbBytes int64, counts map[string]int64) error {
	c, err := json.Marshal(counts)
	if err != nil {
		return err
	}
	return oneRow(q.Exec(ctx, `
UPDATE registry.releases SET state = 'ready', schema_major = $2, database_bytes = $3, counts = $4, updated_at = now()
WHERE release_id = $1 AND state = 'validating'`, id, schemaMajor, dbBytes, c))
}

func oneRow(tag pgconn.CommandTag, err error) error {
	if err == nil && tag.RowsAffected() != 1 {
		err = fmt.Errorf("%w: no release in the expected state", ErrNotEligible)
	}
	return err
}

// Fail marks an importing or validating release failed.
func Fail(ctx context.Context, q Querier, id, reason, actor, source string) error {
	if _, err := q.Exec(ctx, `
UPDATE registry.releases SET state = 'failed', failure_reason = $2, updated_at = now()
WHERE release_id = $1 AND state IN ('importing', 'validating', 'ready')`, id, reason); err != nil {
		return err
	}
	return Audit(ctx, q, AuditEntry{Actor: actor, Source: source, Action: "import_failed", Target: id, Outcome: OutcomeFailed, Reason: reason})
}

// ActivateRequest describes one change of the active pointer.
type ActivateRequest struct {
	Target string
	// Expected is the release that must be active for the change to apply
	// ("" = none); it is only checked when CheckExpected is set.
	Expected      string
	CheckExpected bool
	// Action is the audit action: publish, activate or rollback.
	Action    string
	Actor     string
	Source    string
	Reason    string
	RequestID string
	// PinGrace is how long the release being replaced stays servable to
	// clients that pinned it.
	PinGrace time.Duration
	// Allow is the caller's policy check, run inside the transaction with
	// the current active release (nil if none) and the locked target row.
	Allow func(active, target *Release) error
	// Gate, if set, is a further check inside the transaction, after Allow,
	// that can read the registry (for example the online activation
	// policy, read with a row lock so a concurrent pause is serialized).
	Gate func(ctx context.Context, q Querier) error
	// InTx, if set, runs inside the transaction after the pointer moved and
	// before the commit, so its changes commit or roll back with the switch
	// (a rollback pausing automatic online activation).
	InTx func(ctx context.Context, q Querier) error
	// BeforeCommit runs last inside the transaction (fault injection in tests).
	BeforeCommit func()
}

// ActivateResult reports what changed.
type ActivateResult struct {
	Previous    string     `json:"previous_release_id"`
	Active      string     `json:"active_release_id"`
	Changed     bool       `json:"changed"`
	PinnedUntil *time.Time `json:"previous_pinned_until"`
}

// Activate switches the active pointer to req.Target in one transaction: it
// takes the pointer lock (waiting at most lockTimeout), checks the expected
// active release, the target's state (ready or retired) and the caller's
// policy, retires the previous release with pinned_until = now + PinGrace,
// and records the audit event. A failure changes nothing.
func Activate(ctx context.Context, db TxBeginner, req ActivateRequest, lockTimeout time.Duration) (ActivateResult, error) {
	var res ActivateResult
	tx, err := db.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, lockTimeout.Milliseconds())); err != nil {
		return res, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, pointerLockKey); err != nil {
		if isLockTimeout(err) {
			return res, ErrBusy
		}
		return res, err
	}
	active, err := Active(ctx, tx)
	if err != nil {
		return res, err
	}
	cur := ""
	if active != nil {
		cur = active.ID
	}
	res.Previous, res.Active = cur, cur
	if req.CheckExpected && cur != req.Expected {
		return res, fmt.Errorf("%w: expected %s, found %s", ErrActiveChanged, orNone(req.Expected), orNone(cur))
	}
	if cur == req.Target {
		if err := Audit(ctx, tx, AuditEntry{Actor: req.Actor, Source: req.Source, Action: req.Action, Target: req.Target,
			Outcome: OutcomeNoop, Reason: req.Reason, RequestID: req.RequestID, Detail: map[string]any{"note": "already active"}}); err != nil {
			return res, err
		}
		return res, tx.Commit(ctx)
	}
	target, err := scanRelease(tx.QueryRow(ctx, `SELECT `+releaseColumns+` FROM registry.releases WHERE release_id = $1 FOR UPDATE`, req.Target))
	if errors.Is(err, pgx.ErrNoRows) {
		return res, fmt.Errorf("%w: %s", ErrNotFound, req.Target)
	}
	if err != nil {
		if isLockTimeout(err) {
			return res, ErrBusy
		}
		return res, err
	}
	if target.State != StateReady && target.State != StateRetired {
		return res, fmt.Errorf("%w: release %s is %s; only ready or retired (retained, validated) releases can be activated", ErrNotEligible, target.ID, target.State)
	}
	if req.Allow != nil {
		if err := req.Allow(active, target); err != nil {
			return res, err
		}
	}
	if req.Gate != nil {
		if err := req.Gate(ctx, tx); err != nil {
			if isLockTimeout(err) {
				return res, ErrBusy
			}
			return res, err
		}
	}
	if active != nil {
		var until time.Time
		if err := tx.QueryRow(ctx, `
UPDATE registry.releases SET state = 'retired', deactivated_at = now(),
    pinned_until = now() + make_interval(secs => $2), updated_at = now()
WHERE release_id = $1 RETURNING pinned_until`, active.ID, req.PinGrace.Seconds()).Scan(&until); err != nil {
			return res, err
		}
		res.PinnedUntil = &until
	}
	if err := oneRow(tx.Exec(ctx, `
UPDATE registry.releases SET state = 'active', activated_at = now(), deactivated_at = NULL, pinned_until = NULL, updated_at = now()
WHERE release_id = $1`, target.ID)); err != nil {
		return res, err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO registry.active_release (singleton, release_id, activated_at, activated_by) VALUES (true, $1, now(), $2)
ON CONFLICT (singleton) DO UPDATE SET release_id = EXCLUDED.release_id, activated_at = now(), activated_by = EXCLUDED.activated_by`,
		target.ID, req.Actor); err != nil {
		return res, err
	}
	detail := map[string]any{"previous_release_id": nullable(cur), "activated_release_id": target.ID}
	if res.PinnedUntil != nil {
		detail["previous_pinned_until"] = res.PinnedUntil.UTC().Format(time.RFC3339)
	}
	if err := Audit(ctx, tx, AuditEntry{Actor: req.Actor, Source: req.Source, Action: req.Action, Target: target.ID,
		Outcome: OutcomeSucceeded, Reason: req.Reason, RequestID: req.RequestID, Detail: detail}); err != nil {
		return res, err
	}
	if req.InTx != nil {
		if err := req.InTx(ctx, tx); err != nil {
			return res, err
		}
	}
	if req.BeforeCommit != nil {
		req.BeforeCommit()
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}
	res.Active, res.Changed = target.ID, true
	return res, nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

// MarkRemoving takes a release out of retention before its database is
// dropped. It re-checks eligibility under the row lock: never the active
// release, never one whose pin grace (plus margin) has not ended, only ready,
// retired, failed or already-removing releases. It reports false when the
// release is no longer eligible.
func MarkRemoving(ctx context.Context, db TxBeginner, id string, margin time.Duration, actor, source, reason, requestID string) (bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return false, err
	}
	var state string
	var eligible bool
	err = tx.QueryRow(ctx, `
SELECT state,
       state IN ('ready', 'retired', 'failed', 'removing')
       AND release_id IS DISTINCT FROM (SELECT release_id FROM registry.active_release)
       AND (pinned_until IS NULL OR pinned_until < now() - make_interval(secs => $2))
FROM registry.releases WHERE release_id = $1 FOR UPDATE`, id, margin.Seconds()).Scan(&state, &eligible)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !eligible) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if state != StateRemoving {
		if _, err := tx.Exec(ctx, `UPDATE registry.releases SET state = 'removing', updated_at = now() WHERE release_id = $1`, id); err != nil {
			return false, err
		}
		if err := Audit(ctx, tx, AuditEntry{Actor: actor, Source: source, Action: "remove_release", Target: id,
			Outcome: OutcomeSucceeded, Reason: reason, RequestID: requestID, Detail: map[string]any{"phase": "marked", "previous_state": state}}); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

// MarkRemoved records that a removing release's database is gone.
func MarkRemoved(ctx context.Context, q Querier, id string) error {
	return oneRow(q.Exec(ctx, `
UPDATE registry.releases SET state = 'removed', removed_at = now(), database_bytes = NULL, updated_at = now()
WHERE release_id = $1 AND state = 'removing'`, id))
}

// Submission is one inbox, CLI or online submission.
type Submission struct {
	ID            int64      `json:"id"`
	Source        string     `json:"source"`
	Name          string     `json:"name"`
	Fingerprint   string     `json:"fingerprint"`
	RegionID      string     `json:"region_id"`
	MarkerSHA256  *string    `json:"marker_sha256"`
	SHA256        *string    `json:"sha256"`
	SizeBytes     *int64     `json:"size_bytes"`
	DataTimestamp *time.Time `json:"data_timestamp"`
	State         string     `json:"state"`
	ReasonCode    *string    `json:"reason_code"`
	Reason        *string    `json:"reason"`
	ReleaseID     *string    `json:"release_id"`
	Attempts      int        `json:"attempts"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	// ManifestSerial and ManifestSHA256 identify the signed manifest of an
	// online delivery (null otherwise).
	ManifestSerial *int64  `json:"manifest_serial"`
	ManifestSHA256 *string `json:"manifest_sha256"`
}

// Submission states.
const (
	SubProcessing  = "processing"
	SubPublished   = "published"
	SubReady       = "ready"
	SubDuplicate   = "duplicate"
	SubRejected    = "rejected"
	SubFailed      = "failed"
	SubInterrupted = "interrupted"
)

const submissionColumns = `id, source, name, fingerprint, region_id, marker_sha256, sha256, size_bytes, data_timestamp,
    state, reason_code, reason, release_id, attempts, created_at, updated_at, finished_at, manifest_serial, manifest_sha256`

func scanSubmission(row pgx.Row) (*Submission, error) {
	var s Submission
	err := row.Scan(&s.ID, &s.Source, &s.Name, &s.Fingerprint, &s.RegionID, &s.MarkerSHA256, &s.SHA256, &s.SizeBytes,
		&s.DataTimestamp, &s.State, &s.ReasonCode, &s.Reason, &s.ReleaseID, &s.Attempts, &s.CreatedAt, &s.UpdatedAt, &s.FinishedAt,
		&s.ManifestSerial, &s.ManifestSHA256)
	return &s, err
}

// SubmissionByFingerprint returns the record of a fingerprint, or nil.
func SubmissionByFingerprint(ctx context.Context, q Querier, fingerprint string) (*Submission, error) {
	s, err := scanSubmission(q.QueryRow(ctx, `SELECT `+submissionColumns+` FROM registry.submissions WHERE fingerprint = $1`, fingerprint))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

// ClaimSubmission records a new submission as processing and returns it. If
// the fingerprint is already recorded, it re-claims it only when retry
// allows the existing record (for example an interrupted attempt below the
// attempt limit) and otherwise returns claimed = false with the record.
func ClaimSubmission(ctx context.Context, q Querier, s Submission, retry func(*Submission) bool) (*Submission, bool, error) {
	got, err := scanSubmission(q.QueryRow(ctx, `
INSERT INTO registry.submissions (source, name, fingerprint, region_id, state) VALUES ($1, $2, $3, $4, 'processing')
ON CONFLICT (fingerprint) DO NOTHING RETURNING `+submissionColumns, s.Source, s.Name, s.Fingerprint, s.RegionID))
	if err == nil {
		return got, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	prior, err := SubmissionByFingerprint(ctx, q, s.Fingerprint)
	if err != nil || prior == nil {
		return prior, false, err
	}
	if retry == nil || !retry(prior) {
		return prior, false, nil
	}
	got, err = scanSubmission(q.QueryRow(ctx, `
UPDATE registry.submissions SET state = 'processing', attempts = attempts + 1, reason_code = NULL, reason = NULL,
    finished_at = NULL, updated_at = now()
WHERE id = $1 AND state = $2 RETURNING `+submissionColumns, prior.ID, prior.State))
	if errors.Is(err, pgx.ErrNoRows) {
		return prior, false, nil // claimed concurrently
	}
	return got, err == nil, err
}

// SubmissionUpdate is the outcome written to a submission.
type SubmissionUpdate struct {
	State          string
	ReasonCode     string
	Reason         string
	ReleaseID      string
	MarkerSHA256   string
	SHA256         string
	SizeBytes      int64
	DataTimestamp  *time.Time
	ManifestSerial int64
	ManifestSHA256 string
}

// UpdateSubmission records progress or the outcome of a submission.
func UpdateSubmission(ctx context.Context, q Querier, id int64, u SubmissionUpdate) error {
	terminal := u.State != SubProcessing
	_, err := q.Exec(ctx, `
UPDATE registry.submissions SET state = $2, reason_code = NULLIF($3, ''), reason = NULLIF($4, ''),
    release_id = COALESCE(NULLIF($5, ''), release_id), marker_sha256 = COALESCE(NULLIF($6, ''), marker_sha256),
    sha256 = COALESCE(NULLIF($7, ''), sha256), size_bytes = COALESCE(NULLIF($8, 0), size_bytes),
    data_timestamp = COALESCE($9, data_timestamp), updated_at = now(),
    finished_at = CASE WHEN $10 THEN now() ELSE NULL END,
    manifest_serial = COALESCE(NULLIF($11, 0), manifest_serial), manifest_sha256 = COALESCE(NULLIF($12, ''), manifest_sha256)
WHERE id = $1`, id, u.State, u.ReasonCode, truncate(u.Reason, 2000), u.ReleaseID, u.MarkerSHA256, u.SHA256, u.SizeBytes, u.DataTimestamp, terminal,
		u.ManifestSerial, u.ManifestSHA256)
	return err
}

// ListSubmissions returns the most recent submissions.
func ListSubmissions(ctx context.Context, q Querier, limit int) ([]Submission, error) {
	rows, err := q.Query(ctx, `SELECT `+submissionColumns+` FROM registry.submissions ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Submission
	for rows.Next() {
		s, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// Authorization is an operator's approval of one exact snapshot digest.
type Authorization struct {
	ID           int64      `json:"id"`
	RegionID     string     `json:"region_id"`
	SHA256       string     `json:"sha256"`
	SizeBytes    *int64     `json:"size_bytes"`
	Reason       string     `json:"reason"`
	CreatedBy    string     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    *time.Time `json:"expires_at"`
	RevokedAt    *time.Time `json:"revoked_at"`
	RevokedBy    *string    `json:"revoked_by"`
	RevokeReason *string    `json:"revoke_reason"`
}

const authColumns = `id, region_id, sha256, size_bytes, reason, created_by, created_at, expires_at, revoked_at, revoked_by, revoke_reason`

func scanAuth(row pgx.Row) (*Authorization, error) {
	var a Authorization
	err := row.Scan(&a.ID, &a.RegionID, &a.SHA256, &a.SizeBytes, &a.Reason, &a.CreatedBy, &a.CreatedAt, &a.ExpiresAt, &a.RevokedAt, &a.RevokedBy, &a.RevokeReason)
	return &a, err
}

// Authorize records an authorization for (region, digest). An open,
// unexpired authorization for the same digest is returned unchanged
// (created = false); an expired one is closed first.
func Authorize(ctx context.Context, db TxBeginner, a Authorization, requestID string) (*Authorization, bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	existing, err := scanAuth(tx.QueryRow(ctx, `SELECT `+authColumns+` FROM registry.authorizations
WHERE region_id = $1 AND sha256 = $2 AND revoked_at IS NULL FOR UPDATE`, a.RegionID, a.SHA256))
	switch {
	case err == nil && (existing.ExpiresAt == nil || existing.ExpiresAt.After(time.Now())) &&
		sameSize(existing.SizeBytes, a.SizeBytes):
		if err := Audit(ctx, tx, AuditEntry{Actor: a.CreatedBy, Source: "operator_api", Action: "authorize_digest", Target: a.SHA256,
			Outcome: OutcomeNoop, Reason: a.Reason, RequestID: requestID, Detail: map[string]any{"region_id": a.RegionID, "authorization_id": existing.ID}}); err != nil {
			return nil, false, err
		}
		return existing, false, tx.Commit(ctx)
	case err == nil:
		if _, err := tx.Exec(ctx, `UPDATE registry.authorizations SET revoked_at = now(), revoked_by = $2, revoke_reason = $3 WHERE id = $1`,
			existing.ID, a.CreatedBy, "superseded"); err != nil {
			return nil, false, err
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, false, err
	}
	created, err := scanAuth(tx.QueryRow(ctx, `
INSERT INTO registry.authorizations (region_id, sha256, size_bytes, reason, created_by, expires_at)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+authColumns, a.RegionID, a.SHA256, a.SizeBytes, a.Reason, a.CreatedBy, a.ExpiresAt))
	if err != nil {
		return nil, false, err
	}
	detail := map[string]any{"region_id": a.RegionID, "authorization_id": created.ID}
	if a.SizeBytes != nil {
		detail["size_bytes"] = *a.SizeBytes
	}
	if a.ExpiresAt != nil {
		detail["expires_at"] = a.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if err := Audit(ctx, tx, AuditEntry{Actor: a.CreatedBy, Source: "operator_api", Action: "authorize_digest", Target: a.SHA256,
		Outcome: OutcomeSucceeded, Reason: a.Reason, RequestID: requestID, Detail: detail}); err != nil {
		return nil, false, err
	}
	return created, true, tx.Commit(ctx)
}

func sameSize(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// Revoke closes the open authorization for (region, digest).
func Revoke(ctx context.Context, db TxBeginner, region, digest, actor, reason, requestID string) (bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	tag, err := tx.Exec(ctx, `UPDATE registry.authorizations SET revoked_at = now(), revoked_by = $3, revoke_reason = $4
WHERE region_id = $1 AND sha256 = $2 AND revoked_at IS NULL`, region, digest, actor, reason)
	if err != nil {
		return false, err
	}
	outcome := OutcomeSucceeded
	if tag.RowsAffected() == 0 {
		outcome = OutcomeNoop
	}
	if err := Audit(ctx, tx, AuditEntry{Actor: actor, Source: "operator_api", Action: "revoke_digest", Target: digest,
		Outcome: outcome, Reason: reason, RequestID: requestID, Detail: map[string]any{"region_id": region}}); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, tx.Commit(ctx)
}

// Authorized reports whether an open, unexpired authorization covers the
// digest (and size, when the authorization records one). A registry without
// the table (version < 2) authorizes nothing.
func Authorized(ctx context.Context, q Querier, region, digest string, size int64) (*Authorization, error) {
	a, err := scanAuth(q.QueryRow(ctx, `SELECT `+authColumns+` FROM registry.authorizations
WHERE region_id = $1 AND sha256 = $2 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())
  AND (size_bytes IS NULL OR size_bytes = $3)`, region, digest, size))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "3F000") {
		return nil, nil
	}
	return a, err
}

// ListAuthorizations returns open authorizations of a region, newest first.
func ListAuthorizations(ctx context.Context, q Querier, region string) ([]Authorization, error) {
	rows, err := q.Query(ctx, `SELECT `+authColumns+` FROM registry.authorizations
WHERE region_id = $1 AND revoked_at IS NULL ORDER BY id DESC LIMIT 100`, region)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Authorization
	for rows.Next() {
		a, err := scanAuth(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// Audit outcomes.
const (
	OutcomeSucceeded = "succeeded"
	OutcomeRejected  = "rejected"
	OutcomeFailed    = "failed"
	OutcomeDenied    = "denied"
	OutcomeNoop      = "noop"
)

// AuditEntry is one append-only audit record: who did what to which target,
// with what outcome and why.
type AuditEntry struct {
	ID        int64     `json:"id"`
	At        time.Time `json:"at"`
	Actor     string    `json:"actor"`
	Source    string    `json:"source"`
	Action    string    `json:"action"`
	Target    string    `json:"target,omitempty"`
	Outcome   string    `json:"outcome"`
	Reason    string    `json:"reason,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
	Detail    any       `json:"detail,omitempty"`
}

// Audit appends an audit record.
func Audit(ctx context.Context, q Querier, e AuditEntry) error {
	var detail []byte
	if e.Detail != nil {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return err
		}
		detail = b
	}
	_, err := q.Exec(ctx, `
INSERT INTO registry.audit (actor, source, action, target, outcome, reason, request_id, detail)
VALUES ($1, $2, $3, NULLIF($4, ''), $5, NULLIF($6, ''), NULLIF($7, ''), $8)`,
		truncate(e.Actor, 64), e.Source, e.Action, truncate(e.Target, 200), e.Outcome, truncate(e.Reason, 2000), truncate(e.RequestID, 64), detail)
	return err
}

// ListAudit returns audit records, newest first, before an id (0 = latest).
func ListAudit(ctx context.Context, q Querier, limit int, beforeID int64) ([]AuditEntry, error) {
	rows, err := q.Query(ctx, `
SELECT id, at, actor, source, action, COALESCE(target, ''), outcome, COALESCE(reason, ''), COALESCE(request_id, ''), detail
FROM registry.audit WHERE $2 = 0 OR id < $2 ORDER BY id DESC LIMIT $1`, limit, beforeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var detail []byte
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Source, &e.Action, &e.Target, &e.Outcome, &e.Reason, &e.RequestID, &detail); err != nil {
			return nil, err
		}
		if len(detail) > 0 {
			var d any
			if json.Unmarshal(detail, &d) == nil {
				e.Detail = d
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a UTF-8 boundary.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
