package registry

import (
	"context"
	"errors"
	"time"
)

// ErrOnlinePaused refuses an automatic pointer switch after an operator has
// explicitly switched releases or paused updates.
var ErrOnlinePaused = errors.New("automatic activation is paused")

// OnlineState survives publisher restarts. It contains no upstream URL or
// credential; errors stored here are deliberately short, public-safe codes.
type OnlineState struct {
	Paused            bool       `json:"paused"`
	LastAttempt       *time.Time `json:"last_attempt"`
	LastCheck         *time.Time `json:"last_successful_check"`
	VerifiedDigest    *string    `json:"verified_digest"`
	VerifiedTimestamp *time.Time `json:"verified_timestamp"`
	LastError         *string    `json:"last_error"`
	NextAttempt       *time.Time `json:"next_attempt"`
	Failures          int        `json:"consecutive_failures"`
}

func GetOnlineState(ctx context.Context, q Querier) (OnlineState, error) {
	var s OnlineState
	err := q.QueryRow(ctx, `SELECT paused, last_attempt, last_check, verified_digest, verified_timestamp,
    last_error, next_attempt, failures FROM registry.online_state WHERE singleton`).Scan(
		&s.Paused, &s.LastAttempt, &s.LastCheck, &s.VerifiedDigest, &s.VerifiedTimestamp,
		&s.LastError, &s.NextAttempt, &s.Failures)
	return s, err
}

// RecordOnlineCheck persists a verified manifest before publication starts.
// Identical digests are idempotent across crashes; changed claims for the
// same timestamp are refused, including after a publisher restart.
func RecordOnlineCheck(ctx context.Context, q Querier, digest string, timestamp time.Time) error {
	_, err := q.Exec(ctx, `UPDATE registry.online_state SET last_check = now(),
    verified_digest = $1, verified_timestamp = $2 WHERE singleton`, digest, timestamp)
	return err
}

// RecordOnlineAttempt persists scheduling and a sanitized error code.
func RecordOnlineAttempt(ctx context.Context, q Querier, code string, next time.Time) error {
	_, err := q.Exec(ctx, `UPDATE registry.online_state SET last_attempt = now(),
    last_error = NULLIF($1, ''), next_attempt = $2,
    failures = CASE WHEN $1 = '' THEN 0 ELSE LEAST(failures + 1, 30) END
WHERE singleton`, code, next)
	return err
}

// SetOnlinePaused changes the automatic activation policy with a durable
// audit record in the same transaction. Polls may still check freshness.
func SetOnlinePaused(ctx context.Context, db TxBeginner, paused bool, actor, reason, requestID string) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, pointerLockKey); err != nil {
		return err
	}
	var old bool
	if err = tx.QueryRow(ctx, `SELECT paused FROM registry.online_state WHERE singleton FOR UPDATE`).Scan(&old); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE registry.online_state SET paused = $1 WHERE singleton`, paused); err != nil {
		return err
	}
	action := "resume_online"
	if paused {
		action = "pause_online"
	}
	outcome := OutcomeSucceeded
	if old == paused {
		outcome = OutcomeNoop
	}
	if err = Audit(ctx, tx, AuditEntry{Actor: actor, Source: "operator_api", Action: action,
		Outcome: outcome, Reason: reason, RequestID: requestID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// OnlineClaim returns a submission by its digest fingerprint, even when the
// previous process stopped halfway through a build.
func OnlineClaim(ctx context.Context, q Querier, region, digest string, maxAttempts int) (*Submission, bool, error) {
	return ClaimSubmission(ctx, q, Submission{Source: "online", Name: digest[:16],
		Fingerprint: "online:v1:" + region + ":" + digest, RegionID: region}, func(s *Submission) bool {
		return s.State == SubInterrupted && s.Attempts < maxAttempts ||
			s.State == SubReady && s.ReasonCode != nil && *s.ReasonCode == "online_paused"
	})
}
