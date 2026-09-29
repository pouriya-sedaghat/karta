// Package publish is Karta's publication service: it turns verified
// snapshots into active releases without interrupting serving, and carries
// out the operator actions on releases.
//
// Every publication, from the inbox or the command line, takes the same
// path:
//
//	stage   copy the exact bytes into private staging (package inbox)
//	verify  full snapshot, provenance, region, timestamp and digest
//	        authorization checks on the staged copy (importer.Verify)
//	policy  forward-only rule against the active release (Forward)
//	space   capacity check for the active, retained and candidate releases
//	build   under the build lock, an isolated candidate database, validated
//	        and frozen as a ready release (importer.Build)
//	switch  one registry transaction moves the active pointer, compared
//	        against the active release seen when the build started, and
//	        writes the audit record (registry.Activate)
//
// Serving never pauses: the API keeps answering from the active release
// until it observes the new pointer, and keeps the replaced release
// servable to pinned clients for the pin grace period. A failure at any
// step leaves the active release unchanged. Rollback and activation by an
// operator use the same pointer transaction. Cleanup removes releases
// beyond the retention count once their pin grace has ended and no session
// uses their database. Recover makes every interrupted step safe to retry.
package publish

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pouriya-sedaghat/karta/internal/dbconn"
	"github.com/pouriya-sedaghat/karta/internal/failpoint"
	"github.com/pouriya-sedaghat/karta/internal/importer"
	"github.com/pouriya-sedaghat/karta/internal/inbox"
	"github.com/pouriya-sedaghat/karta/internal/region"
	"github.com/pouriya-sedaghat/karta/internal/registry"
	"github.com/pouriya-sedaghat/karta/internal/release"
	"github.com/pouriya-sedaghat/karta/internal/releaseid"
)

// Config configures the service.
type Config struct {
	RegionPath string
	// InboxDir is the watched inbox ("" disables the watcher).
	InboxDir        string
	StagingDir      string
	MaxInputBytes   int64
	InboxPoll       time.Duration
	InboxSettle     time.Duration
	InboxMaxEntries int
	// AutoActivate switches to a validated inbox submission automatically;
	// otherwise it stays ready for an operator to activate.
	AutoActivate bool
	// PinGrace is how long a replaced release stays servable to clients
	// that pinned it.
	PinGrace       time.Duration
	RetainReleases int
	// CleanupMargin is added to the pin grace before removal; it must
	// exceed the API's drain period.
	CleanupMargin   time.Duration
	CleanupInterval time.Duration
	// MaxAttempts bounds retries of an interrupted submission.
	MaxAttempts        int
	LockTimeout        time.Duration
	PointerLockTimeout time.Duration
	MaxFutureSkew      time.Duration
	// Storage checks.
	StagingReserveBytes int64
	StorageBudgetBytes  int64
	DBVolumePath        string
	MinFreeBytes        int64
	CandidateSizeFactor float64
	Build               importer.BuildOptions
	RegistryDB          string
	Fontstacks          []string
}

// Principal is who asks for an action.
type Principal struct {
	Name string
	// Source is operator_api, inbox, cli or system.
	Source    string
	RequestID string
}

// Service runs publications and operator actions.
type Service struct {
	cfg Config
	log *slog.Logger
	reg *pgxpool.Pool

	settler *inbox.Settler
	mu      sync.Mutex
	job     *JobStatus
	scan    ScanStatus
	now     func() time.Time
}

// JobStatus describes the publication in progress.
type JobStatus struct {
	SubmissionID int64     `json:"submission_id"`
	Name         string    `json:"name"`
	Source       string    `json:"source"`
	Phase        string    `json:"phase"`
	StartedAt    time.Time `json:"started_at"`
	PhaseAt      time.Time `json:"phase_started_at"`
	ReleaseID    string    `json:"release_id,omitempty"`
}

// ScanStatus is the result of the last inbox scan.
type ScanStatus struct {
	At      *time.Time     `json:"at"`
	Error   string         `json:"error,omitempty"`
	Pending []PendingEntry `json:"pending"`
}

// PendingEntry is an inbox submission that is not processed yet.
type PendingEntry struct {
	Name    string `json:"name"`
	Waiting string `json:"waiting"`
	Problem string `json:"problem,omitempty"`
}

// New connects to the registry and migrates it.
func New(ctx context.Context, cfg Config, log *slog.Logger) (*Service, error) {
	p := cfg.Build.DB
	p.MaxConns = 4
	pool, err := p.Pool(ctx, cfg.RegistryDB)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect registry: %w", err)
	}
	if err := registry.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate registry: %w", err)
	}
	return &Service{cfg: cfg, log: log, reg: pool, settler: inbox.NewSettler(), now: time.Now}, nil
}

// Close releases the registry pool.
func (s *Service) Close() { s.reg.Close() }

func (s *Service) setJob(j *JobStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.job = j
}

func (s *Service) phase(name, releaseID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != nil {
		s.job.Phase, s.job.PhaseAt = name, s.now()
		if releaseID != "" {
			s.job.ReleaseID = releaseID
		}
	}
}

// Outcome is the result of one publication.
type Outcome struct {
	SubmissionID int64            `json:"submission_id,omitempty"`
	State        string           `json:"state"`
	Code         string           `json:"reason_code,omitempty"`
	Reason       string           `json:"reason,omitempty"`
	ReleaseID    string           `json:"release_id,omitempty"`
	Previous     string           `json:"previous_release_id,omitempty"`
	Report       *importer.Report `json:"report,omitempty"`
	// Err classifies failures for exit codes (nil on success).
	Err error `json:"-"`
}

// ErrStorage is a publication refused or failed for lack of disk space.
var ErrStorage = importer.ErrStorage

// request is one publication to run.
type request struct {
	source            string
	name              string
	principal         Principal
	subID             int64
	staged            *inbox.Staged
	activate          bool
	allowRegionChange bool
	reason            string
}

// authorizer looks up operator authorizations for digests not pinned in
// the region configuration.
func (s *Service) authorizer(ctx context.Context, regionID, digest string, size int64) (string, error) {
	a, err := registry.Authorized(ctx, s.reg, regionID, digest, size)
	if err != nil || a == nil {
		return "", err
	}
	return fmt.Sprintf("operator authorization %d by %s", a.ID, a.CreatedBy), nil
}

