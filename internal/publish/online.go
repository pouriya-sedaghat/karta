package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/failpoint"
	"github.com/pouriya-sedaghat/karta/internal/importer"
	"github.com/pouriya-sedaghat/karta/internal/inbox"
	"github.com/pouriya-sedaghat/karta/internal/online"
	"github.com/pouriya-sedaghat/karta/internal/region"
	"github.com/pouriya-sedaghat/karta/internal/registry"
)

// Submission sources.
const (
	SourceInbox  = "inbox"
	SourceCLI    = "cli"
	SourceOnline = "online"
)

// Online reason codes (besides the online package's verification codes).
const (
	// CodeOnlinePaused: an online snapshot was built and validated but not
	// activated because automatic activation of online snapshots is paused.
	CodeOnlinePaused = "online_activation_paused"
)

// Errors of online operator actions.
var (
	ErrOnlineDisabled = errors.New("online updates are not enabled on this publisher (KARTA_ONLINE_SOURCE_FILE)")
	ErrNothingToRetry = errors.New("no failed or rejected online delivery is waiting in the fetcher's outbox")
)

func (s *Service) onlineEnabled() bool { return s.cfg.OnlineSourcePath != "" && s.cfg.OnlineDir != "" }

// onlineReject is a refused online delivery.
func onlineReject(code, format string, args ...any) error {
	return &importer.InputError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// verifyDelivery checks a staged online delivery before anything else reads
// it: the signed manifest (exact staged bytes) verifies with a key pinned
// in the source file and is bound to this region and inside its validity
// window; the staged snapshot and sidecar are exactly the signed bytes; and
// the serial is not lower than (or a different manifest under the same
// serial as) the newest manifest verified before, which is then recorded.
// It returns the verified manifest and the digest authorizer for
// importer.Verify: the signature itself, or, with
// require_operator_authorization, the Stage 2 pins and authorizations.
func (s *Service) verifyDelivery(ctx context.Context, req request, cfg region.Config) (*online.Verified, importer.Authorizer, error) {
	src, err := online.LoadSource(s.cfg.OnlineSourcePath)
	if err != nil {
		return nil, nil, onlineReject(online.CodeConfig, "online source file: %v", err)
	}
	if src.RegionID != cfg.ID {
		return nil, nil, onlineReject(online.CodeConfig, "the online source is for region %q, the publisher serves %q", src.RegionID, cfg.ID)
	}
	if req.staged.ManifestPath == "" {
		return nil, nil, onlineReject(inbox.CodeManifestMissing, "the delivery has no signed manifest")
	}
	raw, err := os.ReadFile(req.staged.ManifestPath) // #nosec G304 -- our private staging copy
	if err != nil {
		return nil, nil, err
	}
	v, err := online.Verify(raw, online.VerifyOptions{Source: src, Region: cfg, Now: s.now(), Skew: s.cfg.MaxFutureSkew,
		MaxSnapshotBytes: s.cfg.MaxInputBytes})
	if err != nil {
		return nil, nil, onlineReject(online.CodeOf(err), "signed manifest: %v", err)
	}
	m := v.Manifest
	if req.staged.SHA256 != m.Snapshot.SHA256 || req.staged.Size != m.Snapshot.SizeBytes {
		return nil, nil, onlineReject(online.CodeDigestMismatch, "the delivered snapshot (SHA-256 %s, %d bytes) is not the signed one (%s, %d bytes)",
			req.staged.SHA256, req.staged.Size, m.Snapshot.SHA256, m.Snapshot.SizeBytes)
	}
	switch p := m.Provenance; {
	case p == nil && req.staged.SidecarPath != "":
		return nil, nil, onlineReject(online.CodeManifestConflict, "the delivery has a provenance sidecar the manifest does not sign")
	case p != nil && req.staged.SidecarPath == "":
		return nil, nil, onlineReject(online.CodeManifestConflict, "the manifest signs a provenance sidecar the delivery lacks")
	case p != nil:
		b, err := os.ReadFile(req.staged.SidecarPath) // #nosec G304 -- our private staging copy
		if err != nil {
			return nil, nil, err
		}
		if int64(len(b)) != p.SizeBytes || sha256Hex(b) != p.SHA256 {
			return nil, nil, onlineReject(online.CodeDigestMismatch, "the delivered provenance sidecar is not the signed one")
		}
	}
	subID := req.subID
	err = registry.RecordManifest(ctx, s.reg, registry.SourceState{RegionID: cfg.ID, Serial: m.Serial, EnvelopeSHA256: v.EnvelopeSHA256,
		SnapshotSHA256: m.Snapshot.SHA256, SnapshotSize: m.Snapshot.SizeBytes, DataTimestamp: m.Snapshot.DataTimestamp,
		IssuedAt: m.IssuedAt, ExpiresAt: m.ExpiresAt, KeyID: v.KeyID, SubmissionID: &subID})
	switch {
	case errors.Is(err, registry.ErrManifestReplayed):
		return nil, nil, onlineReject(online.CodeManifestReplayed, "%v; refusing an older manifest", err)
	case errors.Is(err, registry.ErrManifestConflict):
		return nil, nil, onlineReject(online.CodeManifestConflict, "%v", err)
	case err != nil:
		return nil, nil, err
	}
	s.updateSubmission(ctx, req.subID, registry.SubmissionUpdate{State: registry.SubProcessing, ManifestSerial: m.Serial, ManifestSHA256: v.EnvelopeSHA256})
	failpoint.Hit("online.after_verify")
	if src.RequireOperatorAuthorization {
		return v, s.authorizer, nil
	}
	return v, func(_ context.Context, regionID, digest string, size int64) (string, error) {
		if regionID == m.RegionID && digest == m.Snapshot.SHA256 && size == m.Snapshot.SizeBytes {
			return fmt.Sprintf("signed manifest serial %d (key %s, %s)", m.Serial, v.KeyID, v.KeyFingerprint), nil
		}
		return "", nil
	}, nil
}

// onlineGate refuses the automatic activation of an online snapshot while
// automatic activation is paused; it runs inside the pointer transaction.
func onlineGate(regionID string) func(ctx context.Context, q registry.Querier) error {
	return func(ctx context.Context, q registry.Querier) error {
		on, err := registry.OnlineAutoActivateLocked(ctx, q, regionID)
		if err != nil {
			return err
		}
		if !on {
			return policyErr(CodeOnlinePaused, "validated and ready, not activated: automatic activation of online snapshots is paused "+
				"(resume it, or activate this release, with the operator API)")
		}
		return nil
	}
}

// OnlinePolicyResult reports the policy after a pause or resume.
type OnlinePolicyResult struct {
	Changed bool                   `json:"changed"`
	Policy  *registry.OnlinePolicy `json:"policy"`
}

// OnlinePause stops online snapshots from being activated automatically
// (they are still downloaded, verified, built and kept ready).
func (s *Service) OnlinePause(ctx context.Context, p Principal, reason string) (OnlinePolicyResult, error) {
	return s.setOnline(ctx, p, false, reason)
}

// OnlineResume lets online snapshots be activated automatically again (from
// the next delivery on; a release kept ready meanwhile is activated
// explicitly).
func (s *Service) OnlineResume(ctx context.Context, p Principal, reason string) (OnlinePolicyResult, error) {
	return s.setOnline(ctx, p, true, reason)
}

func (s *Service) setOnline(ctx context.Context, p Principal, on bool, reason string) (OnlinePolicyResult, error) {
	action := "online_resume"
	if !on {
		action = "online_pause"
	}
	if !s.onlineEnabled() {
		s.auditAction(ctx, p, action, "", registry.OutcomeRejected, reason, ErrOnlineDisabled.Error())
		return OnlinePolicyResult{}, ErrOnlineDisabled
	}
	regionID := s.regionID()
	changed, err := registry.SetOnlineAutoActivate(ctx, s.reg, regionID, on, p.Name, p.Source, reason, p.RequestID, nil)
	if err != nil {
		return OnlinePolicyResult{}, err
	}
	pol, err := registry.GetOnlinePolicy(ctx, s.reg, regionID)
	if err != nil {
		return OnlinePolicyResult{}, err
	}
	s.log.Info("online activation policy", "action", action, "actor", p.Name, "changed", changed)
	return OnlinePolicyResult{Changed: changed, Policy: pol}, nil
}

// OnlineRetryResult names the delivery queued for another attempt.
type OnlineRetryResult struct {
	SubmissionID int64  `json:"submission_id"`
	Name         string `json:"name"`
	State        string `json:"previous_state"`
	ReasonCode   string `json:"previous_reason_code"`
}

// OnlineRetry queues the newest online delivery for one more attempt if it
// ended failed, rejected or interrupted and its files are still in the
// fetcher's outbox (for example after freeing disk space).
func (s *Service) OnlineRetry(ctx context.Context, p Principal, reason string) (OnlineRetryResult, error) {
	if !s.onlineEnabled() {
		s.auditAction(ctx, p, "online_retry", "", registry.OutcomeRejected, reason, ErrOnlineDisabled.Error())
		return OnlineRetryResult{}, ErrOnlineDisabled
	}
	sub, err := registry.LatestSubmission(ctx, s.reg, SourceOnline)
	if err != nil {
		return OnlineRetryResult{}, err
	}
	present := false
	if sub != nil && (sub.State == registry.SubFailed || sub.State == registry.SubRejected || sub.State == registry.SubInterrupted) {
		entries, err := inbox.ScanWith(s.cfg.OnlineDir, s.cfg.InboxMaxEntries, inbox.ScanOptions{Manifests: true})
		if err != nil {
			return OnlineRetryResult{}, err
		}
		for _, e := range entries {
			present = present || (e.Name == sub.Name && e.Fingerprint() == sub.Fingerprint)
		}
	}
	if !present {
		s.auditAction(ctx, p, "online_retry", "", registry.OutcomeRejected, reason, ErrNothingToRetry.Error())
		return OnlineRetryResult{}, ErrNothingToRetry
	}
	if err := registry.RequestOnlineRetry(ctx, s.reg, sub.RegionID, sub.Fingerprint); err != nil {
		return OnlineRetryResult{}, err
	}
	res := OnlineRetryResult{SubmissionID: sub.ID, Name: sub.Name, State: sub.State}
	if sub.ReasonCode != nil {
		res.ReasonCode = *sub.ReasonCode
	}
	if err := registry.Audit(ctx, s.reg, registry.AuditEntry{Actor: p.Name, Source: p.Source, Action: "online_retry", Target: sub.Name,
		Outcome: registry.OutcomeSucceeded, Reason: reason, RequestID: p.RequestID,
		Detail: map[string]any{"submission_id": sub.ID, "previous_state": sub.State, "previous_reason_code": res.ReasonCode}}); err != nil {
		s.log.Error("could not write audit record", "err", err)
	}
	return res, nil
}

// retryRequested reports whether an operator queued this online submission
// for another attempt after it finished.
func (s *Service) retryRequested(ctx context.Context, prior *registry.Submission) bool {
	if prior.Source != SourceOnline || prior.FinishedAt == nil {
		return false
	}
	pol, err := registry.GetOnlinePolicy(ctx, s.reg, prior.RegionID)
	return err == nil && pol.RetryFingerprint != nil && *pol.RetryFingerprint == prior.Fingerprint &&
		pol.RetryRequestedAt != nil && prior.FinishedAt.Before(*pol.RetryRequestedAt)
}

// OnlineStatus is the operator view of online updates.
type OnlineStatus struct {
	Enabled bool `json:"enabled"`
	// Source describes the configured source (nothing secret).
	Source *OnlineSourceStatus `json:"source"`
	// AutoActivate is whether a validated online snapshot is activated now:
	// KARTA_PUBLISH_AUTO_ACTIVATE and not paused.
	AutoActivate bool                   `json:"auto_activate"`
	Policy       *registry.OnlinePolicy `json:"policy"`
	// Verified is the newest signed manifest the publisher itself verified
	// (on a delivery).
	Verified *registry.SourceState `json:"verified"`
	// Fetcher is the fetcher's own report: checks, errors, next attempt,
	// download progress. It comes from the network-facing process and is
	// shown as reported.
	Fetcher  FetcherReport `json:"fetcher"`
	LastScan ScanStatus    `json:"last_scan"`
}

// OnlineSourceStatus describes the source file.
type OnlineSourceStatus struct {
	RegionID                     string           `json:"region_id"`
	Manifest                     string           `json:"manifest"`
	TrustedKeys                  []online.KeyInfo `json:"trusted_keys"`
	RequireOperatorAuthorization bool             `json:"require_operator_authorization"`
	PollIntervalSeconds          float64          `json:"poll_interval_seconds"`
	MaxManifestValiditySeconds   float64          `json:"max_manifest_validity_seconds"`
	Error                        string           `json:"error,omitempty"`
}

// FetcherReport is the fetcher's state file as read by the publisher.
type FetcherReport struct {
	State *online.State `json:"state"`
	// StateAgeSeconds is how long ago the fetcher last wrote its state; a
	// growing value means the fetcher is not running.
	StateAgeSeconds *float64 `json:"state_age_seconds"`
	Error           string   `json:"error,omitempty"`
}

// FreshnessStatus is the age of the served data against the configured
// threshold. It is computed from the active release's data timestamp,
// never from a source check.
type FreshnessStatus struct {
	UpdateMode           string     `json:"update_mode"`
	ActiveDataTimestamp  *time.Time `json:"active_data_timestamp"`
	ActiveDataAgeSeconds *float64   `json:"active_data_age_seconds"`
	StaleAfterSeconds    *float64   `json:"stale_after_seconds"`
	Stale                *bool      `json:"stale"`
}

func (s *Service) updateMode() string {
	if s.onlineEnabled() {
		return SourceOnline
	}
	return "manual"
}

func (s *Service) freshness(active *registry.Release) FreshnessStatus {
	f := FreshnessStatus{UpdateMode: s.updateMode()}
	if s.cfg.StaleAfter > 0 {
		v := s.cfg.StaleAfter.Seconds()
		f.StaleAfterSeconds = &v
	}
	if active != nil && active.DataTimestamp != nil {
		ts := active.DataTimestamp.UTC()
		age := s.now().Sub(ts).Seconds()
		f.ActiveDataTimestamp, f.ActiveDataAgeSeconds = &ts, &age
		if s.cfg.StaleAfter > 0 {
			stale := age > s.cfg.StaleAfter.Seconds()
			f.Stale = &stale
		}
	}
	return f
}

func (s *Service) onlineStatus(ctx context.Context, regionID string) (OnlineStatus, error) {
	st := OnlineStatus{Enabled: s.onlineEnabled()}
	s.mu.Lock()
	st.LastScan = s.onlineScan
	s.mu.Unlock()
	if st.LastScan.Pending == nil {
		st.LastScan.Pending = []PendingEntry{}
	}
	if !st.Enabled {
		return st, nil
	}
	src, err := online.LoadSource(s.cfg.OnlineSourcePath)
	if err != nil {
		st.Source = &OnlineSourceStatus{Error: err.Error(), TrustedKeys: []online.KeyInfo{}}
	} else {
		u := src.ManifestURLParsed()
		st.Source = &OnlineSourceStatus{RegionID: src.RegionID, Manifest: u.Scheme + "://" + u.Host + u.EscapedPath(),
			TrustedKeys: src.KeyIDs(), RequireOperatorAuthorization: src.RequireOperatorAuthorization,
			PollIntervalSeconds: src.PollInterval.D().Seconds(), MaxManifestValiditySeconds: src.MaxManifestValidity.D().Seconds()}
	}
	if st.Policy, err = registry.GetOnlinePolicy(ctx, s.reg, regionID); err != nil {
		return st, err
	}
	st.AutoActivate = s.cfg.AutoActivate && st.Policy.AutoActivate
	if st.Verified, err = registry.GetSourceState(ctx, s.reg, regionID); err != nil {
		return st, err
	}
	fs, err := online.ReadState(s.cfg.OnlineDir)
	switch {
	case err != nil:
		st.Fetcher.Error = "fetcher state unreadable: " + err.Error()
	case fs == nil:
		st.Fetcher.Error = "the fetcher has not written its state yet (is it running?)"
	default:
		st.Fetcher.State = fs
		age := s.now().Sub(fs.UpdatedAt).Seconds()
		st.Fetcher.StateAgeSeconds = &age
	}
	return st, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
