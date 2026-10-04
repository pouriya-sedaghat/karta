package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// WatcherConfig configures `karta intake watch`.
type WatcherConfig struct {
	// Landing is the protected landing directory (read-only for the
	// watcher).
	Landing string
	// OwnerUID owns the landing directory and every delivery; WriterGID is
	// the one group allowed to write it (nil: none).
	OwnerUID  uint32
	WriterGID *uint32
	// Settle is how long a complete delivery must stay unchanged before it
	// is copied (a race check only: completeness comes from the producer's
	// digest).
	Settle time.Duration
	Poll   time.Duration
	// Handoff configures the handoff (its Letter is set to LetterWatch).
	Handoff HandoffConfig
	// Root, when set, prefixes Landing for the preflight's walk up its
	// parents (a test seam, like Statfs; deployments leave it empty).
	Root string
	// Statfs is a test seam for the preflight.
	Statfs func(string) (int64, error)
}

// landing is the landing directory's path as this process opens it.
func (c WatcherConfig) landing() string { return filepath.Join(c.Root, c.Landing) }

// Watcher watches the landing area and hands complete deliveries over.
type Watcher struct {
	cfg    WatcherConfig
	h      *Handoffer
	log    *slog.Logger
	state  State
	settle map[string]settled
	now    func() time.Time
	// lastPreflight is the last preflight outcome logged.
	lastPreflight string
}

type settled struct {
	fingerprint string
	since       time.Time
}

// NewWatcher prepares the watcher. It refuses to start from a state file
// that exists but cannot be read: the consumed list it holds keeps old
// landing files from being delivered again.
func NewWatcher(cfg WatcherConfig, log *slog.Logger) (*Watcher, error) {
	cfg.Handoff.Letter = LetterWatch
	if cfg.Handoff.Log == nil {
		cfg.Handoff.Log = log
	}
	h, err := NewHandoffer(cfg.Handoff)
	if err != nil {
		return nil, err
	}
	st, err := ReadState(cfg.Handoff.Dir)
	if err != nil {
		return nil, fmt.Errorf("the intake state %s exists but cannot be read (%w); restore it, or move it aside knowingly "+
			"(landing files it records as consumed would then be delivered again; duplicates change nothing)",
			filepath.Join(cfg.Handoff.Dir, StateDirName, StateFileName), err)
	}
	w := &Watcher{cfg: cfg, h: h, log: log, settle: map[string]settled{}, now: h.now}
	if st != nil {
		w.state = *st
	}
	w.state.Landing = cfg.Landing
	return w, w.save()
}

// State returns a copy of the watcher state.
func (w *Watcher) State() State { return w.state }

func (w *Watcher) save() error {
	w.state.UpdatedAt = w.now().UTC()
	return writeState(w.cfg.Handoff.Dir, &w.state)
}

func (w *Watcher) setError(code string, err error) {
	w.state.LastError = &StateError{At: w.now().UTC(), Code: code, Message: truncateRunes(err.Error(), 500)}
}

// Run scans until ctx ends, then records the clean stop.
func (w *Watcher) Run(ctx context.Context) {
	for {
		w.ScanOnce(ctx)
		select {
		case <-ctx.Done():
			now := w.now().UTC()
			w.state.StoppedAt = &now
			if err := w.save(); err != nil {
				w.log.Error("could not save the intake state", "err", err)
			}
			return
		case <-time.After(w.cfg.Poll):
		}
	}
}

// ScanOnce runs the preflight, settles finished and interrupted handoffs,
// and hands over every complete, settled landing delivery that is not
// consumed yet, one at a time.
func (w *Watcher) ScanOnce(ctx context.Context) {
	defer func() {
		if err := w.save(); err != nil {
			w.log.Error("could not save the intake state", "err", err)
		}
	}()
	now := w.now().UTC()
	w.state.LastScanAt, w.state.StoppedAt = &now, nil
	pf := RunPreflight(PreflightOptions{Dir: w.cfg.Landing, Root: w.cfg.Root, OwnerUID: w.cfg.OwnerUID, WriterGID: w.cfg.WriterGID,
		Statfs: w.cfg.Statfs, Now: w.now})
	w.state.Preflight = pf
	if summary := fmt.Sprint(pf.OK, pf.Problems); summary != w.lastPreflight {
		w.lastPreflight = summary
		if pf.OK {
			w.log.Info("landing preflight passed", "landing", w.cfg.Landing, "fs_type", pf.FSType)
		} else {
			w.log.Error("landing preflight failed: nothing is delivered until it passes (fail closed); use the authenticated command meanwhile",
				"landing", w.cfg.Landing, "problems", pf.Problems)
		}
	}
	rec, err := w.h.reconcile(ctx, w.resume)
	if err != nil {
		w.setError("operator_api", err)
		w.log.Warn("cannot reach the publisher's intake records; retrying next scan", "err", err)
		return
	}
	w.track(rec)
	if !pf.OK {
		w.state.Entries = nil
		return
	}
	entries, err := scanLanding(w.cfg.landing(), w.cfg.OwnerUID, w.cfg.Handoff.MaxInputBytes)
	if err != nil {
		w.setError("landing_scan", err)
		return
	}
	w.forget(entries)
	out := make([]LandingEntry, 0, len(entries))
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		out = append(out, w.consider(ctx, e))
	}
	w.state.Entries = out
	w.state.LastError = nil
}

