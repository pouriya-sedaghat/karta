package publish

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/dbconn"
	"github.com/pouriya-sedaghat/karta/internal/failpoint"
	"github.com/pouriya-sedaghat/karta/internal/importer"
	"github.com/pouriya-sedaghat/karta/internal/region"
	"github.com/pouriya-sedaghat/karta/internal/registry"
	"github.com/pouriya-sedaghat/karta/internal/release"
	"github.com/pouriya-sedaghat/karta/internal/releaseid"
	"github.com/pouriya-sedaghat/karta/internal/schema"
	"github.com/pouriya-sedaghat/karta/internal/style"
)

// CodeValidationRequired: the release has not passed the content policy in
// force (the region file's min_counts, searches and tiles). Thresholds are
// not part of a release id, so a release built under another policy keeps
// its id; it must be revalidated (without a rebuild) before it is activated.
const CodeValidationRequired = "validation_required"

// CodeRevalidationInProgress: a resubmission found an evaluation of its
// release already running (registry.ErrRevalidating); it is interrupted,
// nothing changed, and may be submitted again once that one finished.
const CodeRevalidationInProgress = "revalidation_in_progress"

// policyGate is the content-policy rule of a forward activation, applied
// inside the pointer transaction (registry.Activate's Allow) to the locked
// target row: the release must have passed the policy of cfg, the region file
// read for this switch (cfgErr if it could not be read). The policy binds the
// region identity (importer.PolicyOf), so a pass recorded for another name,
// box or view does not count either. Nothing else counts: not a pass under an
// earlier policy, and not a revalidation still running.
func policyGate(cfg region.Config, cfgErr error, target *registry.Release) error {
	if cfgErr != nil {
		return policyErr(importer.CodeRegionConfig, "cannot check release %s against the validation policy: region configuration: %v", target.ID, cfgErr)
	}
	if cfg.ID != target.RegionID {
		return policyErr(importer.CodeRegionConfig, "cannot check release %s against the validation policy: this publisher is configured for region %q, "+
			"the release serves %q", target.ID, cfg.ID, target.RegionID)
	}
	want := importer.PolicyOf(cfg).SHA256
	switch {
	case target.ValidationPolicySHA256 == nil:
		return policyErr(CodeValidationRequired, "release %s has not passed the validation policy in force (%s): it was built before validation "+
			"policies were recorded, or its last evaluation failed; revalidate it (operator revalidate, no rebuild) before activating it", target.ID, short(want))
	case *target.ValidationPolicySHA256 != want:
		return policyErr(CodeValidationRequired, "release %s passed validation policy %s, but the region file now requires %s (its checks, or the "+
			"region name, box or view they are evaluated for, changed); revalidate it (operator revalidate, no rebuild) before activating it. "+
			"A release built for another name, box or view cannot be revalidated against this file: publish the snapshot again",
			target.ID, short(*target.ValidationPolicySHA256), short(want))
	}
	return nil
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

// Revalidation is the result of evaluating an existing release against the
// content policy in force.
type Revalidation struct {
	ReleaseID    string                        `json:"release_id"`
	Passed       bool                          `json:"passed"`
	PolicySHA256 string                        `json:"policy_sha256"`
	Checks       []importer.Check              `json:"checks"`
	Searches     []importer.SearchCheckOutcome `json:"searches"`
	// CountsFrom says where the row counts min_counts compares come from:
	// the registry or the release's import report (both recorded when the
	// immutable release was built).
	CountsFrom string `json:"counts_from"`
	// DurationSeconds is how long the evaluation took (the measurement the
	// operator timeouts are set from).
	DurationSeconds float64 `json:"duration_seconds"`
}

// ErrRevalidation is a release that cannot be evaluated by this build.
var ErrRevalidation = errors.New("release cannot be revalidated")

// Revalidate evaluates an existing release (ready, retired or active)
// against the content policy of the region file, without a rebuild, and
// records the outcome with the release and in the audit log. It never
// activates anything: a release that passes may then be activated by an
// operator, and one that fails stays as it is and cannot be activated under
// this policy.
func (s *Service) Revalidate(ctx context.Context, p Principal, releaseID, reason string) (Revalidation, error) {
	cfg, err := region.Load(s.cfg.RegionPath)
	if err != nil {
		err = policyErr(importer.CodeRegionConfig, "region configuration: %v", err)
		s.auditAction(ctx, p, "revalidate", releaseID, registry.OutcomeRejected, reason, err.Error())
		return Revalidation{}, err
	}
	r, err := registry.Get(ctx, s.reg, releaseID)
	if err != nil {
		return Revalidation{}, err
	}
	if r == nil {
		err := fmt.Errorf("%w: %s", registry.ErrNotFound, releaseID)
		s.auditAction(ctx, p, "revalidate", releaseID, registry.OutcomeRejected, reason, err.Error())
		return Revalidation{}, err
	}
	res, err := s.revalidate(ctx, p, r, cfg, reason)
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || transient(err):
		// The result, pass or fail, is recorded with its own audit record
		// (release_revalidated) in the transaction that records it; without
		// one for this request, the release is as it was.
		s.auditAction(ctx, p, "revalidate", releaseID, registry.OutcomeFailed, reason,
			"interrupted before a result was recorded (a result is recorded only with a release_revalidated record of this request): "+err.Error())
	default:
		s.auditAction(ctx, p, "revalidate", releaseID, registry.OutcomeRejected, reason, err.Error())
	}
	return res, err
}

