package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Errors of intake authorizations.
var (
	// ErrIntakeLimit is a request beyond the credential's open-authorization
	// limit.
	ErrIntakeLimit = errors.New("the credential already holds its maximum of open intake authorizations")
	// ErrIntakeNotOwned is a close of an authorization the credential did not
	// create (reported as not found, so ids of others are not revealed).
	ErrIntakeNotOwned = errors.New("no such intake authorization of this credential")
	// ErrIntakeBlocked is a digest an operator revoked: only an operator can
	// authorize it again.
	ErrIntakeBlocked = errors.New("an operator revoked this digest; only an operator can authorize it again")
)

// intakeLockKey serialises the open-authorization count of one intake
// credential (transaction scope, with the credential's hash as second key).
const intakeLockKey int32 = 0x6b617269 // "kari"

// IntakeRequest is a local intake credential's request for an
// authorization. The channel comes from the credential's scope, never from
// the request.
type IntakeRequest struct {
	RegionID  string
	SHA256    string
	SizeBytes int64
	ExpiresAt time.Time
	Channel   string
	// Name is the handoff name the authorization is for.
	Name      string
	CreatedBy string
	Reason    string
	RequestID string
	// MaxOpen bounds the credential's open, unexpired authorizations.
	MaxOpen int
}

// AuthorizeIntake records an intake authorization. The same credential
// asking again for the same digest, size and name gets its existing open
// row back (created = false): a restarted intake is idempotent. Otherwise
// the credential's own open row for the digest, if any, is closed first;
// rows of operators and of other intake credentials are never touched.
// Expired rows of the credential are closed, and the request is refused
// with ErrIntakeLimit when the credential already holds MaxOpen open ones.
func AuthorizeIntake(ctx context.Context, db TxBeginner, r IntakeRequest) (*Authorization, bool, error) {
	if r.Channel != ChannelIntakeWatch && r.Channel != ChannelIntakeSubmit {
		return nil, false, fmt.Errorf("channel %q is not an intake channel", r.Channel)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`, intakeLockKey, r.CreatedBy); err != nil {
		return nil, false, err
	}
	audit := func(outcome, why string, detail map[string]any) error {
		if detail == nil {
			detail = map[string]any{}
		}
		detail["region_id"], detail["channel"], detail["name"], detail["size_bytes"] = r.RegionID, r.Channel, r.Name, r.SizeBytes
		detail["expires_at"] = r.ExpiresAt.UTC().Format(time.RFC3339)
		if why != "" {
			detail["refused"] = why
		}
		return Audit(ctx, tx, AuditEntry{Actor: r.CreatedBy, Source: "operator_api", Action: "intake_authorize", Target: r.SHA256,
			Outcome: outcome, Reason: r.Reason, RequestID: r.RequestID, Detail: detail})
	}
	var blockedBy string
	switch err := tx.QueryRow(ctx, `SELECT blocked_by FROM registry.intake_blocks WHERE region_id = $1 AND sha256 = $2`,
		r.RegionID, r.SHA256).Scan(&blockedBy); {
	case err == nil:
		why := "revoked by " + blockedBy
		if err := audit(OutcomeRejected, why, nil); err != nil {
			return nil, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, err
		}
		return nil, false, fmt.Errorf("%w (%s)", ErrIntakeBlocked, why)
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, false, err
	}
	// Expired rows of this credential are closed; they authorize nothing.
	if _, err := tx.Exec(ctx, `UPDATE registry.authorizations SET revoked_at = now(), revoked_by = $1, revoke_reason = 'expired'
WHERE created_by = $1 AND channel <> 'operator' AND revoked_at IS NULL AND expires_at <= now()`, r.CreatedBy); err != nil {
		return nil, false, err
	}
	existing, err := scanAuth(tx.QueryRow(ctx, `SELECT `+authColumns+` FROM registry.authorizations
WHERE region_id = $1 AND sha256 = $2 AND created_by = $3 AND channel <> 'operator' AND revoked_at IS NULL FOR UPDATE`,
		r.RegionID, r.SHA256, r.CreatedBy))
	switch {
	case err == nil && existing.Channel == r.Channel && existing.SizeBytes != nil && *existing.SizeBytes == r.SizeBytes &&
		existing.IntakeName != nil && *existing.IntakeName == r.Name:
		if err := audit(OutcomeNoop, "", map[string]any{"authorization_id": existing.ID}); err != nil {
			return nil, false, err
		}
		return existing, false, tx.Commit(ctx)
	case err == nil:
		if _, err := tx.Exec(ctx, `UPDATE registry.authorizations SET revoked_at = now(), revoked_by = $2, revoke_reason = 'superseded'
WHERE id = $1`, existing.ID, r.CreatedBy); err != nil {
			return nil, false, err
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, false, err
	}
	var open int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM registry.authorizations
WHERE created_by = $1 AND channel <> 'operator' AND revoked_at IS NULL`, r.CreatedBy).Scan(&open); err != nil {
		return nil, false, err
	}
	if open >= r.MaxOpen {
		why := fmt.Sprintf("%d open intake authorizations, the limit is %d", open, r.MaxOpen)
		// The refusal is recorded even though the transaction's other
		// changes (closing expired rows) are kept as well.
		if err := audit(OutcomeRejected, why, nil); err != nil {
			return nil, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, err
		}
		return nil, false, fmt.Errorf("%w (%s)", ErrIntakeLimit, why)
	}
	created, err := scanAuth(tx.QueryRow(ctx, `
INSERT INTO registry.authorizations (region_id, sha256, size_bytes, reason, created_by, expires_at, channel, intake_name)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+authColumns,
		r.RegionID, r.SHA256, r.SizeBytes, truncate(r.Reason, 2000), r.CreatedBy, r.ExpiresAt, r.Channel, r.Name))
	if err != nil {
		return nil, false, err
	}
	if err := audit(OutcomeSucceeded, "", map[string]any{"authorization_id": created.ID}); err != nil {
		return nil, false, err
	}
	return created, true, tx.Commit(ctx)
}

// CloseIntakeAuthorization closes one of the credential's own intake
// authorizations. Closing one that is already closed is a no-op (closed =
// false); an id the credential did not create is ErrIntakeNotOwned.
func CloseIntakeAuthorization(ctx context.Context, db TxBeginner, id int64, createdBy, reason, requestID string) (bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	a, err := scanAuth(tx.QueryRow(ctx, `SELECT `+authColumns+` FROM registry.authorizations WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (a.CreatedBy != createdBy || a.Channel == ChannelOperator)) {
		if aerr := Audit(ctx, tx, AuditEntry{Actor: createdBy, Source: "operator_api", Action: "intake_close", Target: fmt.Sprint(id),
			Outcome: OutcomeRejected, Reason: reason, RequestID: requestID, Detail: map[string]any{"refused": "not an authorization of this credential"}}); aerr != nil {
			return false, aerr
		}
		if cerr := tx.Commit(ctx); cerr != nil {
			return false, cerr
		}
		return false, ErrIntakeNotOwned
	}
	if err != nil {
		return false, err
	}
	outcome, closed := OutcomeNoop, false
	if a.RevokedAt == nil {
		if _, err := tx.Exec(ctx, `UPDATE registry.authorizations SET revoked_at = now(), revoked_by = $2, revoke_reason = $3 WHERE id = $1`,
			id, createdBy, truncate(reason, 2000)); err != nil {
			return false, err
		}
		outcome, closed = OutcomeSucceeded, true
	}
	if err := Audit(ctx, tx, AuditEntry{Actor: createdBy, Source: "operator_api", Action: "intake_close", Target: a.SHA256,
		Outcome: outcome, Reason: reason, RequestID: requestID, Detail: map[string]any{"authorization_id": id, "channel": a.Channel,
			"name": a.IntakeName}}); err != nil {
		return false, err
	}
	return closed, tx.Commit(ctx)
}

// IntakeRecord is an intake authorization with the newest submission the
// publisher recorded under its handoff name.
type IntakeRecord struct {
	Authorization Authorization `json:"authorization"`
	Submission    *Submission   `json:"submission"`
}

// ListIntakeAuthorizations returns a credential's own intake authorizations:
// the open ones and those closed in the last week, newest first, each with
// the latest intake submission of its handoff name (nil if the publisher has
// not seen it yet).
func ListIntakeAuthorizations(ctx context.Context, q Querier, region, createdBy string, limit int) ([]IntakeRecord, error) {
	rows, err := q.Query(ctx, `SELECT `+authColumns+` FROM registry.authorizations
WHERE region_id = $1 AND created_by = $2 AND channel <> 'operator'
  AND (revoked_at IS NULL OR revoked_at > now() - interval '7 days')
ORDER BY (revoked_at IS NULL) DESC, id DESC LIMIT $3`, region, createdBy, limit)
	if err != nil {
		return nil, err
	}
	var out []IntakeRecord
	for rows.Next() {
		a, err := scanAuth(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, IntakeRecord{Authorization: *a})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		name := out[i].Authorization.IntakeName
		if name == nil {
			continue
		}
		s, err := scanSubmission(q.QueryRow(ctx, `SELECT `+submissionColumns+` FROM registry.submissions
WHERE source = 'intake' AND name = $1 ORDER BY id DESC LIMIT 1`, *name))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return nil, err
		default:
			out[i].Submission = s
		}
	}
	return out, nil
}