// publish runs verify, policy, capacity, build and switch for a staged
// submission.
func (s *Service) publish(ctx context.Context, req request) Outcome {
	out := Outcome{SubmissionID: req.subID}
	fail := func(state, code string, err error) Outcome {
		out.State, out.Code, out.Reason, out.Err = state, code, err.Error(), err
		return out
	}
	cfg, err := region.Load(s.cfg.RegionPath)
	if err != nil {
		return fail(registry.SubRejected, importer.CodeRegionConfig, fmt.Errorf("%w: region: %v", importer.ErrInput, err))
	}
	s.phase("verifying", "")
	v, err := importer.Verify(ctx, importer.VerifyOptions{
		SnapshotPath: req.staged.SnapshotPath, ProvenancePath: req.staged.SidecarPath, Region: cfg,
		MaxInputBytes: s.cfg.MaxInputBytes, MaxFutureSkew: s.cfg.MaxFutureSkew, Now: s.now, Authorize: s.authorizer,
	})
	if err != nil {
		if code := importer.InputCode(err); code != "" {
			return fail(registry.SubRejected, code, err)
		}
		return fail(registry.SubFailed, CodeBuildFailed, err)
	}
	ts := v.Source.DataTimestamp
	s.updateSubmission(ctx, req.subID, registry.SubmissionUpdate{State: registry.SubProcessing, SHA256: v.Info.SHA256, SizeBytes: v.Info.Size, DataTimestamp: &ts})
	cand := Candidate{RegionID: cfg.ID, SHA256: v.Info.SHA256, DataTimestamp: ts}

	active, err := registry.Active(ctx, s.reg)
	if err != nil {
		return fail(registry.SubInterrupted, CodeInterrupted, err)
	}
	if err := Forward(active, cand, req.allowRegionChange); err != nil {
		return fail(registry.SubRejected, PolicyCode(err), err)
	}
	s.phase("capacity", "")
	if err := s.checkCapacity(ctx, v.Info.Size, cfg.ID); err != nil {
		return fail(registry.SubRejected, CodeInsufficient, err)
	}

	s.phase("waiting_for_build_lock", "")
	conn, err := s.cfg.Build.DB.Connect(ctx, s.cfg.RegistryDB)
	if err != nil {
		return fail(registry.SubInterrupted, CodeInterrupted, fmt.Errorf("connect registry: %w", err))
	}
	defer conn.Close(context.Background())
	if err := registry.LockImports(ctx, conn, s.cfg.LockTimeout); err != nil {
		return fail(registry.SubInterrupted, CodeInterrupted, err)
	}
	defer func() { _ = registry.UnlockImports(context.Background(), conn) }()

	// The active release seen under the build lock is the one this
	// publication replaces; if it changes before the switch (a rollback or
	// an operator activation), the build is kept ready, not activated.
	active, err = registry.Active(ctx, conn)
	if err != nil {
		return fail(registry.SubInterrupted, CodeInterrupted, err)
	}
	if err := Forward(active, cand, req.allowRegionChange); err != nil {
		return fail(registry.SubRejected, PolicyCode(err), err)
	}
	expected := ""
	opts := s.cfg.Build
	opts.Actor, opts.Source = req.principal.Name, req.principal.Source
	if req.subID != 0 {
		id := req.subID
		opts.SubmissionID = &id
	}
	if active != nil {
		expected = active.ID
		if active.RegionID == cfg.ID {
			opts.ActiveID = active.ID
			opts.ActiveCounts = s.activeCounts(ctx, active)
		}
	}
	s.phase("building", "")
	built, err := importer.Build(ctx, conn, v, opts, s.log)
	if built != nil {
		out.ReleaseID, out.Report = built.ReleaseID, built.Report
	}
	if err != nil {
		switch {
		case importer.InputCode(err) != "":
			return fail(registry.SubRejected, importer.InputCode(err), err)
		case errors.Is(err, importer.ErrValidation):
			return fail(registry.SubFailed, CodeValidationFailed, err)
		case errors.Is(err, importer.ErrStorage):
			return fail(registry.SubFailed, CodeInsufficient, err)
		case errors.Is(err, context.Canceled), transient(err):
			return fail(registry.SubInterrupted, CodeInterrupted, err)
		}
		return fail(registry.SubFailed, CodeBuildFailed, err)
	}
	s.phase("built", built.ReleaseID)
	if e := built.Existing; e != nil {
		resumable := e.State == registry.StateReady && e.ActivatedAt == nil
		switch {
		case e.State == registry.StateActive:
			out.State, out.Code, out.Reason = registry.SubDuplicate, CodeDuplicateActive, "release "+e.ID+" is already active"
			return out
		case e.State == registry.StateRemoving:
			return fail(registry.SubRejected, CodeBeingRemoved, policyErr(CodeBeingRemoved, "release %s is being removed; submit again after cleanup finishes", e.ID))
		case !resumable:
			return fail(registry.SubDuplicate, CodeDuplicateRetained, policyErr(CodeDuplicateRetained,
				"release %s already exists (%s); a duplicate never changes the active release: activate or roll back to it with the operator API", e.ID, e.State))
		}
		// A validated release that was never activated (an interrupted
		// publication, or a manual-activation build) is activated now if
		// this publication asks for it.
	}
	if !req.activate {
		out.State, out.Code, out.Reason = registry.SubReady, CodeManualActivation, "validated and ready; an operator activates it"
		return out
	}
	s.phase("activating", built.ReleaseID)
	res, err := registry.Activate(ctx, s.reg, registry.ActivateRequest{
		Target: built.ReleaseID, Expected: expected, CheckExpected: true, Action: "publish",
		Actor: req.principal.Name, Source: req.principal.Source, Reason: req.reason, RequestID: req.principal.RequestID,
		PinGrace: s.cfg.PinGrace,
		Allow: func(a, t *registry.Release) error {
			return Forward(a, Candidate{RegionID: t.RegionID, SHA256: t.SourceSHA256, DataTimestamp: ts}, req.allowRegionChange)
		},
		BeforeCommit: func() { failpoint.Hit("activate.before_commit") },
	}, s.cfg.PointerLockTimeout)
	if err != nil {
		switch {
		case errors.Is(err, registry.ErrActiveChanged):
			out.State, out.Code, out.Reason = registry.SubReady, CodeActiveChanged,
				"validated, but the active release changed during the build ("+err.Error()+"); kept ready for an operator to activate"
			return out
		case PolicyCode(err) != "":
			out.State, out.Code, out.Reason = registry.SubReady, PolicyCode(err), err.Error()
			return out
		}
		return fail(registry.SubInterrupted, CodeInterrupted, err)
	}
	failpoint.Hit("activate.after_commit")
	out.State, out.Previous = registry.SubPublished, res.Previous
	s.log.Info("release published", "release_id", built.ReleaseID, "previous", res.Previous)
	return out
}