// revalidate evaluates release r against cfg's content policy and records
// the outcome (registry.RecordValidation). The release database is only
// read, on a read-only session; the counts min_counts compares are those
// recorded when it was built. Nothing is recorded unless every check ran to
// completion: an evaluation cut short by the deadline, a cancellation or a
// lost connection returns an error and leaves the release as it was.
func (s *Service) revalidate(ctx context.Context, p Principal, r *registry.Release, cfg region.Config, reason string) (Revalidation, error) {
	res := Revalidation{ReleaseID: r.ID}
	switch {
	case r.State != registry.StateReady && r.State != registry.StateRetired && r.State != registry.StateActive:
		return res, fmt.Errorf("%w: release %s is %s; only ready, retired or active releases can be revalidated", registry.ErrNotEligible, r.ID, r.State)
	case r.RegionID != cfg.ID:
		return res, policyErr(importer.CodeRegionConfig, "release %s serves region %q; this publisher is configured for %q, whose policy does not apply to it",
			r.ID, r.RegionID, cfg.ID)
	case r.SchemaRevision != schema.Revision() || r.StyleRevision != style.Revision():
		// The checks of this build assume its own layer catalog and style.
		return res, fmt.Errorf("%w: release %s was built with schema %s and style %s, this build validates schema %s and style %s; "+
			"publish the snapshot again to build a release this build can validate", ErrRevalidation, r.ID, r.SchemaRevision, r.StyleRevision,
			schema.Revision(), style.Revision())
	case !releaseid.Valid(r.ID) || r.Database != releaseid.DatabaseName(r.ID):
		return res, fmt.Errorf("%w: registry entry %s is malformed", ErrIncompatible, r.ID)
	}
	// One evaluation of a release at a time, across processes: the lock is
	// held on a registry session of its own, and the outcome is recorded on
	// that session, so a record is never made without the lock. A second
	// evaluation is refused at once (registry.ErrRevalidating), never queued.
	lock, err := s.cfg.Build.DB.Connect(ctx, s.cfg.RegistryDB)
	if err != nil {
		return res, interrupted(ctx, fmt.Errorf("connect registry: %w", err))
	}
	defer lock.Close(context.Background())
	if err := registry.LockRevalidation(ctx, lock, r.ID); err != nil {
		return res, interrupted(ctx, err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Closing the session (above) releases the lock if this fails.
		_ = registry.UnlockRevalidation(cctx, lock, r.ID)
	}()
	failpoint.Hit("revalidate.after_lock")
	began := time.Now()
	counts, err := releaseCounts(ctx, r, s.readCounts)
	if err != nil {
		if ctx.Err() != nil || transient(err) {
			return res, interrupted(ctx, err)
		}
		return res, fmt.Errorf("%w: %v", ErrRevalidation, err)
	}
	res.CountsFrom = "registry"
	if len(r.Counts) == 0 {
		res.CountsFrom = "import_report"
	}
	conn, err := s.revalidationParams(ctx).Connect(ctx, r.Database)
	if err != nil {
		if ctx.Err() != nil || transient(err) {
			return res, interrupted(ctx, fmt.Errorf("connect release database %s: %w", r.Database, err))
		}
		return res, fmt.Errorf("%w: cannot open %s: %v", ErrIncompatible, r.Database, err)
	}
	defer conn.Close(context.Background())
	// The checks the API applies before serving the release (id, schema
	// major, style, toolchain), then what the release records it was built
	// from: the region file whose policy is evaluated must describe the
	// region this release serves, exactly as at its build.
	rel, _, err := release.Check(ctx, conn, r.ID, s.cfg.Fontstacks)
	if err != nil {
		if ctx.Err() != nil || transient(err) {
			return res, interrupted(ctx, err)
		}
		return res, fmt.Errorf("%w: %v", ErrIncompatible, err)
	}
	if err := sameBuild(rel.Info, cfg); err != nil {
		return res, fmt.Errorf("%w: release %s: %v; publish the snapshot again under this region file instead", ErrRevalidation, r.ID, err)
	}
	report := importer.Revalidate(ctx, conn, cfg, counts)
	// Checks cut short by the deadline, a cancellation or a lost connection
	// would read as failures of the release: nothing is recorded.
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if err := conn.Ping(ctx); err != nil {
		return res, interrupted(ctx, fmt.Errorf("release database %s: connection lost during the evaluation: %w", r.Database, err))
	}
	policy := importer.PolicyOf(cfg)
	failed := report.Failed()
	res.Passed, res.PolicySHA256, res.Checks, res.Searches = len(failed) == 0, policy.SHA256, report.Checks, report.Searches
	res.DurationSeconds = math.Round(time.Since(began).Seconds()*1000) / 1000
	v := registry.Validation{PolicySHA256: policy.SHA256, Policy: policy.Canonical, Passed: res.Passed, Kind: registry.ValidationRevalidation,
		At: s.now().UTC(), Actor: p.Name, Source: p.Source, Checks: len(report.Checks), Failed: failed, DurationSeconds: res.DurationSeconds}
	if err := registry.RecordValidation(ctx, lock, r.ID, v, reason, p.RequestID); err != nil {
		return res, err
	}
	s.log.Info("release revalidated", "release_id", r.ID, "passed", res.Passed, "policy_sha256", policy.SHA256, "checks", len(report.Checks),
		"failed", len(failed), "duration_seconds", res.DurationSeconds, "actor", p.Name)
	return res, nil
}

// releaseParams are the parameters for reading a release database:
// read-only sessions, independent of the read-only default the database was
// given when its release was frozen.
func (s *Service) releaseParams() dbconn.Params {
	p := s.cfg.Build.DB
	p.ReadOnly = true
	return p
}

// revalidationParams are the release-database parameters of an evaluation
// bounded by ctx: read only, and with the time left until ctx's deadline as
// the statement timeout. pgx asks the server to cancel a statement the
// deadline interrupts; the timeout stops it on the server even if that
// request cannot be delivered.
func (s *Service) revalidationParams(ctx context.Context) dbconn.Params {
	p := s.releaseParams()
	if dl, ok := ctx.Deadline(); ok {
		p.StatementTimeout = max(time.Until(dl), time.Millisecond)
	}
	return p
}

// interrupted returns ctx's error if it is done (so a deadline is reported
// as one), else err.
func interrupted(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("%w: %v", cerr, err)
	}
	return err
}

