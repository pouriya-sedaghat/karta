package publish

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/failpoint"
	"github.com/pouriya-sedaghat/karta/internal/online"
	"github.com/pouriya-sedaghat/karta/internal/registry"
)

// RunOnline polls only in the publisher. The public API never makes network
// requests; failures schedule a later attempt and leave the active pointer.
func (s *Service) RunOnline(ctx context.Context) {
	if s.cfg.Online.ManifestURL == "" {
		return
	}
	for ctx.Err() == nil {
		state, err := registry.GetOnlineState(ctx, s.reg)
		if err != nil {
			s.log.Warn("online state unavailable", "err", err)
			waitOnline(ctx, time.Minute)
			continue
		}
		if state.NextAttempt != nil && s.now().Before(*state.NextAttempt) {
			waitOnline(ctx, min(time.Until(*state.NextAttempt), time.Minute))
			continue
		}
		code := s.OnlineOnce(ctx)
		delay := s.cfg.OnlineInterval
		if code != "" {
			failures := min(state.Failures+1, 12)
			delay = min(time.Minute*time.Duration(1<<failures), 6*time.Hour)
		}
		// Jitter spreads retries across deployments without unbounded delay.
		jitter := time.Duration(0)
		if n, err := rand.Int(rand.Reader, big.NewInt(int64(delay/5+1))); err == nil {
			jitter = time.Duration(n.Int64())
		} else {
			s.log.Warn("online retry jitter unavailable", "err", err)
		}
		next := s.now().Add(delay - delay/10 + jitter)
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if err := registry.RecordOnlineAttempt(persistCtx, s.reg, code, next); err != nil {
			s.log.Warn("could not persist online schedule", "err", err)
			cancel()
			waitOnline(ctx, time.Minute)
			continue
		}
		cancel()
	}
}

func waitOnline(ctx context.Context, delay time.Duration) {
	t := time.NewTimer(max(delay, time.Second))
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// OnlineOnce performs one signed check and, for a new snapshot, publishes it
// through exactly the same validation/build/switch path as manual imports.
// The return value is a bounded operator-visible error code (never a URL).
func (s *Service) OnlineOnce(ctx context.Context) (code string) {
	c := s.cfg.Online
	if c.ManifestURL == "" {
		return "online_disabled"
	}
	defer func() {
		outcome := registry.OutcomeSucceeded
		if code != "" {
			outcome = registry.OutcomeRejected
		}
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := registry.Audit(auditCtx, s.reg, registry.AuditEntry{Actor: "signed-source", Source: "online",
			Action: "check", Target: s.regionID(), Outcome: outcome, Reason: code}); err != nil {
			s.log.Warn("could not audit online check", "err", err)
		}
	}()
	state, err := registry.GetOnlineState(ctx, s.reg)
	if err != nil {
		return "registry_unavailable"
	}
	regionID := s.regionID()
	if regionID == "unknown" {
		return "region_config"
	}
	c.RegionID = regionID
	netCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	m, err := online.FetchManifest(netCtx, c, s.now())
	if err != nil {
		s.log.Warn("online manifest check failed", "code", "manifest_invalid")
		return "manifest_invalid"
	}
	if state.VerifiedTimestamp != nil {
		if m.DataTimestamp.Before(*state.VerifiedTimestamp) ||
			m.DataTimestamp.Equal(*state.VerifiedTimestamp) && state.VerifiedDigest != nil && m.SHA256 != *state.VerifiedDigest {
			return "source_conflict"
		}
		if state.VerifiedDigest != nil && m.SHA256 == *state.VerifiedDigest && !m.DataTimestamp.Equal(*state.VerifiedTimestamp) {
			return "source_conflict"
		}
	}
	if state.VerifiedDigest != nil && m.SHA256 == *state.VerifiedDigest {
		prior, err := registry.SubmissionByFingerprint(ctx, s.reg, "online:v1:"+regionID+":"+m.SHA256)
		if err != nil {
			return "registry_unavailable"
		}
		resume := prior != nil && prior.State == registry.SubReady && prior.ReasonCode != nil && *prior.ReasonCode == "online_paused" && !state.Paused
		if !resume {
			if err := registry.RecordOnlineCheck(ctx, s.reg, m.SHA256, m.DataTimestamp); err != nil {
				return "registry_unavailable"
			}
			return ""
		}
	}
	sub, claimed, err := registry.OnlineClaim(ctx, s.reg, regionID, m.SHA256, s.cfg.MaxAttempts)
	if err != nil {
		return "registry_unavailable"
	}
	if !claimed {
		if sub.State == registry.SubPublished || sub.State == registry.SubReady || sub.State == registry.SubDuplicate {
			if err := registry.RecordOnlineCheck(ctx, s.reg, m.SHA256, m.DataTimestamp); err != nil {
				return "registry_unavailable"
			}
			return ""
		}
		return "submission_not_retryable"
	}
	req := request{source: "online", name: sub.Name, principal: Principal{Name: "signed-source", Source: "online"},
		subID: sub.ID, activate: s.cfg.AutoActivate, reason: "verified online snapshot", onlineManifest: &m}
	s.setJob(&JobStatus{SubmissionID: sub.ID, Name: sub.Name, Source: "online", Phase: "downloading", StartedAt: s.now(), PhaseAt: s.now()})
	defer s.clearJob("online")
	staged, err := online.Download(netCtx, c, m, s.cfg.StagingDir)
	if err != nil {
		s.finish(ctx, req, Outcome{SubmissionID: sub.ID, State: registry.SubInterrupted, Code: "download_failed",
			Reason: "bounded online download failed", Err: err}, "")
		return "download_failed"
	}
	defer os.RemoveAll(staged.Dir)
	req.staged = staged
	failpoint.Hit("online.after_download")
	if !s.now().Before(m.ExpiresAt) {
		s.finish(ctx, req, Outcome{SubmissionID: sub.ID, State: registry.SubRejected, Code: "manifest_expired",
			Reason: "signed manifest expired before publication"}, m.SHA256)
		return "manifest_expired"
	}
	out := s.publish(ctx, req)
	s.finish(ctx, req, out, m.SHA256)
	if out.State == registry.SubPublished {
		s.cleanupAfterPublish(ctx)
	}
	if out.State == registry.SubPublished || out.State == registry.SubReady || out.State == registry.SubDuplicate {
		if err := registry.RecordOnlineCheck(ctx, s.reg, m.SHA256, m.DataTimestamp); err != nil {
			return "registry_unavailable"
		}
		return ""
	}
	if out.State == registry.SubInterrupted {
		return "publication_interrupted"
	}
	if out.Code != "" {
		return out.Code
	}
	return "publication_failed"
}

// OnlinePolicy is an authenticated, audited pause/resume action.
func (s *Service) OnlinePolicy(ctx context.Context, p Principal, paused bool, reason string) error {
	if s.cfg.Online.ManifestURL == "" {
		err := policyErr("online_disabled", "online updates are not configured")
		s.auditAction(ctx, p, "online_policy", "", registry.OutcomeRejected, reason, err.Error())
		return err
	}
	if err := registry.SetOnlinePaused(ctx, s.reg, paused, p.Name, reason, p.RequestID); err != nil {
		return fmt.Errorf("change online policy: %w", err)
	}
	return nil
}