// CountOpenIntakeAuthorizations counts open, unexpired intake
// authorizations by channel (metrics).
func CountOpenIntakeAuthorizations(ctx context.Context, q Querier, region string) (map[string]int64, error) {
	rows, err := q.Query(ctx, `SELECT channel, count(*) FROM registry.authorizations
WHERE region_id = $1 AND channel <> 'operator' AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())
GROUP BY channel`, region)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{ChannelIntakeWatch: 0, ChannelIntakeSubmit: 0}
	for rows.Next() {
		var ch string
		var n int64
		if err := rows.Scan(&ch, &n); err != nil {
			return nil, err
		}
		out[ch] = n
	}
	return out, rows.Err()
}

// IntakePolicy is the operator policy for the unattended intake watcher.
type IntakePolicy struct {
	RegionID string `json:"region_id"`
	// WatcherAutoActivate is whether a snapshot admitted only by an
	// intake_watch authorization is activated automatically.
	WatcherAutoActivate bool       `json:"watcher_auto_activate"`
	ChangedAt           *time.Time `json:"changed_at"`
	ChangedBy           *string    `json:"changed_by"`
	ChangeReason        *string    `json:"change_reason"`
}

// GetIntakePolicy returns the region's intake policy (watcher activation on
// if no row exists).
func GetIntakePolicy(ctx context.Context, q Querier, regionID string) (*IntakePolicy, error) {
	var p IntakePolicy
	err := q.QueryRow(ctx, `SELECT region_id, watcher_auto_activate, changed_at, changed_by, change_reason
FROM registry.intake_policy WHERE region_id = $1`, regionID).Scan(&p.RegionID, &p.WatcherAutoActivate, &p.ChangedAt, &p.ChangedBy, &p.ChangeReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return &IntakePolicy{RegionID: regionID, WatcherAutoActivate: true}, nil
	}
	return &p, err
}