// sameBuild compares what a release database records it was built from
// with what this build and the region file cfg would build it from: the
// region identity (id, name, box, view, all part of the release id) and the
// schema and style revisions. A release passing the checks of a region file
// that describes another region (or of another build) would prove nothing
// about what it serves.
func sameBuild(info release.Info, cfg region.Config) error {
	id := cfg.Identity()
	var diff []string
	if info.RegionID != id.ID {
		diff = append(diff, fmt.Sprintf("region id %q, the region file has %q", info.RegionID, id.ID))
	}
	if info.RegionName != id.Name {
		diff = append(diff, fmt.Sprintf("region name %q, the region file has %q", info.RegionName, id.Name))
	}
	if info.BBox != id.BBox {
		diff = append(diff, fmt.Sprintf("bbox %v, the region file has %v", info.BBox, id.BBox))
	}
	if info.Center != id.Center || info.Zoom != id.Zoom {
		diff = append(diff, fmt.Sprintf("view %v z%v, the region file has %v z%v", info.Center, info.Zoom, id.Center, id.Zoom))
	}
	if info.SchemaRevision != schema.Revision() || info.StyleRevision != style.Revision() {
		diff = append(diff, fmt.Sprintf("schema %s and style %s, this build has %s and %s", info.SchemaRevision, info.StyleRevision,
			schema.Revision(), style.Revision()))
	}
	if len(diff) > 0 {
		return fmt.Errorf("it was built with %s", strings.Join(diff, "; "))
	}
	return nil
}

// revalidationFailure is the error of a release that failed the content
// policy in force on a resubmission.
func revalidationFailure(id string, res Revalidation) error {
	var failed []string
	for _, c := range res.Checks {
		if !c.Passed {
			failed = append(failed, c.Name+": "+c.Detail)
		}
	}
	return fmt.Errorf("%w: release %s already exists but fails the validation policy in force (%s): %s; it stays as it is and is not activated "+
		"(no rebuild was needed: fix the region file's checks or the data, then revalidate)", importer.ErrValidation, id, short(res.PolicySHA256),
		strings.Join(failed, "; "))
}

// policyCurrent reports whether release r has passed the content policy of
// cfg.
func policyCurrent(r registry.Release, policy string) bool {
	return policy != "" && r.ValidationPolicySHA256 != nil && *r.ValidationPolicySHA256 == policy
}
