package online

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/failpoint"
	"github.com/pouriya-sedaghat/karta/internal/inbox"
	"github.com/pouriya-sedaghat/karta/internal/region"
)

// FetcherConfig configures `karta fetcher`.
type FetcherConfig struct {
	SourcePath string
	RegionPath string
	// Dir is the outbox: deliveries for the publisher, plus the hidden
	// state and partial-download directories.
	Dir string
	// MaxInputBytes bounds a snapshot (KARTA_MAX_INPUT_MB, as for imports).
	MaxInputBytes int64
	// ReserveBytes must stay free on the outbox volume after a download.
	ReserveBytes int64
	// MaxFutureSkew tolerates clock differences for issued_at.
	MaxFutureSkew time.Duration
	Version       string
}

// Request timeouts that are not configurable per source.
const (
	smallFileTimeout = 30 * time.Second
	dialTimeout      = 10 * time.Second
	headerTimeout    = 20 * time.Second
	handshakeTimeout = 10 * time.Second
	stateSaveEvery   = 5 * time.Second
	// keepDeliveries is how many complete deliveries stay in the outbox.
	keepDeliveries = 2
)

// Fetcher polls one source and delivers verified snapshots to its outbox.
type Fetcher struct {
	cfg   FetcherConfig
	log   *slog.Logger
	state State
	now   func() time.Time
	// jitter returns a factor in [0, 1).
	jitter    func() float64
	lastSaved time.Time
}

// NewFetcher prepares the outbox, loads the persisted state and recovers
// from an interrupted run: temporary files and deliveries without a
// completion marker are removed (the verified bytes stay in the partial
// directory and are delivered again), and a complete delivery the state
// does not record is adopted.
func NewFetcher(cfg FetcherConfig, log *slog.Logger) (*Fetcher, error) {
	f := &Fetcher{cfg: cfg, log: log, now: time.Now, jitter: rand.Float64} // #nosec G404 -- backoff jitter, not a secret
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{{filepath.Join(cfg.Dir, StateDirName), 0o755}, {filepath.Join(cfg.Dir, PartialDirName), 0o700}} {
		if err := os.Mkdir(d.path, d.mode); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if fi, err := os.Lstat(d.path); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", d.path)
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			return nil, err
		}
	}
	st, err := ReadState(cfg.Dir)
	if err != nil {
		log.Warn("fetcher state unreadable; starting from an empty state", "err", err)
	}
	if st != nil {
		f.state = *st
	}
	f.state.Version = stateVersion
	if err := f.recover(); err != nil {
		return nil, err
	}
	return f, f.save()
}

// State returns a copy of the current state.
func (f *Fetcher) State() State { return f.state }

var deliveryPattern = regexp.MustCompile(`^([a-z0-9][a-z0-9-]{0,62})-s([0-9]{12})-([0-9a-f]{12})$`)

// DeliveryName names the outbox submission of a manifest: region, serial
// (zero-padded, so name order is serial order) and digest prefix.
func DeliveryName(regionID string, serial int64, digest string) string {
	return fmt.Sprintf("%s-s%012d-%s", regionID, serial, digest[:12])
}

type outboxEntry struct {
	name     string
	serial   int64
	complete bool
	files    []string
}

func (f *Fetcher) listOutbox() ([]outboxEntry, error) {
	names, err := os.ReadDir(f.cfg.Dir)
	if err != nil {
		return nil, err
	}
	by := map[string]*outboxEntry{}
	for _, de := range names {
		n := de.Name()
		if strings.HasPrefix(n, ".") {
			continue
		}
		base := ""
		for _, suf := range []string{inbox.ManifestSuffix, inbox.SidecarSuffix, inbox.MarkerSuffix, inbox.SnapshotSuffix} {
			if strings.HasSuffix(n, suf) {
				base = strings.TrimSuffix(n, suf)
				break
			}
		}
		m := deliveryPattern.FindStringSubmatch(base)
		if m == nil {
			continue // not ours; never touched
		}
		e := by[base]
		if e == nil {
			serial, _ := strconv.ParseInt(m[2], 10, 64)
			e = &outboxEntry{name: base, serial: serial}
			by[base] = e
		}
		e.files = append(e.files, n)
		if n == base+inbox.MarkerSuffix {
			e.complete = true
		}
	}
	out := make([]outboxEntry, 0, len(by))
	for _, e := range by {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].serial < out[j].serial })
	return out, nil
}