// transient reports a build failure caused by the database going away
// (restart, crash, connection loss) rather than by the snapshot: it is
// retried, up to MaxAttempts, instead of being final.
func transient(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return strings.HasPrefix(pgErr.Code, "08") || strings.HasPrefix(pgErr.Code, "57P")
	}
	var netErr net.Error
	if errors.As(err, &netErr) || pgconn.SafeToRetry(err) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{"terminating connection due to administrator command", "server closed the connection unexpectedly",
		"the database system is shutting down", "the database system is starting up", "Connection refused", "conn closed",
		"connection reset by peer", "unexpected EOF", "failed to connect to"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// activeCounts are the active release's row counts: from the registry, or
// for a Stage 1 release from the report stored in its database.
func (s *Service) activeCounts(ctx context.Context, active *registry.Release) map[string]int64 {
	if len(active.Counts) > 0 {
		return active.Counts
	}
	conn, err := s.cfg.Build.DB.Connect(ctx, active.Database)
	if err != nil {
		s.log.Warn("could not read the active release's counts; relative checks skipped", "err", err)
		return nil
	}
	defer conn.Close(context.Background())
	var b []byte
	if err := conn.QueryRow(ctx, `SELECT report->'counts' FROM karta.release_info`).Scan(&b); err != nil {
		s.log.Warn("could not read the active release's counts; relative checks skipped", "err", err)
		return nil
	}
	var counts map[string]int64
	_ = json.Unmarshal(b, &counts)
	return counts
}

// candidateOverhead is added to the size estimate for the PostGIS template
// every release database starts from.
const candidateOverhead = 32 << 20

// Capacity reports storage use and limits.
type Capacity struct {
	ReleaseBytes      int64  `json:"release_bytes"`
	CandidateEstimate int64  `json:"candidate_estimate_bytes"`
	BudgetBytes       int64  `json:"budget_bytes"`
	DBVolumeFreeBytes *int64 `json:"db_volume_free_bytes"`
	StagingFreeBytes  *int64 `json:"staging_free_bytes"`
	MinFreeBytes      int64  `json:"min_free_bytes"`
}

func (s *Service) capacity(ctx context.Context, snapshotBytes int64, regionID string) (Capacity, error) {
	c := Capacity{BudgetBytes: s.cfg.StorageBudgetBytes, MinFreeBytes: s.cfg.MinFreeBytes}
	var err error
	if c.ReleaseBytes, err = s.releaseBytes(ctx); err != nil {
		return c, err
	}
	var prev int64
	if err := s.reg.QueryRow(ctx, `
SELECT COALESCE(max(database_bytes), 0)::bigint FROM registry.releases
WHERE region_id = $1 AND state IN ('ready', 'active', 'retired')`, regionID).Scan(&prev); err != nil {
		return c, err
	}
	c.CandidateEstimate = int64(float64(snapshotBytes)*s.cfg.CandidateSizeFactor) + candidateOverhead
	if p := prev + prev/4; p > c.CandidateEstimate {
		c.CandidateEstimate = p
	}
	if s.cfg.DBVolumePath != "" {
		if free, err := inbox.FreeBytes(s.cfg.DBVolumePath); err == nil {
			c.DBVolumeFreeBytes = &free
		}
	}
	if s.cfg.StagingDir != "" {
		if free, err := inbox.FreeBytes(s.cfg.StagingDir); err == nil {
			c.StagingFreeBytes = &free
		}
	}
	return c, nil
}

// releaseBytes sums the sizes of the release and candidate databases. Each
// is sized separately: a candidate can be dropped concurrently (a duplicate
// submission drops its candidate at once). A database that disappears
// between the listing and its sizing yields NULL (dropped before the name
// lookup) or an undefined-database error (dropped between the lookup and the
// privilege check); either way it is skipped instead of failing the sum, as
// one aggregate query over pg_database would.
func (s *Service) releaseBytes(ctx context.Context) (int64, error) {
	rows, err := s.reg.Query(ctx, `SELECT datname FROM pg_database WHERE datname ~ '^karta_(r|c)[0-9a-f]{24}$'`)
	if err != nil {
		return 0, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	var total int64
	for _, name := range names {
		var n *int64
		err := s.reg.QueryRow(ctx, `SELECT pg_database_size($1::name)`, name).Scan(&n)
		var pgErr *pgconn.PgError
		switch {
		case err == nil:
			if n != nil {
				total += *n
			}
		case errors.As(err, &pgErr) && (pgErr.Code == "3D000" || pgErr.Code == "58P01"):
			// dropped (or being dropped) since the listing
		default:
			return 0, err
		}
	}
	return total, nil
}

// checkCapacity refuses a build that would exceed the storage budget for
// release databases (active, retained, the candidate estimate), or leave
// less than the reserve free on the database volume.
func (s *Service) checkCapacity(ctx context.Context, snapshotBytes int64, regionID string) error {
	c, err := s.capacity(ctx, snapshotBytes, regionID)
	if err != nil {
		return err
	}
	if c.BudgetBytes > 0 && c.ReleaseBytes+c.CandidateEstimate > c.BudgetBytes {
		return fmt.Errorf("%w: release databases use %d bytes and the candidate needs about %d, above the budget of %d bytes "+
			"(KARTA_RELEASE_STORAGE_BUDGET_MB); run cleanup or raise the budget", ErrStorage, c.ReleaseBytes, c.CandidateEstimate, c.BudgetBytes)
	}
	if c.DBVolumeFreeBytes != nil && *c.DBVolumeFreeBytes < c.CandidateEstimate+c.MinFreeBytes {
		return fmt.Errorf("%w: the database volume has %d bytes free; the candidate needs about %d plus a reserve of %d",
			ErrStorage, *c.DBVolumeFreeBytes, c.CandidateEstimate, c.MinFreeBytes)
	}
	return nil
}

func (s *Service) updateSubmission(ctx context.Context, id int64, u registry.SubmissionUpdate) {
	if id == 0 {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := registry.UpdateSubmission(cctx, s.reg, id, u); err != nil {
		s.log.Error("could not record submission state", "submission_id", id, "err", err)
	}
}

// finish records an outcome on the submission and in the audit log.
func (s *Service) finish(ctx context.Context, req request, out Outcome, markerDigest string) {
	var sha string
	var size int64
	if req.staged != nil {
		sha, size = req.staged.SHA256, req.staged.Size
	}
	s.updateSubmission(ctx, req.subID, registry.SubmissionUpdate{State: out.State, ReasonCode: out.Code, Reason: out.Reason,
		ReleaseID: out.ReleaseID, MarkerSHA256: markerDigest, SHA256: sha, SizeBytes: size})
	outcome := registry.OutcomeSucceeded
	switch out.State {
	case registry.SubRejected, registry.SubDuplicate:
		outcome = registry.OutcomeRejected
	case registry.SubFailed, registry.SubInterrupted:
		outcome = registry.OutcomeFailed
	}
	detail := map[string]any{"submission_id": req.subID, "state": out.State}
	if out.Code != "" {
		detail["reason_code"] = out.Code
	}
	if out.ReleaseID != "" {
		detail["release_id"] = out.ReleaseID
	}
	if sha != "" {
		detail["sha256"] = sha
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := registry.Audit(cctx, s.reg, registry.AuditEntry{Actor: req.principal.Name, Source: req.principal.Source, Action: "submission",
		Target: req.name, Outcome: outcome, Reason: out.Reason, RequestID: req.principal.RequestID, Detail: detail}); err != nil {
		s.log.Error("could not write audit record", "err", err)
	}
	lvl := slog.LevelInfo
	if outcome != registry.OutcomeSucceeded {
		lvl = slog.LevelWarn
	}
	s.log.Log(ctx, lvl, "submission finished", "submission_id", req.subID, "name", req.name, "state", out.State,
		"reason_code", out.Code, "release_id", out.ReleaseID, "reason", out.Reason)
}

// newJobID names a staging directory.
func newJobID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// ImportOptions configure a command-line import.
type ImportOptions struct {
	SnapshotPath      string
	ProvenancePath    string
	Activate          bool
	AllowRegionChange bool
	Actor             string
	Reason            string
}

// ImportFile publishes an operator-named file (the `karta import` command):
// it is staged, verified and built exactly like an inbox submission.
func (s *Service) ImportFile(ctx context.Context, o ImportOptions) Outcome {
	principal := Principal{Name: o.Actor, Source: "cli"}
	prov := o.ProvenancePath
	if prov == "" {
		if fi, err := os.Lstat(o.SnapshotPath + ".provenance.json"); err == nil && !fi.IsDir() {
			prov = o.SnapshotPath + ".provenance.json"
		}
	}
	req := request{source: "cli", name: filepath.Base(o.SnapshotPath), principal: principal, activate: o.Activate,
		allowRegionChange: o.AllowRegionChange, reason: o.Reason}
	sub, _, err := registry.ClaimSubmission(ctx, s.reg, registry.Submission{Source: "cli", Name: req.name,
		Fingerprint: newJobID("cli:"), RegionID: s.regionID()}, nil)
	if err != nil {
		return Outcome{State: registry.SubFailed, Code: CodeInterrupted, Reason: err.Error(), Err: err}
	}
	req.subID = sub.ID
	s.setJob(&JobStatus{SubmissionID: sub.ID, Name: req.name, Source: "cli", Phase: "staging", StartedAt: s.now(), PhaseAt: s.now()})
	defer s.setJob(nil)
	staged, err := inbox.StageFile(o.SnapshotPath, prov, s.cfg.StagingDir, newJobID("cli-"),
		inbox.Options{MaxSnapshotBytes: s.cfg.MaxInputBytes, ReserveBytes: s.cfg.StagingReserveBytes})
	var out Outcome
	if err != nil {
		out = stageOutcome(sub.ID, err)
	} else {
		defer os.RemoveAll(staged.Dir)
		req.staged = staged
		failpoint.Hit("stage.after_copy")
		out = s.publish(ctx, req)
	}
	s.finish(ctx, req, out, "")
	if out.State == registry.SubPublished || out.State == registry.SubReady {
		s.cleanupAfterPublish(ctx)
	}
	return out
}

func stageOutcome(subID int64, err error) Outcome {
	code := inbox.CodeOf(err)
	out := Outcome{SubmissionID: subID, State: registry.SubRejected, Code: code, Reason: err.Error(), Err: fmt.Errorf("%w: %v", importer.ErrInput, err)}
	switch code {
	case inbox.CodeInsufficientSpace:
		out.Err = fmt.Errorf("%w: %v", ErrStorage, err)
	case "", inbox.CodeIO:
		out.State, out.Code = registry.SubFailed, inbox.CodeIO
		out.Err = err
	}
	return out
}

func (s *Service) regionID() string {
	cfg, err := region.Load(s.cfg.RegionPath)
	if err != nil {
		return "unknown"
	}
	return cfg.ID
}

// RunInbox scans the inbox until ctx ends.
func (s *Service) RunInbox(ctx context.Context) {
	t := time.NewTicker(s.cfg.InboxPoll)
	defer t.Stop()
	for {
		s.ScanOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ScanOnce lists the inbox and processes every complete, settled
// submission that has no final outcome yet, in name order.
func (s *Service) ScanOnce(ctx context.Context) {
	now := s.now()
	entries, err := inbox.Scan(s.cfg.InboxDir, s.cfg.InboxMaxEntries)
	st := ScanStatus{At: &now, Pending: []PendingEntry{}}
	if err != nil {
		st.Error = err.Error()
		s.log.Error("inbox scan failed", "err", err)
		s.mu.Lock()
		s.scan = st
		s.mu.Unlock()
		return
	}
	s.settler.Forget(entries)
	var ready []inbox.Entry
	for _, e := range entries {
		switch {
		case !e.Complete():
			st.Pending = append(st.Pending, PendingEntry{Name: e.Name, Waiting: e.Waiting(), Problem: e.Problem})
		case !s.settler.Stable(e, now, s.cfg.InboxSettle):
			st.Pending = append(st.Pending, PendingEntry{Name: e.Name, Waiting: "settling", Problem: e.Problem})
		default:
			ready = append(ready, e)
		}
	}
	s.mu.Lock()
	s.scan = st
	s.mu.Unlock()
	for _, e := range ready {
		if ctx.Err() != nil {
			return
		}
		s.processEntry(ctx, e)
	}
}

// retryable decides whether a recorded submission is processed again.
func (s *Service) retryable(ctx context.Context) func(*registry.Submission) bool {
	return func(prior *registry.Submission) bool {
		switch prior.State {
		case registry.SubInterrupted:
			return prior.Attempts < s.cfg.MaxAttempts
		case registry.SubProcessing:
			// The inbox is processed by this goroutine only, one submission
			// at a time: a processing record seen by a scan was left behind
			// when its outcome could not be written (registry outage).
			return prior.Attempts < s.cfg.MaxAttempts
		case registry.SubRejected:
			// An unauthorized digest is re-evaluated once an operator has
			// authorized exactly that digest (and size).
			if prior.ReasonCode == nil || *prior.ReasonCode != importer.CodeUnauthorizedDigest || prior.SHA256 == nil || prior.SizeBytes == nil {
				return false
			}
			a, err := registry.Authorized(ctx, s.reg, prior.RegionID, *prior.SHA256, *prior.SizeBytes)
			return err == nil && a != nil
		}
		return false
	}
}

func (s *Service) processEntry(ctx context.Context, e inbox.Entry) {
	principal := Principal{Name: "inbox", Source: "inbox"}
	fp := e.Fingerprint()
	prior, err := registry.SubmissionByFingerprint(ctx, s.reg, fp)
	if err != nil {
		s.log.Error("registry unavailable; inbox processing deferred", "err", err)
		return
	}
	if prior != nil && prior.State == registry.SubInterrupted && prior.Attempts >= s.cfg.MaxAttempts {
		s.updateSubmission(ctx, prior.ID, registry.SubmissionUpdate{State: registry.SubFailed, ReasonCode: CodeTooManyAttempts,
			Reason: fmt.Sprintf("interrupted %d times; touch the ready marker to submit again", prior.Attempts)})
		_ = registry.Audit(ctx, s.reg, registry.AuditEntry{Actor: principal.Name, Source: principal.Source, Action: "submission",
			Target: e.Name, Outcome: registry.OutcomeFailed, Reason: "too many interrupted attempts", Detail: map[string]any{"submission_id": prior.ID}})
		return
	}
	if prior != nil && !s.retryable(ctx)(prior) {
		return // final outcome already recorded for these exact files
	}
	sub, claimed, err := registry.ClaimSubmission(ctx, s.reg, registry.Submission{Source: "inbox", Name: e.Name, Fingerprint: fp,
		RegionID: s.regionID()}, s.retryable(ctx))
	if err != nil || !claimed {
		if err != nil {
			s.log.Error("could not claim submission", "name", e.Name, "err", err)
		}
		return
	}
	req := request{source: "inbox", name: e.Name, principal: principal, subID: sub.ID, activate: s.cfg.AutoActivate,
		reason: "inbox submission " + e.Name}
	s.log.Info("processing inbox submission", "name", e.Name, "submission_id", sub.ID, "attempt", sub.Attempts)
	s.setJob(&JobStatus{SubmissionID: sub.ID, Name: e.Name, Source: "inbox", Phase: "staging", StartedAt: s.now(), PhaseAt: s.now()})
	defer s.setJob(nil)
	staged, err := inbox.Stage(s.cfg.InboxDir, e, s.cfg.StagingDir, "sub-"+strconv.FormatInt(sub.ID, 10)+"-"+strconv.Itoa(sub.Attempts),
		inbox.Options{MaxSnapshotBytes: s.cfg.MaxInputBytes, ReserveBytes: s.cfg.StagingReserveBytes})
	var out Outcome
	marker := ""
	if err != nil {
		out = stageOutcome(sub.ID, err)
	} else {
		defer os.RemoveAll(staged.Dir)
		marker = staged.MarkerSHA256
		req.staged = staged
		failpoint.Hit("stage.after_copy")
		out = s.publish(ctx, req)
	}
	s.finish(ctx, req, out, marker)
	if out.State == registry.SubPublished {
		s.cleanupAfterPublish(ctx)
	}
}

func (s *Service) cleanupAfterPublish(ctx context.Context) {
	if _, err := s.Cleanup(ctx, Principal{Name: "system", Source: "system"}, "retention after publication", false); err != nil {
		s.log.Warn("retention cleanup after publication failed; it is retried later", "err", err)
	}
}

// RunMaintenance runs retention cleanup periodically until ctx ends.
func (s *Service) RunMaintenance(ctx context.Context) {
	t := time.NewTicker(s.cfg.CleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.Cleanup(ctx, Principal{Name: "system", Source: "system"}, "periodic retention", false); err != nil {
				s.log.Warn("periodic cleanup failed", "err", err)
			}
		}
	}
}

// ErrIncompatible is a release this build cannot serve on this database.
var ErrIncompatible = errors.New("release is not compatible with this server")

// ErrNoRollbackTarget means no retained release can be rolled back to.
var ErrNoRollbackTarget = errors.New("no retained release to roll back to")

// ActionRequest is an operator activation or rollback.
type ActionRequest struct {
	// ReleaseID is the target (required for activate; rollback defaults to
	// the most recently replaced retained release).
	ReleaseID string
	// ExpectedActive, when set, must be the active release at the time of
	// the switch ("none" for no active release); otherwise the active
	// release seen when the request started is expected.
	ExpectedActive    string
	AllowRegionChange bool
	Reason            string
}

// checkCompatible opens a release database and runs the same checks the API
// applies before serving it, including the toolchain comparison.
func (s *Service) checkCompatible(ctx context.Context, r *registry.Release) error {
	if !releaseid.Valid(r.ID) || r.Database != releaseid.DatabaseName(r.ID) {
		return fmt.Errorf("%w: registry entry %s is malformed", ErrIncompatible, r.ID)
	}
	conn, err := s.cfg.Build.DB.Connect(ctx, r.Database)
	if err != nil {
		return fmt.Errorf("%w: cannot open %s: %v", ErrIncompatible, r.Database, err)
	}
	defer conn.Close(context.Background())
	if _, _, err := release.Check(ctx, conn, r.ID, s.cfg.Fontstacks); err != nil {
		return fmt.Errorf("%w: %v", ErrIncompatible, err)
	}
	return nil
}

// Activate makes a ready or retired release active on an operator's
// request, subject to the forward rule (use Rollback to go back).
func (s *Service) Activate(ctx context.Context, p Principal, a ActionRequest) (registry.ActivateResult, error) {
	return s.switchTo(ctx, p, "activate", a, func(active, target *registry.Release) error {
		ts := time.Time{}
		if target.DataTimestamp != nil {
			ts = *target.DataTimestamp
		}
		return Forward(active, Candidate{RegionID: target.RegionID, SHA256: target.SourceSHA256, DataTimestamp: ts}, a.AllowRegionChange)
	})
}

// Rollback makes a retained, validated, compatible release active again.
func (s *Service) Rollback(ctx context.Context, p Principal, a ActionRequest) (registry.ActivateResult, error) {
	if a.ReleaseID == "" {
		var id string
		err := s.reg.QueryRow(ctx, `
SELECT release_id FROM registry.releases
WHERE state = 'retired' AND release_id IS DISTINCT FROM (SELECT release_id FROM registry.active_release)
ORDER BY deactivated_at DESC NULLS LAST, release_id LIMIT 1`).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			s.auditAction(ctx, p, "rollback", "", registry.OutcomeRejected, a.Reason, ErrNoRollbackTarget.Error())
			return registry.ActivateResult{}, ErrNoRollbackTarget
		}
		if err != nil {
			return registry.ActivateResult{}, err
		}
		a.ReleaseID = id
	}
	return s.switchTo(ctx, p, "rollback", a, func(active, target *registry.Release) error {
		if active != nil && active.RegionID != target.RegionID && !a.AllowRegionChange {
			return policyErr(CodeRegionChanged, "release %s serves region %q, the active release %q; a region change must be explicit",
				target.ID, target.RegionID, active.RegionID)
		}
		return nil
	})
}

func (s *Service) switchTo(ctx context.Context, p Principal, action string, a ActionRequest, allow func(active, target *registry.Release) error) (registry.ActivateResult, error) {
	target, err := registry.Get(ctx, s.reg, a.ReleaseID)
	if err != nil {
		return registry.ActivateResult{}, err
	}
	if target == nil {
		err := fmt.Errorf("%w: %s", registry.ErrNotFound, a.ReleaseID)
		s.auditAction(ctx, p, action, a.ReleaseID, registry.OutcomeRejected, a.Reason, err.Error())
		return registry.ActivateResult{}, err
	}
	expected := a.ExpectedActive
	if expected == "" {
		cur, err := registry.Active(ctx, s.reg)
		if err != nil {
			return registry.ActivateResult{}, err
		}
		if cur != nil {
			expected = cur.ID
		}
	} else if expected == "none" {
		expected = ""
	}
	if target.ID != expected && (target.State == registry.StateReady || target.State == registry.StateRetired) {
		if err := s.checkCompatible(ctx, target); err != nil {
			s.auditAction(ctx, p, action, target.ID, registry.OutcomeRejected, a.Reason, err.Error())
			return registry.ActivateResult{}, err
		}
	}
	res, err := registry.Activate(ctx, s.reg, registry.ActivateRequest{
		Target: target.ID, Expected: expected, CheckExpected: true, Action: action,
		Actor: p.Name, Source: p.Source, Reason: a.Reason, RequestID: p.RequestID, PinGrace: s.cfg.PinGrace, Allow: allow,
		BeforeCommit: func() { failpoint.Hit("activate.before_commit") },
	}, s.cfg.PointerLockTimeout)
	if err != nil {
		s.auditAction(ctx, p, action, target.ID, registry.OutcomeRejected, a.Reason, err.Error())
		return res, err
	}
	failpoint.Hit("activate.after_commit")
	s.log.Info("active release changed by operator", "action", action, "actor", p.Name, "release_id", res.Active, "previous", res.Previous, "changed", res.Changed)
	return res, nil
}

// auditAction records a refused or failed operator action (successful
// switches are audited inside their transaction).
func (s *Service) auditAction(ctx context.Context, p Principal, action, target, outcome, reason, detail string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := registry.Audit(cctx, s.reg, registry.AuditEntry{Actor: p.Name, Source: p.Source, Action: action, Target: target,
		Outcome: outcome, Reason: reason, RequestID: p.RequestID, Detail: map[string]any{"error": detail}}); err != nil {
		s.log.Error("could not write audit record", "err", err)
	}
}

// AuditDenied records an operator request refused before it reached the
// service (authentication or authorization failure).
func (s *Service) AuditDenied(ctx context.Context, p Principal, action, reason string) {
	s.auditAction(ctx, p, action, "", registry.OutcomeDenied, reason, reason)
}

// CleanupResult reports a cleanup.
type CleanupResult struct {
	DryRun    bool       `json:"dry_run"`
	Decisions []Decision `json:"decisions"`
	Removed   []string   `json:"removed"`
	Skipped   []Skipped  `json:"skipped"`
}

// Skipped is a release selected for removal that was not removed.
type Skipped struct {
	ReleaseID string `json:"release_id"`
	Why       string `json:"why"`
}

// Cleanup removes releases beyond the retention policy. A release is never
// removed while it is active, inside its pin grace (plus margin), or while
// any session is connected to its database (the drop itself fails then).
func (s *Service) Cleanup(ctx context.Context, p Principal, reason string, dryRun bool) (CleanupResult, error) {
	res := CleanupResult{DryRun: dryRun, Removed: []string{}, Skipped: []Skipped{}}
	rels, err := registry.List(ctx, s.reg, 1000)
	if err != nil {
		return res, err
	}
	active, err := registry.Active(ctx, s.reg)
	if err != nil {
		return res, err
	}
	activeID := ""
	if active != nil {
		activeID = active.ID
	}
	res.Decisions = Retention{Keep: s.cfg.RetainReleases, Margin: s.cfg.CleanupMargin}.Select(rels, activeID, s.now())
	// An operator's cleanup request is audited as a whole (each removal is
	// audited too); automatic cleanups only audit what they remove.
	if p.Source == "operator_api" {
		defer func() {
			outcome := registry.OutcomeSucceeded
			if dryRun || len(res.Removed) == 0 {
				outcome = registry.OutcomeNoop
			}
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_ = registry.Audit(cctx, s.reg, registry.AuditEntry{Actor: p.Name, Source: p.Source, Action: "cleanup", Outcome: outcome,
				Reason: reason, RequestID: p.RequestID, Detail: map[string]any{"dry_run": dryRun, "removed": res.Removed, "skipped": res.Skipped}})
		}()
	}
	if dryRun {
		return res, nil
	}
	byID := map[string]registry.Release{}
	for _, r := range rels {
		byID[r.ID] = r
	}
	conn, err := s.cfg.Build.DB.Connect(ctx, s.cfg.RegistryDB)
	if err != nil {
		return res, err
	}
	defer conn.Close(context.Background())
	for _, d := range res.Decisions {
		if d.Action != "remove" {
			continue
		}
		r := byID[d.ReleaseID]
		var sessions int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1`, r.Database).Scan(&sessions); err != nil {
			return res, err
		}
		if sessions > 0 {
			res.Skipped = append(res.Skipped, Skipped{r.ID, fmt.Sprintf("%d session(s) still connected to %s", sessions, r.Database)})
			continue
		}
		ok, err := registry.MarkRemoving(ctx, s.reg, r.ID, s.cfg.CleanupMargin, p.Name, p.Source, reason, p.RequestID)
		if err != nil {
			return res, err
		}
		if !ok {
			res.Skipped = append(res.Skipped, Skipped{r.ID, "no longer eligible (activated, pinned or changed concurrently)"})
			continue
		}
		failpoint.Hit("cleanup.after_mark")
		if err := importer.DropDatabase(ctx, conn, r.Database, false); err != nil {
			// In use again (for example a new API pool): stays removing,
			// retried by the next cleanup or at start.
			res.Skipped = append(res.Skipped, Skipped{r.ID, "drop failed, retried later: " + err.Error()})
			continue
		}
		failpoint.Hit("cleanup.after_drop")
		if err := registry.MarkRemoved(ctx, s.reg, r.ID); err != nil && !errors.Is(err, registry.ErrNotEligible) {
			return res, err
		}
		_ = registry.Audit(ctx, s.reg, registry.AuditEntry{Actor: p.Name, Source: p.Source, Action: "remove_release", Target: r.ID,
			Outcome: registry.OutcomeSucceeded, Reason: reason, RequestID: p.RequestID, Detail: map[string]any{"phase": "dropped", "database": r.Database}})
		res.Removed = append(res.Removed, r.ID)
		s.log.Info("release removed", "release_id", r.ID, "database", r.Database, "reason", reason)
	}
	return res, nil
}

var releaseDBPattern = regexp.MustCompile(`^karta_r[0-9a-f]{24}$`)

// RecoveryReport lists what Recover repaired.
type RecoveryReport struct {
	DroppedCandidates  bool     `json:"dropped_candidates"`
	FailedInterrupted  []string `json:"interrupted_releases_failed"`
	FinishedRemovals   []string `json:"finished_removals"`
	DroppedOrphans     []string `json:"dropped_orphan_databases"`
	InterruptedSubs    int64    `json:"interrupted_submissions"`
	CleanedStagingDirs []string `json:"cleaned_staging_dirs"`
}

// Recover repairs what an interrupted process left behind, under the build
// lock so no build is running: leftover candidate databases, releases stuck
// importing or validating (their databases are dropped and they are marked
// failed), unfinished removals, release databases the registry does not
// reference as retained, submissions left processing, and staging copies.
// The active pointer is never changed: it only ever moves in a committed
// transaction.
func (s *Service) Recover(ctx context.Context, cleanStaging bool) (RecoveryReport, error) {
	var rep RecoveryReport
	conn, err := s.cfg.Build.DB.Connect(ctx, s.cfg.RegistryDB)
	if err != nil {
		return rep, err
	}
	defer conn.Close(context.Background())
	if err := registry.LockImports(ctx, conn, s.cfg.LockTimeout); err != nil {
		return rep, err
	}
	defer func() { _ = registry.UnlockImports(context.Background(), conn) }()
	sys := Principal{Name: "system", Source: "system"}

	if err := importer.DropLeftoverCandidates(ctx, conn, s.log); err != nil {
		return rep, err
	}
	rep.DroppedCandidates = true

	rows, err := conn.Query(ctx, `SELECT release_id, database_name, state FROM registry.releases WHERE state IN ('importing', 'validating', 'removing')`)
	if err != nil {
		return rep, err
	}
	type stuck struct{ id, db, state string }
	var list []stuck
	for rows.Next() {
		var st stuck
		if err := rows.Scan(&st.id, &st.db, &st.state); err != nil {
			rows.Close()
			return rep, err
		}
		list = append(list, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}
	for _, st := range list {
		if err := importer.DropDatabase(ctx, conn, st.db, st.state != registry.StateRemoving); err != nil {
			s.log.Warn("could not drop database during recovery; retried later", "database", st.db, "err", err)
			continue
		}
		if st.state == registry.StateRemoving {
			// A concurrent cleanup may have finished the same removal.
			if err := registry.MarkRemoved(ctx, conn, st.id); err != nil && !errors.Is(err, registry.ErrNotEligible) {
				return rep, err
			}
			_ = registry.Audit(ctx, conn, registry.AuditEntry{Actor: sys.Name, Source: sys.Source, Action: "remove_release", Target: st.id,
				Outcome: registry.OutcomeSucceeded, Reason: "recovery finished an interrupted removal", Detail: map[string]any{"phase": "dropped"}})
			rep.FinishedRemovals = append(rep.FinishedRemovals, st.id)
			continue
		}
		if err := registry.Fail(ctx, conn, st.id, "interrupted: the process stopped while the release was "+st.state+"; the candidate was dropped",
			sys.Name, sys.Source); err != nil {
			return rep, err
		}
		rep.FailedInterrupted = append(rep.FailedInterrupted, st.id)
	}

	dbs, err := conn.Query(ctx, `
SELECT d.datname FROM pg_database d
WHERE d.datname ~ '^karta_r[0-9a-f]{24}$'
  AND NOT EXISTS (SELECT 1 FROM registry.releases r WHERE r.database_name = d.datname AND r.state IN ('ready', 'active', 'retired'))`)
	if err != nil {
		return rep, err
	}
	orphans, err := pgx.CollectRows(dbs, pgx.RowTo[string])
	if err != nil {
		return rep, err
	}
	for _, name := range orphans {
		if !releaseDBPattern.MatchString(name) {
			continue
		}
		if err := importer.DropDatabase(ctx, conn, name, false); err != nil {
			s.log.Warn("orphan release database in use; not dropped", "database", name, "err", err)
			continue
		}
		_ = registry.Audit(ctx, conn, registry.AuditEntry{Actor: sys.Name, Source: sys.Source, Action: "drop_orphan_database", Target: name,
			Outcome: registry.OutcomeSucceeded, Reason: "database not referenced by a retained release"})
		rep.DroppedOrphans = append(rep.DroppedOrphans, name)
	}

	tag, err := conn.Exec(ctx, `
UPDATE registry.submissions SET state = 'interrupted', reason_code = 'interrupted',
    reason = 'the process stopped while this submission was processing', updated_at = now()
WHERE state = 'processing'`)
	if err != nil {
		return rep, err
	}
	rep.InterruptedSubs = tag.RowsAffected()

	if cleanStaging && s.cfg.StagingDir != "" {
		if rep.CleanedStagingDirs, err = inbox.CleanStaging(s.cfg.StagingDir, nil); err != nil {
			return rep, err
		}
	}
	if len(rep.FailedInterrupted)+len(rep.FinishedRemovals)+len(rep.DroppedOrphans)+len(rep.CleanedStagingDirs) > 0 || rep.InterruptedSubs > 0 {
		_ = registry.Audit(ctx, conn, registry.AuditEntry{Actor: sys.Name, Source: sys.Source, Action: "recover", Outcome: registry.OutcomeSucceeded,
			Reason: "startup recovery", Detail: rep})
		s.log.Warn("recovered from an interrupted run", "report", rep)
	}
	return rep, nil
}

// LockStaging takes an exclusive lock on the staging directory for the
// life of the process, so two publishers never share one.
func LockStaging(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600) // #nosec G304 -- configured staging directory
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { // #nosec G115 -- file descriptors are small
		_ = f.Close()
		return nil, fmt.Errorf("staging directory %s is used by another publisher: %w", dir, err)
	}
	return f, nil
}

// Authorize records an operator authorization for a digest of the
// configured region.
func (s *Service) Authorize(ctx context.Context, p Principal, digest string, size *int64, expires *time.Time, reason string) (*registry.Authorization, bool, error) {
	cfg, err := region.Load(s.cfg.RegionPath)
	if err != nil {
		return nil, false, err
	}
	return registry.Authorize(ctx, s.reg, registry.Authorization{RegionID: cfg.ID, SHA256: digest, SizeBytes: size, Reason: reason,
		CreatedBy: p.Name, ExpiresAt: expires}, p.RequestID)
}

// Revoke closes an operator authorization.
func (s *Service) Revoke(ctx context.Context, p Principal, digest, reason string) (bool, error) {
	cfg, err := region.Load(s.cfg.RegionPath)
	if err != nil {
		return false, err
	}
	return registry.Revoke(ctx, s.reg, cfg.ID, digest, p.Name, reason, p.RequestID)
}

// ReleaseStatus is a release as reported to operators.
type ReleaseStatus struct {
	registry.Release
	// RollbackEligible: retained, validated and not active (compatibility
	// is checked when a rollback is requested).
	RollbackEligible bool `json:"rollback_eligible"`
	// PinnedServed: a retired release clients may still use.
	PinnedServed bool `json:"pinned_served"`
}

// Status is the operator view of publication.
type Status struct {
	Region         RegionStatus             `json:"region"`
	Policy         PolicyStatus             `json:"policy"`
	Active         *registry.Release        `json:"active"`
	Releases       []ReleaseStatus          `json:"releases"`
	Submissions    []registry.Submission    `json:"submissions"`
	Authorizations []registry.Authorization `json:"authorizations"`
	Inbox          InboxStatus              `json:"inbox"`
	Job            *JobStatus               `json:"job"`
	Storage        Capacity                 `json:"storage"`
	Failpoints     []string                 `json:"failpoints,omitempty"`
}

// RegionStatus describes the configured region.
type RegionStatus struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	PinnedDigests     []string `json:"pinned_digests"`
	RequireProvenance bool     `json:"require_provenance"`
	Error             string   `json:"error,omitempty"`
}

// PolicyStatus reports the publication policy in force.
type PolicyStatus struct {
	AutoActivate       bool    `json:"auto_activate"`
	PinGraceSeconds    float64 `json:"pin_grace_seconds"`
	RetainReleases     int     `json:"retain_releases"`
	MaxAttempts        int     `json:"max_attempts"`
	InboxSettleSeconds float64 `json:"inbox_settle_seconds"`
}

// InboxStatus reports the watcher.
type InboxStatus struct {
	Enabled  bool       `json:"enabled"`
	LastScan ScanStatus `json:"last_scan"`
}

// Status gathers the operator status.
func (s *Service) Status(ctx context.Context) (Status, error) {
	st := Status{Policy: PolicyStatus{AutoActivate: s.cfg.AutoActivate, PinGraceSeconds: s.cfg.PinGrace.Seconds(),
		RetainReleases: s.cfg.RetainReleases, MaxAttempts: s.cfg.MaxAttempts, InboxSettleSeconds: s.cfg.InboxSettle.Seconds()},
		Releases: []ReleaseStatus{}, Submissions: []registry.Submission{}, Authorizations: []registry.Authorization{}}
	cfg, err := region.Load(s.cfg.RegionPath)
	if err != nil {
		st.Region.Error = err.Error()
	} else {
		st.Region = RegionStatus{ID: cfg.ID, Name: cfg.Name, PinnedDigests: cfg.Source.PinnedDigests(), RequireProvenance: cfg.Source.RequireProvenance}
		if st.Region.PinnedDigests == nil {
			st.Region.PinnedDigests = []string{}
		}
		if st.Authorizations, err = registry.ListAuthorizations(ctx, s.reg, cfg.ID); err != nil {
			return st, err
		}
		if st.Authorizations == nil {
			st.Authorizations = []registry.Authorization{}
		}
	}
	if st.Active, err = registry.Active(ctx, s.reg); err != nil {
		return st, err
	}
	rels, err := registry.List(ctx, s.reg, 100)
	if err != nil {
		return st, err
	}
	now := s.now()
	for _, r := range rels {
		rs := ReleaseStatus{Release: r}
		isActive := st.Active != nil && st.Active.ID == r.ID
		rs.RollbackEligible = !isActive && (r.State == registry.StateReady || r.State == registry.StateRetired)
		rs.PinnedServed = r.State == registry.StateRetired && r.PinnedUntil != nil && now.Before(*r.PinnedUntil)
		st.Releases = append(st.Releases, rs)
	}
	sort.SliceStable(st.Releases, func(i, j int) bool { return st.Releases[i].CreatedAt.After(st.Releases[j].CreatedAt) })
	if subs, err := registry.ListSubmissions(ctx, s.reg, 50); err != nil {
		return st, err
	} else if subs != nil {
		st.Submissions = subs
	}
	if st.Storage, err = s.capacity(ctx, 0, st.Region.ID); err != nil {
		return st, err
	}
	s.mu.Lock()
	st.Inbox = InboxStatus{Enabled: s.cfg.InboxDir != "", LastScan: s.scan}
	if s.job != nil {
		j := *s.job
		st.Job = &j
	}
	s.mu.Unlock()
	if st.Inbox.LastScan.Pending == nil {
		st.Inbox.LastScan.Pending = []PendingEntry{}
	}
	st.Failpoints, _ = failpoint.Enabled()
	return st, nil
}

// Audit returns audit records, newest first.
func (s *Service) Audit(ctx context.Context, limit int, before int64) ([]registry.AuditEntry, error) {
	return registry.ListAudit(ctx, s.reg, limit, before)
}

// Ping checks the registry connection (operator readiness).
func (s *Service) Ping(ctx context.Context) error { return s.reg.Ping(ctx) }

// Params returns the database parameters (for the CLI's own connections).
func (s *Service) Params() dbconn.Params { return s.cfg.Build.DB }