// EnsureIntakePolicy creates the region's intake policy row (watcher
// activation on) if it is missing, so that an activation's share lock and a
// pause's update always meet on the same row.
func EnsureIntakePolicy(ctx context.Context, q Querier, regionID string) error {
	_, err := q.Exec(ctx, `INSERT INTO registry.intake_policy (region_id) VALUES ($1) ON CONFLICT (region_id) DO NOTHING`, regionID)
	return err
}

// IntakeWatcherAutoActivateLocked reads, with a share lock on the policy
// row, whether watcher-admitted snapshots are activated automatically:
// inside an activation transaction a pause that commits first is seen, one
// that waits commits after the switch.
func IntakeWatcherAutoActivateLocked(ctx context.Context, q Querier, regionID string) (bool, error) {
	var on bool
	err := q.QueryRow(ctx, `SELECT watcher_auto_activate FROM registry.intake_policy WHERE region_id = $1 FOR SHARE`, regionID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	return on, err
}

// SetIntakeWatcherAutoActivate pauses (on = false) or resumes automatic
// activation of watcher-admitted snapshots and audits the change; a
// request that changes nothing is audited as a noop.
func SetIntakeWatcherAutoActivate(ctx context.Context, q Querier, regionID string, on bool, actor, source, reason, requestID string, detail map[string]any) (bool, error) {
	if err := EnsureIntakePolicy(ctx, q, regionID); err != nil {
		return false, err
	}
	tag, err := q.Exec(ctx, `
UPDATE registry.intake_policy SET watcher_auto_activate = $2, changed_at = now(), changed_by = $3, change_reason = $4
WHERE region_id = $1 AND watcher_auto_activate IS DISTINCT FROM $2`, regionID, on, truncate(actor, 64), truncate(reason, 2000))
	if err != nil {
		return false, err
	}
	action := "intake_resume"
	if !on {
		action = "intake_pause"
	}
	outcome := OutcomeSucceeded
	if tag.RowsAffected() == 0 {
		outcome = OutcomeNoop
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["region_id"] = regionID
	detail["watcher_auto_activate"] = on
	if err := Audit(ctx, q, AuditEntry{Actor: actor, Source: source, Action: action, Target: regionID, Outcome: outcome,
		Reason: reason, RequestID: requestID, Detail: detail}); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