func (f *Fetcher) recover() error {
	for _, d := range []string{f.cfg.Dir, filepath.Join(f.cfg.Dir, StateDirName), filepath.Join(f.cfg.Dir, PartialDirName)} {
		tmps, _ := filepath.Glob(filepath.Join(d, ".tmp-*"))
		for _, t := range tmps {
			_ = os.Remove(t)
		}
	}
	entries, err := f.listOutbox()
	if err != nil {
		return err
	}
	var newest *outboxEntry
	for i := range entries {
		e := &entries[i]
		if !e.complete {
			// Interrupted before its marker: never seen by the publisher.
			for _, n := range e.files {
				_ = os.Remove(filepath.Join(f.cfg.Dir, n))
			}
			f.log.Warn("removed an incomplete delivery left by an interrupted run", "name", e.name)
			continue
		}
		newest = e
	}
	if newest != nil && (f.state.Delivered == nil || newest.serial > f.state.Delivered.Serial) {
		d, err := f.readDelivery(newest.name)
		if err != nil {
			f.log.Warn("complete delivery not adopted", "name", newest.name, "err", err)
		} else {
			f.state.Delivered = d
			if d.Serial > f.state.HighestSerial {
				f.state.HighestSerial = d.Serial
			}
			f.log.Info("adopted a complete delivery left by an interrupted run", "name", d.Name)
		}
	}
	if d := f.state.Delivered; d != nil && f.deliveryPresent(d.Name) {
		// A crash after the marker can leave the delivered bytes' partial
		// file behind (a second link to the delivered snapshot).
		_ = os.Remove(PartialPath(filepath.Join(f.cfg.Dir, PartialDirName), d.SnapshotSHA256))
	}
	return nil
}

// readDelivery reads back the manifest of one of our own deliveries.
func (f *Fetcher) readDelivery(name string) (*Delivery, error) {
	b, err := readRegular(filepath.Join(f.cfg.Dir, name+inbox.ManifestSuffix), MaxEnvelopeBytes)
	if err != nil {
		return nil, err
	}
	m, err := ParseUnverified(b)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(filepath.Join(f.cfg.Dir, name+inbox.MarkerSuffix))
	if err != nil {
		return nil, err
	}
	return &Delivery{Name: name, Serial: m.Serial, SnapshotSHA256: m.Snapshot.SHA256, SizeBytes: m.Snapshot.SizeBytes,
		DataTimestamp: m.Snapshot.DataTimestamp.UTC(), DeliveredAt: fi.ModTime().UTC()}, nil
}

