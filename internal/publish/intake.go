package publish

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/importer"
	"github.com/pouriya-sedaghat/karta/internal/intake"
	"github.com/pouriya-sedaghat/karta/internal/region"
	"github.com/pouriya-sedaghat/karta/internal/registry"
)

// SourceIntake is the local intake's handoff directory (Stage 5).
const SourceIntake = "intake"

// Authorization reason codes (manual paths; ADR 0006).
const (
	// CodeAuthorizationRevoked: the authorization that admitted the digest
	// was revoked (at verification or at the switch).
	CodeAuthorizationRevoked = "authorization_revoked"
	// CodeAuthorizationExpired: it expired before it was used, or during the
	// publication.
	CodeAuthorizationExpired = "authorization_expired"
	// CodeIntakePaused: a snapshot admitted only by the unattended watcher
	// was built and validated but not activated, because a rollback (or an
	// operator) paused automatic activation of watcher deliveries.
	CodeIntakePaused = "intake_activation_paused"
)

// Errors of intake operator actions.
var (
	ErrIntakeDisabled = errors.New("the local intake is not enabled on this publisher (KARTA_INTAKE_DIR)")
	ErrIntakeLimit    = registry.ErrIntakeLimit
	ErrIntakeNotOwned = registry.ErrIntakeNotOwned
)

// IntakeRefusal is an intake authorization request the publisher refuses
// (region, size or validity outside its bounds).
type IntakeRefusal struct {
	Code string
	Msg  string
}

func (e *IntakeRefusal) Error() string { return e.Msg }

// Intake refusal codes.
const (
	CodeIntakeRegion   = "region_mismatch"
	CodeIntakeTooLarge = "too_large"
	CodeIntakeTTL      = "validity_beyond_cap"
	// CodeIntakeRevoked is a digest an operator revoked: the intake cannot
	// authorize it again until an operator authorizes it.
	CodeIntakeRevoked = "digest_revoked"
)

func (s *Service) intakeEnabled() bool { return s.cfg.IntakeDir != "" }

func (s *Service) intakeFeed() feed {
	return feed{source: SourceIntake, dir: s.cfg.IntakeDir, settler: s.intakeSettler, scan: &s.intakeScan}
}

// IntakeLimits are the bounds of intake authorizations, reported to the
// intake so it never asks for more.
type IntakeLimits struct {
	RegionID         string  `json:"region_id"`
	MaxTTLSeconds    float64 `json:"max_ttl_seconds"`
	MaxOpen          int     `json:"max_open"`
	MaxSnapshotBytes int64   `json:"max_snapshot_bytes"`
}

func (s *Service) intakeLimits() IntakeLimits {
	return IntakeLimits{RegionID: s.regionID(), MaxTTLSeconds: s.cfg.IntakeMaxAge.Seconds(), MaxOpen: s.cfg.IntakeMaxOpen,
		MaxSnapshotBytes: s.cfg.MaxInputBytes}
}

// DefaultIntakeMaxAge is the validity an intake authorization may have: it
// must outlive a wait behind the running build, its own publication and
// the interrupted retries of every queued intake delivery (ADR 0006).
func DefaultIntakeMaxAge(maxOpen, maxAttempts int, publishTimeout time.Duration) time.Duration {
	return time.Duration(1+maxOpen*maxAttempts)*publishTimeout + time.Hour
}

// IntakeAuthorizeRequest is an intake credential's request.
type IntakeAuthorizeRequest struct {
	SHA256    string
	SizeBytes int64
	RegionID  string
	TTL       time.Duration
	Name      string
	Reason    string
}

// IntakeAuthorize records an authorization for the caller, an intake
// credential whose scope gave channel. The publisher fixes the bounds: its
// own region, at most the configured validity and size, and at most the
// configured number of open authorizations per credential.
func (s *Service) IntakeAuthorize(ctx context.Context, p Principal, channel string, r IntakeAuthorizeRequest) (*registry.Authorization, bool, error) {
	refuse := func(err error) (*registry.Authorization, bool, error) {
		s.auditAction(ctx, p, "intake_authorize", r.SHA256, registry.OutcomeRejected, r.Reason, err.Error())
		return nil, false, err
	}
	if !s.intakeEnabled() {
		return refuse(ErrIntakeDisabled)
	}
	cfg, err := region.Load(s.cfg.RegionPath)
	if err != nil {
		return nil, false, err
	}
	switch {
	case r.RegionID != cfg.ID:
		return refuse(&IntakeRefusal{Code: CodeIntakeRegion, Msg: fmt.Sprintf("the request is for region %q, this publisher serves %q", r.RegionID, cfg.ID)})
	case r.SizeBytes > s.cfg.MaxInputBytes:
		return refuse(&IntakeRefusal{Code: CodeIntakeTooLarge, Msg: fmt.Sprintf("%d bytes is above the limit of %d (KARTA_MAX_INPUT_MB)", r.SizeBytes, s.cfg.MaxInputBytes)})
	case r.TTL > s.cfg.IntakeMaxAge:
		return refuse(&IntakeRefusal{Code: CodeIntakeTTL, Msg: fmt.Sprintf("a validity of %s is beyond the cap of %s (KARTA_INTAKE_AUTHORIZATION_MAX_AGE)",
			r.TTL, s.cfg.IntakeMaxAge)})
	}
	a, created, err := registry.AuthorizeIntake(ctx, s.reg, registry.IntakeRequest{RegionID: cfg.ID, SHA256: r.SHA256, SizeBytes: r.SizeBytes,
		ExpiresAt: s.now().Add(r.TTL), Channel: channel, Name: r.Name, CreatedBy: p.Name, Reason: r.Reason, RequestID: p.RequestID,
		MaxOpen: s.cfg.IntakeMaxOpen})
	if errors.Is(err, registry.ErrIntakeBlocked) {
		// Audited by the registry; reported as a bounded refusal.
		return nil, false, &IntakeRefusal{Code: CodeIntakeRevoked, Msg: err.Error()}
	}
	return a, created, err
}