func (w *Watcher) consumed(fp string) *Consumed {
	for i := range w.state.Consumed {
		if w.state.Consumed[i].Fingerprint == fp {
			return &w.state.Consumed[i]
		}
	}
	return nil
}

func (w *Watcher) consume(e landingEntry, fp, outcome, code string) {
	w.state.Consumed = append(w.state.Consumed, Consumed{Fingerprint: fp, Name: e.Name, Outcome: outcome, Code: code, At: w.now().UTC()})
}

func (w *Watcher) forget(entries []landingEntry) {
	keep := map[string]bool{}
	for _, e := range entries {
		keep[e.Name] = true
	}
	for n := range w.settle {
		if !keep[n] {
			delete(w.settle, n)
		}
	}
}

// consider decides one landing delivery.
func (w *Watcher) consider(ctx context.Context, e landingEntry) LandingEntry {
	le := LandingEntry{Name: e.Name}
	if e.Snapshot != nil {
		le.SizeBytes = e.Snapshot.Size
	}
	fp := e.fingerprint()
	if c := w.consumed(fp); c != nil {
		le.State, le.Code = c.Outcome, c.Code
		return le
	}
	refused := func(code, detail string) LandingEntry {
		w.consume(e, fp, StateRefused, code)
		w.log.Warn("landing delivery refused", "name", e.Name, "code", code, "detail", detail)
		le.State, le.Code, le.Detail = StateRefused, code, detail
		return le
	}
	switch {
	case e.Problem != "":
		return refused(e.Problem, e.Detail)
	case e.Snapshot == nil || e.Completion == nil:
		le.State = WaitingForCompletion
		return le
	}
	b, err := readSmall(filepath.Join(w.cfg.landing(), e.Name+CompletionSuffix), e.Completion, MaxCompletionBytes)
	if err != nil {
		le.State, le.Code, le.Detail = WaitingSettling, RefusalCode(err), err.Error()
		return le
	}
	c, err := ParseCompletion(b, e.snapshotName())
	switch {
	case err != nil:
		return refused(CodeInvalidCompletion, err.Error())
	case e.Completion.ModTime.Before(e.Snapshot.ModTime):
		return refused(CodeStaleCompletion, fmt.Sprintf("the completion marker (%s) is older than %s (%s): the snapshot was replaced after "+
			"the marker was written", e.Completion.ModTime.UTC().Format(time.RFC3339Nano), e.snapshotName(), e.Snapshot.ModTime.UTC().Format(time.RFC3339Nano)))
	case c.SizeBytes != e.Snapshot.Size:
		return refused(CodeSizeMismatch, fmt.Sprintf("%s is %d bytes, the completion marker says %d: an incomplete or different copy",
			e.snapshotName(), e.Snapshot.Size, c.SizeBytes))
	}
	if !w.stable(e, fp) {
		le.State = WaitingSettling
		return le
	}
	d := delivery{path: filepath.Join(w.cfg.landing(), e.snapshotName()), want: e.Snapshot, expectSHA256: c.SHA256, expectSize: c.SizeBytes,
		reason: fmt.Sprintf("intake watcher: landing delivery %s (owner UID %d, %d bytes, modified %s); SHA-256 from the producer's completion marker",
			e.Name, e.Snapshot.UID, e.Snapshot.Size, e.Snapshot.ModTime.UTC().Format(time.RFC3339))}
	if e.Sidecar != nil {
		d.sidecarPath, d.sidecarWant = filepath.Join(w.cfg.landing(), e.Name+SidecarSuffix), e.Sidecar
	}
	d.beforeAuthorize = func(name, digest string, size int64) error {
		// A retry under another name (the first was taken) replaces the
		// pending record.
		w.dropPending(e.Name, nil)
		w.state.Handoffs = append(w.state.Handoffs, Handoff{Name: name, From: e.Name, SHA256: digest, SizeBytes: size, Channel: "intake_watch",
			HandedOffAt: w.now().UTC(), LandingFingerprint: fp})
		return w.save()
	}
	ho, err := w.h.deliver(ctx, d)
	switch code := RefusalCode(err); {
	case err == nil:
		for i := range w.state.Handoffs {
			if w.state.Handoffs[i].Name == ho.Name {
				w.state.Handoffs[i].AuthorizationID, w.state.Handoffs[i].HandedOffAt = ho.AuthorizationID, ho.At
			}
		}
		w.consume(e, fp, StateDelivered, "")
		w.log.Info("landing delivery handed to the publisher", "name", e.Name, "handoff", ho.Name, "sha256", ho.SHA256, "size_bytes", ho.Size,
			"authorization_id", ho.AuthorizationID)
		le.State = StateDelivered
	case errors.Is(err, ErrQueueFull):
		le.State = WaitingQueueFull
	case code == CodeStorage, code == CodeChanged:
		le.State, le.Code, le.Detail = WaitingSettling, code, err.Error()
		w.setError(code, err)
	case code != "":
		le = refused(code, err.Error())
	default:
		le.State, le.Detail = WaitingSettling, err.Error()
		w.setError("handoff", err)
		w.log.Warn("handoff failed; retrying next scan", "name", e.Name, "err", err)
	}
	w.dropPending(e.Name, ho)
	return le
}