// ParseUnverified decodes an envelope's manifest without checking its
// signature: for bookkeeping of files the fetcher wrote itself, never for
// a trust decision.
func ParseUnverified(raw []byte) (*Manifest, error) {
	var env Envelope
	if err := strictDecode(raw, &env); err != nil {
		return nil, err
	}
	payload, err := decodeB64(env.Payload)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := strictDecode(payload, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (f *Fetcher) save() error {
	f.state.UpdatedAt = f.now().UTC()
	b, err := marshalIndent(f.state)
	if err != nil {
		return err
	}
	f.lastSaved = f.now()
	return writeFileAtomic(filepath.Join(f.cfg.Dir, StateDirName, StateFileName), b, 0o644)
}

// persist saves the state as part of a check: a failure fails the check
// (with the write's code, e.g. insufficient_storage), and nothing that
// depends on the saved state happens.
func (f *Fetcher) persist(what string) error {
	if err := f.save(); err != nil {
		return errorf(codeOr(CodeOf(err)), "could not record %s in the fetcher state: %s", what, errMsg(err))
	}
	return nil
}

// Run checks the source until ctx ends: at once if the persisted next
// attempt has passed, otherwise at that time (but never later than one poll
// interval after a start, so a shortened interval applies after a restart),
// then on the schedule.
func (f *Fetcher) Run(ctx context.Context) {
	first := true
	for {
		wait := time.Duration(0)
		if n := f.state.NextAttemptAt; n != nil {
			wait = n.Sub(f.now())
		}
		if first && wait > f.pollInterval() {
			wait = f.pollInterval()
		}
		first = false
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
		if ctx.Err() != nil {
			return
		}
		f.RunOnce(ctx)
	}
}

// RunOnce runs one check and schedules the next.
func (f *Fetcher) RunOnce(ctx context.Context) {
	err := f.CheckOnce(ctx)
	if ctx.Err() != nil {
		_ = f.save() // shutting down: keep progress, record no failure
		return
	}
	now := f.now().UTC()
	poll := f.pollInterval()
	var next time.Time
	switch code := CodeOf(err); {
	case err == nil:
		f.state.LastSuccessAt, f.state.LastError, f.state.ConsecutiveFailures = &now, nil, 0
		next = now.Add(time.Duration(float64(poll) * (0.9 + 0.2*f.jitter())))
		f.log.Info("source check succeeded", "serial", serialOf(f.state.Current), "next_attempt_at", next.Format(time.RFC3339))
	case code == CodeAbandoned:
		// The manifest is fine; one snapshot is set aside. Keep checking
		// at the normal interval so a different snapshot is noticed.
		f.state.LastError = &StateError{At: now, Code: code, Message: errMsg(err)}
		next = now.Add(poll)
		if d := f.state.Download; d != nil && d.AbandonedUntil != nil && d.AbandonedUntil.Before(next) {
			next = *d.AbandonedUntil
		}
		f.log.Warn("source check: snapshot set aside after repeated failures", "err", err, "next_attempt_at", next.Format(time.RFC3339))
	default:
		f.state.ConsecutiveFailures++
		f.state.LastError = &StateError{At: now, Code: codeOr(code), Message: errMsg(err)}
		next = now.Add(f.backoff(f.state.ConsecutiveFailures))
		f.log.Warn("source check failed", "code", codeOr(code), "err", errMsg(err), "consecutive_failures", f.state.ConsecutiveFailures,
			"next_attempt_at", next.Format(time.RFC3339))
	}
	f.state.NextAttemptAt = &next
	if serr := f.save(); serr != nil {
		f.log.Error("could not save fetcher state", "err", serr)
	}
}

func serialOf(m *ManifestSummary) int64 {
	if m == nil {
		return 0
	}
	return m.Serial
}

func codeOr(code string) string {
	if code == "" {
		return CodeIO
	}
	return code
}

func errMsg(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return truncate(e.Msg, 500)
	}
	return truncate(err.Error(), 500)
}

func (f *Fetcher) pollInterval() time.Duration {
	if src, err := LoadSource(f.cfg.SourcePath); err == nil {
		return src.PollInterval.D()
	}
	return time.Minute
}

// backoff is exponential from retry_initial, capped at retry_max, with
// jitter between half and all of it.
func (f *Fetcher) backoff(failures int) time.Duration {
	initial, maxDelay := DefaultRetryInitial, DefaultRetryMax
	if src, err := LoadSource(f.cfg.SourcePath); err == nil {
		initial, maxDelay = src.RetryInitial.D(), src.RetryMax.D()
	}
	return Backoff(initial, maxDelay, failures, f.jitter())
}

// Backoff returns min(max, initial * 2^(failures-1)) scaled by
// 0.5 + jitter/2, for jitter in [0, 1).
func Backoff(initial, maxDelay time.Duration, failures int, jitter float64) time.Duration {
	d := initial
	for i := 1; i < failures && d < maxDelay; i++ {
		d *= 2
	}
	if d > maxDelay {
		d = maxDelay
	}
	return time.Duration(float64(d) * (0.5 + jitter/2))
}

// CheckOnce runs one check: fetch and verify the manifest, and download and
// deliver the snapshot it names unless it was delivered already.
func (f *Fetcher) CheckOnce(ctx context.Context) error {
	now := f.now().UTC()
	f.state.LastCheckAt = &now
	src, err := LoadSource(f.cfg.SourcePath)
	if err != nil {
		return errorf(CodeConfig, "source file: %v", err)
	}
	reg, err := region.Load(f.cfg.RegionPath)
	if err != nil {
		return errorf(CodeConfig, "region file: %v", err)
	}
	if src.RegionID != reg.ID {
		return errorf(CodeConfig, "the source is for region %q, the region file is %q", src.RegionID, reg.ID)
	}
	f.state.Source = redact(src.manifestURL)
	req, err := f.requester(src)
	if err != nil {
		return err
	}
	raw, err := req.FetchSmall(ctx, src.manifestURL, MaxEnvelopeBytes, smallFileTimeout)
	if CodeOf(err) == CodeTooLarge {
		return errorf(CodeManifestTooLarge, "the manifest is larger than %d bytes", MaxEnvelopeBytes)
	}
	if err != nil {
		return err
	}
	v, err := Verify(raw, VerifyOptions{Source: src, Region: reg, Now: f.now(), Skew: f.cfg.MaxFutureSkew, MaxSnapshotBytes: f.cfg.MaxInputBytes})
	if err != nil {
		return err
	}
	m := v.Manifest
	switch cur := f.state.Current; {
	case m.Serial < f.state.HighestSerial:
		return errorf(CodeManifestReplayed, "manifest serial %d is lower than serial %d already verified; refusing an older manifest",
			m.Serial, f.state.HighestSerial)
	case m.Serial == f.state.HighestSerial && cur != nil && cur.Serial == m.Serial && cur.EnvelopeSHA256 != v.EnvelopeSHA256:
		return errorf(CodeManifestConflict, "a different manifest was published under serial %d", m.Serial)
	}
	f.state.HighestSerial, f.state.Current = m.Serial, summary(v, f.now())
	// The accepted serial is on disk before anything else happens: if a
	// restart forgot it while this manifest's snapshot was never delivered,
	// an older manifest that is still valid would pass the replay check.
	if err := f.persist("the verified manifest"); err != nil {
		return err
	}
	failpoint.Hit("fetch.after_manifest")
	if d := f.state.Delivered; d != nil && d.SnapshotSHA256 == m.Snapshot.SHA256 && f.deliveryPresent(d.Name) {
		if d.Serial == m.Serial || f.stillAuthorizes(d.Name, src, reg) {
			return nil // up to date
		}
		// The delivered manifest no longer verifies (it expired, or its key
		// was removed or retired), so the publisher may have refused to
		// activate these bytes: hand it this newer manifest for the same
		// snapshot, without downloading the snapshot again.
		return f.redeliver(ctx, req, src, v, raw, d)
	}

	dl := f.state.Download
	if dl == nil || dl.SnapshotSHA256 != m.Snapshot.SHA256 {
		dl = &DownloadState{SnapshotSHA256: m.Snapshot.SHA256, SizeBytes: m.Snapshot.SizeBytes, StartedAt: now}
		f.state.Download = dl
		f.removePartials(m.Snapshot.SHA256)
	}
	if dl.AbandonedUntil != nil {
		if now.Before(*dl.AbandonedUntil) {
			return errorf(CodeAbandoned, "snapshot %s failed %d downloads; not tried again before %s",
				m.Snapshot.SHA256, dl.Attempts, dl.AbandonedUntil.Format(time.RFC3339))
		}
		dl.AbandonedUntil, dl.Attempts = nil, 0
	}
	if err := f.checkSpace(m); err != nil {
		return err
	}
	prov, err := fetchProvenance(ctx, req, src, m)
	if err != nil {
		return err
	}
	u, err := ResolveURL(src, m.Snapshot.URL)
	if err != nil {
		return err
	}
	dl.Attempts++
	if err := f.persist("the download attempt"); err != nil {
		return err
	}
	part, err := req.Download(ctx, DownloadSpec{URL: u, SHA256: m.Snapshot.SHA256, Size: m.Snapshot.SizeBytes,
		Dir: filepath.Join(f.cfg.Dir, PartialDirName), Timeout: src.DownloadTimeout.D(), Stall: src.StallTimeout.D(),
		Progress: func(have int64) {
			dl.Bytes = have
			if f.now().Sub(f.lastSaved) >= stateSaveEvery {
				_ = f.save()
			}
		}})
	if err != nil {
		if ctx.Err() == nil && dl.Attempts >= src.MaxDownloadAttempts {
			until := f.now().Add(src.AbandonFor.D()).UTC()
			dl.AbandonedUntil = &until
		}
		return err
	}
	dl.Bytes = m.Snapshot.SizeBytes
	failpoint.Hit("fetch.after_download")
	return f.deliver(v, raw, prov, part)
}

// fetchProvenance fetches the provenance sidecar a manifest signs, if any,
// and checks it is exactly the signed bytes.
func fetchProvenance(ctx context.Context, req *Requester, src *Source, m Manifest) ([]byte, error) {
	p := m.Provenance
	if p == nil {
		return nil, nil
	}
	u, err := ResolveURL(src, p.URL)
	if err != nil {
		return nil, err
	}
	prov, err := req.FetchSmall(ctx, u, p.SizeBytes, smallFileTimeout)
	if err != nil {
		return nil, err
	}
	if int64(len(prov)) != p.SizeBytes {
		return nil, errorf(CodeSizeMismatch, "the provenance sidecar is %d bytes, the manifest signs %d", len(prov), p.SizeBytes)
	}
	if got := sha256Hex(prov); got != p.SHA256 {
		return nil, errorf(CodeDigestMismatch, "the provenance sidecar has SHA-256 %s, the manifest signs %s", got, p.SHA256)
	}
	return prov, nil
}

// stillAuthorizes reports whether the manifest of one of our deliveries
// still verifies under the source file now (as the publisher checks it
// again when it activates the snapshot).
func (f *Fetcher) stillAuthorizes(name string, src *Source, reg region.Config) bool {
	b, err := readRegular(filepath.Join(f.cfg.Dir, name+inbox.ManifestSuffix), MaxEnvelopeBytes)
	if err != nil {
		return false
	}
	_, err = Verify(b, VerifyOptions{Source: src, Region: reg, Now: f.now(), Skew: f.cfg.MaxFutureSkew, MaxSnapshotBytes: f.cfg.MaxInputBytes})
	return err == nil
}

// redeliver delivers a newer manifest for the snapshot of delivery d: the
// delivered bytes, checked against the signed digest again, are linked
// into the new delivery (no download).
func (f *Fetcher) redeliver(ctx context.Context, req *Requester, src *Source, v *Verified, raw []byte, d *Delivery) error {
	m := v.Manifest
	prov, err := fetchProvenance(ctx, req, src, m)
	if err != nil {
		return err
	}
	f.removePartials(m.Snapshot.SHA256)
	part := PartialPath(filepath.Join(f.cfg.Dir, PartialDirName), m.Snapshot.SHA256)
	_ = os.Remove(part)
	if err := os.Link(filepath.Join(f.cfg.Dir, d.Name+inbox.SnapshotSuffix), part); err != nil {
		return storageErr(err)
	}
	if sum, size, err := fileDigest(part); err != nil || sum != m.Snapshot.SHA256 || size != m.Snapshot.SizeBytes {
		_ = os.Remove(part)
		if err != nil {
			return storageErr(err)
		}
		f.state.Delivered = nil // the next check downloads the snapshot
		return errorf(CodeDigestMismatch, "the delivered snapshot %s no longer has the signed digest; it is downloaded again", d.Name)
	}
	f.log.Info("the delivered manifest no longer verifies; delivering a newer manifest for the same snapshot", "previous", d.Name,
		"serial", m.Serial)
	return f.deliver(v, raw, prov, part)
}

func (f *Fetcher) requester(src *Source) (*Requester, error) {
	opts := ClientOptions{Policy: Policy{Allowed: src.Networks()}, DialTimeout: dialTimeout, HeaderTimeout: headerTimeout,
		HandshakeLimit: handshakeTimeout}
	if src.CAFile != "" {
		roots, err := LoadRoots(src.CAFile)
		if err != nil {
			return nil, errorf(CodeConfig, "ca_file: %v", err)
		}
		opts.Roots = roots
	}
	r := &Requester{Client: NewHTTPClient(opts), UserAgent: "karta-fetcher/" + f.cfg.Version}
	if src.AuthTokenFile != "" {
		t, err := ReadToken(src.AuthTokenFile)
		if err != nil {
			return nil, errorf(CodeConfig, "auth_token_file: %v", err)
		}
		r.Token = t
	}
	return r, nil
}

func (f *Fetcher) deliveryPresent(name string) bool {
	_, err := os.Lstat(filepath.Join(f.cfg.Dir, name+inbox.MarkerSuffix))
	return err == nil
}

// removePartials deletes partial downloads of other snapshots.
func (f *Fetcher) removePartials(keep string) {
	dir := filepath.Join(f.cfg.Dir, PartialDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), keep+".") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// checkSpace refuses a download that would leave less than the reserve.
func (f *Fetcher) checkSpace(m Manifest) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(f.cfg.Dir, &st); err != nil {
		return nil // unknown: the write itself reports a full volume
	}
	free := int64(st.Bavail) * int64(st.Bsize) // #nosec G115 -- block counts fit in int64
	need := m.Snapshot.SizeBytes + f.cfg.ReserveBytes
	if p := m.Provenance; p != nil {
		need += p.SizeBytes
	}
	if fi, err := os.Lstat(PartialPath(filepath.Join(f.cfg.Dir, PartialDirName), m.Snapshot.SHA256)); err == nil {
		need -= fi.Size()
	}
	if free < need {
		return errorf(CodeStorage, "the outbox volume has %d bytes free; the download needs %d plus a reserve of %d",
			free, need-f.cfg.ReserveBytes, f.cfg.ReserveBytes)
	}
	return nil
}