// IntakeClose closes one of the caller's own intake authorizations.
func (s *Service) IntakeClose(ctx context.Context, p Principal, id int64, reason string) (bool, error) {
	return registry.CloseIntakeAuthorization(ctx, s.reg, id, p.Name, reason, p.RequestID)
}

// IntakeList is the caller's narrow view: its own authorizations, the
// submission of each handoff name, and the limits.
type IntakeList struct {
	Limits  IntakeLimits            `json:"limits"`
	Records []registry.IntakeRecord `json:"records"`
}

// IntakeList returns the caller's own intake records.
func (s *Service) IntakeList(ctx context.Context, p Principal) (IntakeList, error) {
	if !s.intakeEnabled() {
		return IntakeList{}, ErrIntakeDisabled
	}
	recs, err := registry.ListIntakeAuthorizations(ctx, s.reg, s.regionID(), p.Name, 50)
	if err != nil {
		return IntakeList{}, err
	}
	if recs == nil {
		recs = []registry.IntakeRecord{}
	}
	return IntakeList{Limits: s.intakeLimits(), Records: recs}, nil
}

// IntakePolicyResult reports the watcher policy after a pause or resume.
type IntakePolicyResult struct {
	Changed bool                   `json:"changed"`
	Policy  *registry.IntakePolicy `json:"policy"`
}

// IntakePause stops snapshots admitted only by the unattended watcher from
// being activated automatically (they are still built and kept ready).
func (s *Service) IntakePause(ctx context.Context, p Principal, reason string) (IntakePolicyResult, error) {
	return s.setIntake(ctx, p, false, reason)
}

// IntakeResume lets watcher deliveries be activated automatically again.
func (s *Service) IntakeResume(ctx context.Context, p Principal, reason string) (IntakePolicyResult, error) {
	return s.setIntake(ctx, p, true, reason)
}

func (s *Service) setIntake(ctx context.Context, p Principal, on bool, reason string) (IntakePolicyResult, error) {
	action := "intake_resume"
	if !on {
		action = "intake_pause"
	}
	if !s.intakeEnabled() {
		s.auditAction(ctx, p, action, "", registry.OutcomeRejected, reason, ErrIntakeDisabled.Error())
		return IntakePolicyResult{}, ErrIntakeDisabled
	}
	regionID := s.regionID()
	changed, err := registry.SetIntakeWatcherAutoActivate(ctx, s.reg, regionID, on, p.Name, p.Source, reason, p.RequestID, nil)
	if err != nil {
		return IntakePolicyResult{}, err
	}
	pol, err := registry.GetIntakePolicy(ctx, s.reg, regionID)
	if err != nil {
		return IntakePolicyResult{}, err
	}
	s.log.Info("intake watcher activation policy", "action", action, "actor", p.Name, "changed", changed)
	return IntakePolicyResult{Changed: changed, Policy: pol}, nil
}

// unauthorized explains why a digest is neither pinned nor authorized now:
// the newest authorization it had was revoked or expired, or it never had
// one (or one for another size).
func (s *Service) unauthorized(ctx context.Context, q registry.Querier, regionID, digest string, size int64, scope registry.AuthScope) (string, string, error) {
	a, err := registry.LatestAuthorization(ctx, q, regionID, digest, scope)
	if err != nil {
		return "", "", err
	}
	why := ""
	if a != nil && a.RevokeReason != nil {
		why = *a.RevokeReason
	}
	switch {
	case a == nil:
	case a.RevokedAt != nil && why == "expired",
		a.RevokedAt == nil && a.ExpiresAt != nil && !a.ExpiresAt.After(s.now()):
		return CodeAuthorizationExpired, fmt.Sprintf("the %s of SHA-256 %s expired at %s; authorize it again (a fresh delivery through the intake, "+
			"or an operator) to publish it", a.Describe(), digest, a.ExpiresAt.UTC().Format(time.RFC3339)), nil
	case a.RevokedAt != nil && why != "superseded":
		by := ""
		if a.RevokedBy != nil {
			by = " by " + *a.RevokedBy
		}
		return CodeAuthorizationRevoked, fmt.Sprintf("the %s of SHA-256 %s was revoked%s at %s (%s)", a.Describe(), digest, by,
			a.RevokedAt.UTC().Format(time.RFC3339), why), nil
	}
	return importer.CodeUnauthorizedDigest, fmt.Sprintf("SHA-256 %s (%d bytes) is not pinned in region %q and not authorized", digest, size, regionID), nil
}