// dropPending removes the pending record of a handoff that did not happen.
func (w *Watcher) dropPending(from string, ho *handedOff) {
	if ho != nil {
		return
	}
	out := w.state.Handoffs[:0]
	for _, h := range w.state.Handoffs {
		if h.From == from && h.AuthorizationID == 0 {
			continue
		}
		out = append(out, h)
	}
	w.state.Handoffs = out
}

func (w *Watcher) stable(e landingEntry, fp string) bool {
	now := w.now()
	cur, ok := w.settle[e.Name]
	if !ok || cur.fingerprint != fp {
		w.settle[e.Name] = settled{fingerprint: fp, since: now}
		return w.cfg.Settle <= 0
	}
	return now.Sub(cur.since) >= w.cfg.Settle
}

// resume allows completing a handoff interrupted after its authorization
// only when this watcher recorded it and its landing delivery is still the
// same files.
func (w *Watcher) resume(rec Record) bool {
	if !w.state.Preflight.OK {
		return false // the landing area is not trusted now
	}
	name := *rec.Authorization.IntakeName
	for _, h := range w.state.Handoffs {
		if h.Name != name || h.LandingFingerprint == "" {
			continue
		}
		entries, err := scanLanding(w.cfg.landing(), w.cfg.OwnerUID, w.cfg.Handoff.MaxInputBytes)
		if err != nil {
			return false
		}
		for _, e := range entries {
			if e.Name == h.From && e.fingerprint() == h.LandingFingerprint && e.Problem == "" {
				// The landing delivery is handed off by this completion:
				// never deliver it again.
				if w.consumed(h.LandingFingerprint) == nil {
					w.consume(e, h.LandingFingerprint, StateDelivered, "")
				}
				return true
			}
		}
	}
	return false
}

// track refreshes the reported handoffs from the publisher's records.
func (w *Watcher) track(rec Reconciled) {
	by := map[string]Record{}
	for _, r := range rec.Records {
		if r.Authorization.IntakeName != nil {
			by[*r.Authorization.IntakeName] = r
		}
	}
	finished := map[string]bool{}
	for _, r := range rec.Finished {
		finished[*r.Authorization.IntakeName] = true
	}
	out := w.state.Handoffs[:0]
	for _, h := range w.state.Handoffs {
		r, ok := by[h.Name]
		if !ok {
			continue // never authorized (a crash before it), or older than the records kept
		}
		h.AuthorizationID = r.Authorization.ID
		if s := r.Submission; s != nil {
			h.SubmissionState, h.FinishedAt = s.State, s.FinishedAt
			h.ReasonCode, h.ReleaseID = "", ""
			if s.ReasonCode != nil {
				h.ReasonCode = *s.ReasonCode
			}
			if s.ReleaseID != nil {
				h.ReleaseID = *s.ReleaseID
			}
		}
		if finished[h.Name] {
			hh := h
			w.state.LastOutcome = &hh
			w.log.Info("intake delivery finished", "handoff", h.Name, "from", h.From, "state", h.SubmissionState, "reason_code", h.ReasonCode,
				"release_id", h.ReleaseID)
			continue
		}
		if _, err := os.Lstat(filepath.Join(w.cfg.Handoff.Dir, h.Name+".osm.pbf.ready")); err != nil && !r.Authorization.Open(w.now()) {
			continue // discarded by reconciliation
		}
		out = append(out, h)
	}
	w.state.Handoffs = out
}