// deliver hands a verified snapshot to the publisher with the completion
// protocol: the signed manifest (exact bytes), the provenance sidecar and
// the snapshot (a hard link to the verified download) are put in place
// first, the marker last; each by an atomic rename or link.
func (f *Fetcher) deliver(v *Verified, raw, prov []byte, part string) error {
	m := v.Manifest
	name := DeliveryName(m.RegionID, m.Serial, m.Snapshot.SHA256)
	dir := f.cfg.Dir
	for _, suf := range []string{inbox.MarkerSuffix, inbox.SnapshotSuffix, inbox.SidecarSuffix, inbox.ManifestSuffix} {
		_ = os.Remove(filepath.Join(dir, name+suf))
	}
	if err := writeFileAtomic(filepath.Join(dir, name+inbox.ManifestSuffix), raw, 0o644); err != nil {
		return err
	}
	if prov != nil {
		if err := writeFileAtomic(filepath.Join(dir, name+inbox.SidecarSuffix), prov, 0o644); err != nil {
			return err
		}
	}
	if err := os.Chmod(part, 0o644); err != nil { // #nosec G302 -- the publisher (another user) reads deliveries
		return storageErr(err)
	}
	if err := os.Link(part, filepath.Join(dir, name+inbox.SnapshotSuffix)); err != nil {
		return storageErr(err)
	}
	failpoint.Hit("fetch.before_marker")
	marker := fmt.Sprintf("%s  %s\n", m.Snapshot.SHA256, name+inbox.SnapshotSuffix)
	if err := writeFileAtomic(filepath.Join(dir, name+inbox.MarkerSuffix), []byte(marker), 0o644); err != nil {
		return err
	}
	syncDir(dir)
	failpoint.Hit("fetch.after_marker")
	_ = os.Remove(part)
	at := f.now().UTC()
	f.state.Delivered = &Delivery{Name: name, Serial: m.Serial, SnapshotSHA256: m.Snapshot.SHA256, SizeBytes: m.Snapshot.SizeBytes,
		DataTimestamp: m.Snapshot.DataTimestamp.UTC(), DeliveredAt: at}
	f.state.Download = nil
	f.log.Info("snapshot delivered to the publisher", "name", name, "serial", m.Serial, "sha256", m.Snapshot.SHA256,
		"size_bytes", m.Snapshot.SizeBytes, "data_timestamp", m.Snapshot.DataTimestamp.UTC().Format(time.RFC3339), "key_id", v.KeyID)
	f.prune(name)
	return f.save()
}

// prune removes all but the newest keepDeliveries complete deliveries.
func (f *Fetcher) prune(current string) {
	entries, err := f.listOutbox()
	if err != nil {
		return
	}
	var complete []outboxEntry
	for _, e := range entries {
		if e.complete && e.name != current {
			complete = append(complete, e)
		}
	}
	for i := 0; i < len(complete)-(keepDeliveries-1); i++ {
		e := complete[i]
		// Marker first, so the publisher never sees a half-removed delivery
		// as complete.
		_ = os.Remove(filepath.Join(f.cfg.Dir, e.name+inbox.MarkerSuffix))
		for _, n := range e.files {
			_ = os.Remove(filepath.Join(f.cfg.Dir, n))
		}
	}
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil { // #nosec G304 -- our outbox
		_ = d.Sync()
		_ = d.Close()
	}
}

// LockOutbox takes an exclusive lock on the outbox for the life of the
// process, so two fetchers never share one.
func LockOutbox(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600) // #nosec G304 -- configured outbox
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { // #nosec G115 -- file descriptors are small
		_ = f.Close()
		return nil, fmt.Errorf("outbox %s is used by another fetcher: %w", dir, err)
	}
	return f, nil
}