// manualGate runs inside the pointer transaction of an automatic manual,
// command-line or intake activation and fails closed: the digest must
// still be pinned in the region file as it is now, or covered by an open,
// unexpired authorization. A snapshot covered only by a watcher
// authorization also needs automatic watcher activation not to be paused.
// A refusal leaves the active release as it is and the candidate ready.
func (s *Service) manualGate(digest string, size int64, scope registry.AuthScope) func(context.Context, registry.Querier) error {
	scope.Lock = true
	return func(ctx context.Context, q registry.Querier) error {
		cfg, err := region.Load(s.cfg.RegionPath)
		if err != nil {
			return &importer.InputError{Code: importer.CodeRegionConfig, Msg: "at activation, the region file cannot be read: " + err.Error()}
		}
		if cfg.Source.Pinned(digest) {
			return nil
		}
		a, err := registry.Authorized(ctx, q, cfg.ID, digest, size, scope)
		if err != nil {
			return err
		}
		if a == nil {
			code, msg, err := s.unauthorized(ctx, q, cfg.ID, digest, size, scope)
			if err != nil {
				return err
			}
			return &importer.InputError{Code: code, Msg: "at activation, " + msg}
		}
		if a.Channel == registry.ChannelIntakeWatch {
			on, err := registry.IntakeWatcherAutoActivateLocked(ctx, q, cfg.ID)
			if err != nil {
				return err
			}
			if !on {
				return policyErr(CodeIntakePaused, "validated and ready, not activated: automatic activation of deliveries admitted only by the "+
					"unattended intake watcher is paused (a rollback pauses it); resume it with the operator API, deliver with the "+
					"authenticated command, or activate this release explicitly")
			}
		}
		return nil
	}
}

// IntakeStatus is the operator view of the local intake.
type IntakeStatus struct {
	Enabled bool `json:"enabled"`
	// AutoActivate is whether a watcher delivery is activated now:
	// KARTA_PUBLISH_AUTO_ACTIVATE and the watcher not paused.
	AutoActivate bool                   `json:"auto_activate"`
	Policy       *registry.IntakePolicy `json:"policy"`
	Limits       *IntakeLimits          `json:"limits"`
	// OpenAuthorizations counts open, unexpired intake authorizations by
	// channel.
	OpenAuthorizations map[string]int64 `json:"open_authorizations"`
	// Watcher is the watcher's own report, as it wrote it.
	Watcher  WatcherReport `json:"watcher"`
	LastScan ScanStatus    `json:"last_scan"`
}

// WatcherReport is the watcher's state file as read by the publisher.
type WatcherReport struct {
	State           *intake.State `json:"state"`
	StateAgeSeconds *float64      `json:"state_age_seconds"`
	Error           string        `json:"error,omitempty"`
}

func (s *Service) intakeStatus(ctx context.Context, regionID string) (IntakeStatus, error) {
	st := IntakeStatus{Enabled: s.intakeEnabled(), OpenAuthorizations: map[string]int64{}}
	s.mu.Lock()
	st.LastScan = s.intakeScan
	s.mu.Unlock()
	if st.LastScan.Pending == nil {
		st.LastScan.Pending = []PendingEntry{}
	}
	if !st.Enabled {
		return st, nil
	}
	lim := s.intakeLimits()
	st.Limits = &lim
	var err error
	if st.Policy, err = registry.GetIntakePolicy(ctx, s.reg, regionID); err != nil {
		return st, err
	}
	st.AutoActivate = s.cfg.AutoActivate && st.Policy.WatcherAutoActivate
	if st.OpenAuthorizations, err = registry.CountOpenIntakeAuthorizations(ctx, s.reg, regionID); err != nil {
		return st, err
	}
	ws, err := intake.ReadState(s.cfg.IntakeDir)
	switch {
	case err != nil:
		st.Watcher.Error = "intake watcher state unreadable: " + err.Error()
	case ws == nil:
		st.Watcher.Error = "no intake watcher has written its state (the watcher is optional; the command works without it)"
	default:
		st.Watcher.State = ws
		age := s.now().Sub(ws.UpdatedAt).Seconds()
		st.Watcher.StateAgeSeconds = &age
	}
	return st, nil
}

// resubmitHintIntake is the recovery of an intake submission.
const resubmitHintIntake = "deliver it again through the intake (a new completion marker, or run the command again)"
