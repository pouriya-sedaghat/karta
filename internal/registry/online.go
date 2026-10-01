package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Errors of online manifest bookkeeping.
var (
	// ErrManifestReplayed is a verified manifest with a lower serial than
	// one already verified.
	ErrManifestReplayed = errors.New("manifest serial is lower than one already verified")
	// ErrManifestConflict is a different manifest under a serial already
	// verified.
	ErrManifestConflict = errors.New("a different manifest was verified under the same serial")
)

// SourceState is the newest signed manifest the publisher verified.
type SourceState struct {
	RegionID       string    `json:"region_id"`
	Serial         int64     `json:"serial"`
	EnvelopeSHA256 string    `json:"envelope_sha256"`
	SnapshotSHA256 string    `json:"snapshot_sha256"`
	SnapshotSize   int64     `json:"snapshot_size_bytes"`
	DataTimestamp  time.Time `json:"data_timestamp"`
	IssuedAt       time.Time `json:"issued_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	KeyID          string    `json:"key_id"`
	SubmissionID   *int64    `json:"submission_id"`
	VerifiedAt     time.Time `json:"verified_at"`
}

const sourceStateColumns = `region_id, last_serial, envelope_sha256, snapshot_sha256, snapshot_size, data_timestamp, issued_at,
    expires_at, key_id, submission_id, verified_at`

func scanSourceState(row pgx.Row) (*SourceState, error) {
	var s SourceState
	err := row.Scan(&s.RegionID, &s.Serial, &s.EnvelopeSHA256, &s.SnapshotSHA256, &s.SnapshotSize, &s.DataTimestamp, &s.IssuedAt,
		&s.ExpiresAt, &s.KeyID, &s.SubmissionID, &s.VerifiedAt)
	return &s, err
}

// GetSourceState returns the region's verified manifest state, or nil.
func GetSourceState(ctx context.Context, q Querier, regionID string) (*SourceState, error) {
	s, err := scanSourceState(q.QueryRow(ctx, `SELECT `+sourceStateColumns+` FROM registry.source_state WHERE region_id = $1`, regionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

// RecordManifest records a verified manifest if its serial is higher than
// the region's last one; the same manifest again (same serial and envelope
// digest) is accepted unchanged. A lower serial is ErrManifestReplayed, a
// different envelope under the same serial ErrManifestConflict.
func RecordManifest(ctx context.Context, db TxBeginner, s SourceState) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	cur, err := scanSourceState(tx.QueryRow(ctx, `SELECT `+sourceStateColumns+` FROM registry.source_state WHERE region_id = $1 FOR UPDATE`, s.RegionID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		cur = nil
	case err != nil:
		return err
	}
	if cur != nil {
		switch {
		case s.Serial < cur.Serial:
			return fmt.Errorf("%w: serial %d, already verified %d", ErrManifestReplayed, s.Serial, cur.Serial)
		case s.Serial == cur.Serial && s.EnvelopeSHA256 != cur.EnvelopeSHA256:
			return fmt.Errorf("%w: serial %d", ErrManifestConflict, s.Serial)
		case s.Serial == cur.Serial:
			return tx.Commit(ctx)
		}
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO registry.source_state (region_id, last_serial, envelope_sha256, snapshot_sha256, snapshot_size, data_timestamp,
    issued_at, expires_at, key_id, submission_id, verified_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
ON CONFLICT (region_id) DO UPDATE SET last_serial = EXCLUDED.last_serial, envelope_sha256 = EXCLUDED.envelope_sha256,
    snapshot_sha256 = EXCLUDED.snapshot_sha256, snapshot_size = EXCLUDED.snapshot_size, data_timestamp = EXCLUDED.data_timestamp,
    issued_at = EXCLUDED.issued_at, expires_at = EXCLUDED.expires_at, key_id = EXCLUDED.key_id,
    submission_id = EXCLUDED.submission_id, verified_at = now()`,
		s.RegionID, s.Serial, s.EnvelopeSHA256, s.SnapshotSHA256, s.SnapshotSize, s.DataTimestamp, s.IssuedAt, s.ExpiresAt,
		truncate(s.KeyID, 64), s.SubmissionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// OnlinePolicy is the operator policy for online deliveries.
type OnlinePolicy struct {
	RegionID         string     `json:"region_id"`
	AutoActivate     bool       `json:"auto_activate"`
	ChangedAt        *time.Time `json:"changed_at"`
	ChangedBy        *string    `json:"changed_by"`
	ChangeReason     *string    `json:"change_reason"`
	RetryFingerprint *string    `json:"-"`
	RetryRequestedAt *time.Time `json:"retry_requested_at"`
}

const policyColumns = `region_id, auto_activate, changed_at, changed_by, change_reason, retry_fingerprint, retry_requested_at`

func scanPolicy(row pgx.Row) (*OnlinePolicy, error) {
	var p OnlinePolicy
	err := row.Scan(&p.RegionID, &p.AutoActivate, &p.ChangedAt, &p.ChangedBy, &p.ChangeReason, &p.RetryFingerprint, &p.RetryRequestedAt)
	return &p, err
}

// EnsureOnlinePolicy creates the region's policy row (automatic activation
// on) if it does not exist.
func EnsureOnlinePolicy(ctx context.Context, q Querier, regionID string) error {
	_, err := q.Exec(ctx, `INSERT INTO registry.online_policy (region_id) VALUES ($1) ON CONFLICT (region_id) DO NOTHING`, regionID)
	return err
}

// GetOnlinePolicy returns the region's policy (automatic activation on if
// no row exists).
func GetOnlinePolicy(ctx context.Context, q Querier, regionID string) (*OnlinePolicy, error) {
	p, err := scanPolicy(q.QueryRow(ctx, `SELECT `+policyColumns+` FROM registry.online_policy WHERE region_id = $1`, regionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return &OnlinePolicy{RegionID: regionID, AutoActivate: true}, nil
	}
	return p, err
}

// OnlineAutoActivateLocked reads whether automatic activation is on, with a
// share lock on the policy row, inside an activation transaction: a pause
// that commits first is seen, one that waits commits after the switch.
func OnlineAutoActivateLocked(ctx context.Context, q Querier, regionID string) (bool, error) {
	var on bool
	err := q.QueryRow(ctx, `SELECT auto_activate FROM registry.online_policy WHERE region_id = $1 FOR SHARE`, regionID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	return on, err
}

// SetOnlineAutoActivate pauses (on = false) or resumes automatic activation
// and audits the change; a request that changes nothing is audited as a
// noop. It reports whether the policy changed.
func SetOnlineAutoActivate(ctx context.Context, q Querier, regionID string, on bool, actor, source, reason, requestID string, detail map[string]any) (bool, error) {
	if err := EnsureOnlinePolicy(ctx, q, regionID); err != nil {
		return false, err
	}
	tag, err := q.Exec(ctx, `
UPDATE registry.online_policy SET auto_activate = $2, changed_at = now(), changed_by = $3, change_reason = $4
WHERE region_id = $1 AND auto_activate IS DISTINCT FROM $2`, regionID, on, truncate(actor, 64), truncate(reason, 2000))
	if err != nil {
		return false, err
	}
	action := "online_resume"
	if !on {
		action = "online_pause"
	}
	outcome := OutcomeSucceeded
	if tag.RowsAffected() == 0 {
		outcome = OutcomeNoop
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["region_id"] = regionID
	detail["auto_activate"] = on
	if err := Audit(ctx, q, AuditEntry{Actor: actor, Source: source, Action: action, Target: regionID, Outcome: outcome,
		Reason: reason, RequestID: requestID, Detail: detail}); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RequestOnlineRetry queues one more attempt of the online submission with
// this fingerprint.
func RequestOnlineRetry(ctx context.Context, q Querier, regionID, fingerprint string) error {
	if err := EnsureOnlinePolicy(ctx, q, regionID); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `UPDATE registry.online_policy SET retry_fingerprint = $2, retry_requested_at = now() WHERE region_id = $1`,
		regionID, fingerprint)
	return err
}

// LatestSubmission returns the newest submission of a source, or nil.
func LatestSubmission(ctx context.Context, q Querier, source string) (*Submission, error) {
	s, err := scanSubmission(q.QueryRow(ctx, `SELECT `+submissionColumns+` FROM registry.submissions WHERE source = $1 ORDER BY id DESC LIMIT 1`, source))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

// Freshness is what the public manifest says about updates.
type Freshness struct {
	// UpdateMode is "manual" or "online".
	UpdateMode string
	// StaleAfter is the configured staleness threshold (nil: none).
	StaleAfter *time.Duration
}

// SetFreshness records the publisher's update mode and staleness threshold.
func SetFreshness(ctx context.Context, q Querier, f Freshness) error {
	var secs *int64
	if f.StaleAfter != nil {
		s := int64(f.StaleAfter.Seconds())
		secs = &s
	}
	_, err := q.Exec(ctx, `
INSERT INTO registry.freshness (singleton, update_mode, stale_after_seconds, updated_at) VALUES (true, $1, $2, now())
ON CONFLICT (singleton) DO UPDATE SET update_mode = EXCLUDED.update_mode, stale_after_seconds = EXCLUDED.stale_after_seconds,
    updated_at = now()`, f.UpdateMode, secs)
	return err
}

// ReadFreshness reads the freshness settings; a registry without the table
// (before schema version 3) or without a row reports manual updates and no
// threshold.
func ReadFreshness(ctx context.Context, q Querier) (Freshness, error) {
	f := Freshness{UpdateMode: "manual"}
	var mode string
	var secs *int64
	err := q.QueryRow(ctx, `SELECT update_mode, stale_after_seconds FROM registry.freshness`).Scan(&mode, &secs)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows), errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "3F000"):
		return f, nil
	case err != nil:
		return f, err
	}
	f.UpdateMode = mode
	if secs != nil {
		d := time.Duration(*secs) * time.Second
		f.StaleAfter = &d
	}
	return f, nil
}

// CountSubmissions counts submissions by source and state (metrics).
func CountSubmissions(ctx context.Context, q Querier) (map[[2]string]int64, error) {
	rows, err := q.Query(ctx, `SELECT source, state, count(*) FROM registry.submissions GROUP BY source, state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]string]int64{}
	for rows.Next() {
		var src, st string
		var n int64
		if err := rows.Scan(&src, &st, &n); err != nil {
			return nil, err
		}
		out[[2]string{src, st}] = n
	}
	return out, rows.Err()
}

// CountReleases counts releases by state (metrics).
func CountReleases(ctx context.Context, q Querier) (map[string]int64, error) {
	rows, err := q.Query(ctx, `SELECT state, count(*) FROM registry.releases GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}
