package bridge

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/failpoint"
	"github.com/pouriya-sedaghat/karta/internal/importer"
	"github.com/pouriya-sedaghat/karta/internal/inbox"
	"github.com/pouriya-sedaghat/karta/internal/online"
	"github.com/pouriya-sedaghat/karta/internal/osmfile"
	"github.com/pouriya-sedaghat/karta/internal/provenance"
	"github.com/pouriya-sedaghat/karta/internal/region"
	"github.com/pouriya-sedaghat/karta/internal/safefile"
)

// Signer failure and hold codes.
const (
	CodeNotNewer        = "not_newer"
	CodeStateBehind     = "state_behind_published"
	CodeClockBehind     = "clock_behind"
	CodeSerialExhausted = "serial_exhausted"
	CodeAssetMissing    = "asset_missing"
	CodeNoAcquisition   = "acquisition_record_missing"
	CodeNoHeaderBox     = "no_header_box"
	// CodePublishedUntrusted is a published manifest that does not verify
	// with the signer's own keys and region: it is not adopted (fail closed).
	CodePublishedUntrusted = "published_manifest_untrusted"
	// CodeEnvelopeConflict is a published manifest with the serial of the
	// signer's persisted envelope but other bytes (fail closed).
	CodeEnvelopeConflict = "published_envelope_conflict"
)

// Published file names (what serve serves).
const (
	ManifestFile = "manifest.json"
	SnapshotsDir = "snapshots"
	// ProvenanceFormat tags the bridge's own provenance object.
	ProvenanceFormat = "karta-bridge-provenance/1"
)

// SignConfig configures `karta bridge sign`.
type SignConfig struct {
	ConfigPath string
	RegionPath string
	// Spool is acquire's output (read-only here).
	Spool string
	// Publish is what serve serves (written only here).
	Publish string
	// StateDir holds the signer's own state (high-water serial, envelopes).
	StateDir      string
	MaxInputBytes int64
	MaxFutureSkew time.Duration
	Poll          time.Duration
	Version       string
}

// Signer signs and publishes manifests for new acquisitions, and renews
// the held one before it expires.
type Signer struct {
	cfg   SignConfig
	conf  *SignerConfig
	keys  []online.Signer
	state SignerState
	log   *slog.Logger
	now   func() time.Time
	// lock is held for the signer's life: one process allocates serials.
	lock *os.File
}

// LockFileName is the signer's single-instance lock in its state directory.
const LockFileName = "lock"

// ErrFailClosed is a signer that must not sign until an operator acts.
var ErrFailClosed = errors.New("the signer refuses to sign")

// NewSigner loads the configuration and keys, reconciles its state with
// the published manifest, and promotes a manifest a crash left unpublished.
// raise, when set, is an operator's raise of the high-water serial after a
// state restore or loss (it never lowers it).
func NewSigner(cfg SignConfig, log *slog.Logger, raise *Raise) (*Signer, error) {
	conf, err := LoadSignerConfig(cfg.ConfigPath)
	if err != nil {
		return nil, err
	}
	s := &Signer{cfg: cfg, conf: conf, log: log, now: time.Now}
	for _, k := range conf.Keys {
		key, err := online.LoadSigningKey(k.File)
		if err != nil {
			return nil, fmt.Errorf("signing key %s: %w", k.ID, err)
		}
		s.keys = append(s.keys, online.Signer{KeyID: k.ID, Key: key})
	}
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{{filepath.Join(cfg.Publish, SnapshotsDir), 0o755}, {filepath.Join(cfg.Publish, stagingDirName), 0o700},
		{filepath.Join(cfg.Publish, StatusDirName), 0o755}, {cfg.StateDir, 0o700}} {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return nil, err
		}
	}
	lk, err := safefile.Lock(filepath.Join(cfg.StateDir, LockFileName))
	if err != nil {
		return nil, fmt.Errorf("another signer holds %s (stop it first; a raise needs the signer stopped): %w",
			filepath.Join(cfg.StateDir, LockFileName), err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = lk.Close()
		}
	}()
	s.lock = lk
	st, err := ReadSignerState(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("%w: the signer state exists but cannot be read (%v); restore it, then if needed raise the high-water serial", ErrFailClosed, err)
	}
	pubRaw, pub, err := s.readPublished()
	if err != nil {
		return nil, err
	}
	published := int64(0)
	if pub != nil {
		published = pub.Serial
	}
	if st != nil {
		s.state = *st
	}
	s.state.Version, s.state.BridgeID = stateVersion, conf.BridgeID
	switch {
	case raise != nil:
		floor := max(published, s.state.HighWater)
		if raise.To < floor || raise.To > MaxSerial || strings.TrimSpace(raise.Reason) == "" {
			return nil, fmt.Errorf("the high-water serial can only be raised, to at least %d (the published manifest and the state) and at most %d, "+
				"with a reason", floor, int64(MaxSerial))
		}
		raise.From, raise.At = s.state.HighWater, s.now().UTC()
		s.state.HighWater = raise.To
		s.state.Raises = append(s.state.Raises, *raise)
		log.Warn("high-water serial raised by an operator", "from", raise.From, "to", raise.To, "reason", raise.Reason)
	case st == nil && published > 0:
		return nil, fmt.Errorf("%w (%s): there is no signer state but a manifest with serial %d is published: the state was lost; restore it, "+
			"or raise the high-water serial (--raise-high-water N, N at least %d and at least the serial Karta accepted, "+
			"operator status online.verified.serial)", ErrFailClosed, CodeStateBehind, published, published)
	case published > s.state.HighWater:
		return nil, fmt.Errorf("%w (%s): the state's high-water serial %d is below the published manifest's %d: the state is older than the "+
			"publication (restored from a backup?); raise the high-water serial (--raise-high-water N, N at least %d and at least the "+
			"serial Karta accepted)", ErrFailClosed, CodeStateBehind, s.state.HighWater, published, published)
	}
	if err := s.clockCheck(); err != nil {
		return nil, err
	}
	_, _ = inbox.CleanStaging(filepath.Join(cfg.Publish, stagingDirName), nil)
	if err := s.reconcilePublished(pubRaw, pub); err != nil {
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

// Close releases the single-instance lock.
func (s *Signer) Close() error {
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

// readPublished reads the published manifest (nil if there is none) and
// verifies it as one of the signer's own: a valid signature by one of its
// keys (none claiming one of them without verifying) and bound to its region
// (id and box). Anything else fails closed: the signer never takes a serial,
// a snapshot or an envelope from a publication it cannot prove it made.
func (s *Signer) readPublished() ([]byte, *online.Manifest, error) {
	raw, err := safefile.ReadRegular(filepath.Join(s.cfg.Publish, ManifestFile), online.MaxEnvelopeBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: the published manifest cannot be read: %v", ErrFailClosed, err)
	}
	cfg, err := region.Load(s.cfg.RegionPath)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: the region file cannot be read to check the published manifest: %v", ErrFailClosed, err)
	}
	m, _, err := online.VerifySignedBy(raw, s.PublicKeys(), cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("%w (%s): the published manifest is not a valid publication of this signer (keys %v, region %q): %v; "+
			"find out who wrote it, then restore the signer's own manifest or remove it (docs/runbook.md)", ErrFailClosed, CodePublishedUntrusted,
			s.keyIDs(), cfg.ID, err)
	}
	return raw, m, nil
}

func (s *Signer) keyIDs() []string {
	ids := make([]string, 0, len(s.keys))
	for _, k := range s.keys {
		ids = append(ids, k.KeyID)
	}
	return ids
}

// reconcilePublished makes the current manifest agree with the verified
// published one (pub, nil if none). An envelope persisted but not yet
// published (a crash or a failed write in between) is published as it is,
// but only if it is newer than what is published: an older state's pending
// envelope (a restored backup, after a raise) never replaces a newer
// manifest. A published manifest with the persisted envelope's serial but
// other bytes fails closed. A published manifest newer than the state knows
// becomes the current one, so renewal and the not-newer rule work from what
// Karta can actually see.
func (s *Signer) reconcilePublished(raw []byte, pub *online.Manifest) error {
	c := s.state.Current
	switch {
	case pub == nil || (c != nil && c.Serial > pub.Serial):
		if c != nil && s.now().Before(c.ExpiresAt) && (!c.Promoted || string(raw) != c.Envelope) {
			if err := s.promote(); err != nil {
				return err
			}
			s.log.Info("published the current manifest, which was not published", "serial", c.Serial)
		}
	case c != nil && c.Serial == pub.Serial:
		if string(raw) != c.Envelope {
			return fmt.Errorf("%w (%s): the published manifest has serial %d, the serial of the envelope this signer persisted, but other "+
				"bytes (published SHA-256 %s, persisted %s): a serial is never bound to two envelopes; find out how it was written",
				ErrFailClosed, CodeEnvelopeConflict, pub.Serial, safefile.SHA256Hex(raw), c.EnvelopeSHA256)
		}
		c.Promoted = true
	default:
		prev := int64(0)
		if c != nil {
			prev = c.Serial
		}
		var ids []string
		var env online.Envelope
		if json.Unmarshal(raw, &env) == nil {
			own := s.PublicKeys()
			for _, sig := range env.Signatures {
				if _, ok := own[sig.KeyID]; ok {
					ids = append(ids, sig.KeyID)
				}
			}
		}
		s.state.Current = &Signed{Serial: pub.Serial, SHA256: pub.Snapshot.SHA256, SizeBytes: pub.Snapshot.SizeBytes,
			DataTimestamp: pub.Snapshot.DataTimestamp, IssuedAt: pub.IssuedAt, ExpiresAt: pub.ExpiresAt, EnvelopeSHA256: safefile.SHA256Hex(raw),
			KeyIDs: ids, Envelope: string(raw), Promoted: true, Reason: "adopted: the published manifest (verified) is newer than the state"}
		found := false
		for i := range s.state.Assets {
			if s.state.Assets[i].SHA256 == pub.Snapshot.SHA256 {
				found = true
				if pub.ExpiresAt.After(s.state.Assets[i].ExpiresAt) {
					s.state.Assets[i].ExpiresAt = pub.ExpiresAt
				}
			}
		}
		if !found {
			s.state.Assets = append(s.state.Assets, AssetRef{SHA256: pub.Snapshot.SHA256, ExpiresAt: pub.ExpiresAt})
		}
		s.log.Warn("adopted the verified published manifest as the current one (the state knew only an older one)", "published_serial",
			pub.Serial, "state_serial", prev, "high_water", s.state.HighWater)
	}
	return nil
}

// publishPending publishes, during a run, a persisted envelope whose
// publication failed (for example for space): it is retried at every run,
// not only at the next start.
func (s *Signer) publishPending() error {
	c := s.state.Current
	if c == nil || c.Promoted || !s.now().Before(c.ExpiresAt) {
		return nil
	}
	raw, pub, err := s.readPublished()
	if err != nil {
		return err
	}
	if pub != nil && c.Serial <= pub.Serial {
		return s.reconcilePublished(raw, pub)
	}
	if err := s.promote(); err != nil {
		return err
	}
	s.log.Info("published a manifest whose publication had failed", "serial", c.Serial)
	return nil
}

// State returns a copy of the signer state.
func (s *Signer) State() SignerState { return s.state }

func (s *Signer) clockCheck() error {
	if c := s.state.Current; c != nil && s.now().Add(s.cfg.MaxFutureSkew).Before(c.IssuedAt) {
		return fmt.Errorf("%w (%s): the clock (%s) is earlier than the last manifest's issued_at (%s); fix the clock",
			ErrFailClosed, CodeClockBehind, s.now().UTC().Format(time.RFC3339), c.IssuedAt.Format(time.RFC3339))
	}
	return nil
}

func (s *Signer) save() error {
	s.state.UpdatedAt = s.now().UTC()
	if len(s.state.History) > 50 {
		s.state.History = s.state.History[len(s.state.History)-50:]
	}
	if len(s.state.Processed) > 50 {
		s.state.Processed = s.state.Processed[len(s.state.Processed)-50:]
	}
	if err := writeJSONState(filepath.Join(s.cfg.StateDir, stateFileName), s.state); err != nil {
		return err
	}
	// A status copy for serve's metrics, without the envelopes.
	status := s.state
	if status.Current != nil {
		c := *status.Current
		c.Envelope = ""
		status.Current = &c
	}
	return writeJSONState(filepath.Join(s.cfg.Publish, StatusDirName, "signer.json"), status)
}

func (s *Signer) setError(code string, err error) {
	s.state.LastError = &StateError{At: s.now().UTC(), Code: code, Message: trunc(err.Error(), 500)}
}

// Run signs until ctx ends.
func (s *Signer) Run(ctx context.Context) {
	for {
		s.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.Poll):
		}
	}
}

// RunOnce processes new acquisitions, renews the current manifest when
// due, and removes assets no unexpired manifest names.
func (s *Signer) RunOnce(ctx context.Context) {
	defer func() {
		if err := s.save(); err != nil {
			s.log.Error("could not save the signer state", "err", err)
		}
	}()
	if err := s.clockCheck(); err != nil {
		s.setError(CodeClockBehind, err)
		s.log.Error("not signing", "err", err)
		return
	}
	failed := false
	fail := func(code string, err error, msg string) {
		failed = true
		s.setError(code, err)
		s.log.Error(msg, "err", err)
	}
	if err := s.publishPending(); err != nil {
		if errors.Is(err, ErrFailClosed) {
			// Something else wrote the publish volume: sign nothing.
			s.setError(failClosedCode(err), err)
			s.log.Error("not signing", "err", err)
			return
		}
		fail(codeOr(online.CodeOf(err), "publish"), err, "could not publish the current manifest; retrying")
	}
	entries, err := inbox.Scan(s.cfg.Spool, 1000)
	if err != nil {
		fail("spool", err, "could not read the spool")
		entries = nil
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Marker == nil || entries[j].Marker == nil {
			return entries[j].Marker != nil
		}
		return entries[i].Marker.ModTime.Before(entries[j].Marker.ModTime)
	})
	for _, e := range entries {
		if ctx.Err() != nil || !e.Complete() || s.processed(e.Name) {
			continue
		}
		// A failing acquisition (for example no space to stage it) never
		// stops the renewal of the current manifest below.
		if err := s.consider(ctx, e); err != nil {
			fail(codeOr(online.CodeOf(err), "sign"), err, "could not process an acquisition; retrying ("+e.Name+")")
		}
	}
	if err := s.renew(); err != nil {
		fail(codeOr(online.CodeOf(err), "renew"), err, "could not renew the manifest")
	}
	s.pruneAssets()
	if !failed {
		s.state.LastError = nil
	}
}

// failClosedCode names a fail-closed refusal for the state and metrics.
func failClosedCode(err error) string {
	for _, c := range []string{CodePublishedUntrusted, CodeEnvelopeConflict, CodeStateBehind, CodeClockBehind} {
		if strings.Contains(err.Error(), "("+c+")") {
			return c
		}
	}
	return "fail_closed"
}

func codeOr(code, def string) string {
	if code == "" {
		return def
	}
	return code
}

func (s *Signer) processed(name string) bool {
	for _, p := range s.state.Processed {
		if p == name {
			return true
		}
	}
	return false
}

func (s *Signer) hold(sha string, ts time.Time, code, reason string) {
	s.state.Held = &Held{SHA256: sha, DataTimestamp: ts.UTC(), Code: code, Reason: trunc(reason, 500), At: s.now().UTC()}
	s.state.Processed = append(s.state.Processed, sha)
	s.log.Warn("acquisition held: not signed", "sha256", sha, "code", code, "reason", reason)
}

// consider re-reads one acquisition itself (a private copy with the digest
// checked against the spool marker), runs Karta's input checks on it, and
// signs it when it is newer than the current manifest's snapshot.
func (s *Signer) consider(ctx context.Context, e inbox.Entry) error {
	if !sha256Name(e.Name) {
		s.hold(e.Name, time.Time{}, "invalid_name", "a spool entry not named by its SHA-256")
		return nil
	}
	staged, err := inbox.StageContext(ctx, s.cfg.Spool, e, filepath.Join(s.cfg.Publish, stagingDirName), "sign-"+e.Name[:16],
		inbox.Options{MaxSnapshotBytes: s.cfg.MaxInputBytes})
	if err != nil {
		if code := inbox.CodeOf(err); code != "" && code != inbox.CodeIO && code != inbox.CodeInsufficientSpace {
			s.hold(e.Name, time.Time{}, code, err.Error())
			return nil
		}
		return err
	}
	defer os.RemoveAll(staged.Dir)
	if staged.SHA256 != e.Name {
		s.hold(e.Name, time.Time{}, "digest_mismatch", "the spooled file is not the bytes its name says")
		return nil
	}
	accBytes, err := safefile.ReadRegular(filepath.Join(s.cfg.Spool, e.Name+AcquisitionSuffix), 64<<10)
	var acq Acquisition
	if err == nil {
		err = safefile.StrictDecode(accBytes, &acq)
	}
	if err != nil || acq.SHA256 != e.Name {
		s.hold(e.Name, time.Time{}, CodeNoAcquisition, fmt.Sprintf("the acquisition record is missing or does not describe these bytes: %v", err))
		return nil
	}
	cfg, err := region.Load(s.cfg.RegionPath)
	if err != nil {
		return codedErr(CodeSourceConfig, "region file: %v", err)
	}
	// The first check runs before the bridge has written its sidecar, so a
	// region that requires provenance is checked without that rule first,
	// and with it (and the sidecar) before anything is signed.
	bare := cfg
	bare.Source.RequireProvenance = false
	verify := func(prov string) (*importer.Verified, error) {
		r := cfg
		if prov == "" {
			r = bare
		}
		return importer.Verify(ctx, importer.VerifyOptions{SnapshotPath: staged.SnapshotPath, ProvenancePath: prov, Region: r,
			MaxInputBytes: s.cfg.MaxInputBytes, MaxFutureSkew: s.cfg.MaxFutureSkew, Now: s.now,
			Authorize: func(context.Context, string, string, int64) (string, error) {
				return "bridge policy (" + s.conf.BridgeID + ")", nil
			}})
	}
	v, err := verify("")
	if err != nil {
		code := importer.InputCode(err)
		if code == "" {
			return err
		}
		s.hold(e.Name, time.Time{}, code, "Karta's input checks refuse it: "+err.Error())
		return nil
	}
	ts := v.Source.DataTimestamp
	if c := s.state.Current; c != nil {
		switch {
		case v.Info.SHA256 == c.SHA256:
			s.state.Processed = append(s.state.Processed, e.Name)
			return nil
		case !ts.After(c.DataTimestamp):
			s.hold(e.Name, ts, CodeNotNewer, fmt.Sprintf("different bytes with data timestamp %s, not newer than the signed %s (%s): Karta's forward "+
				"rule would refuse it; held for inspection", ts.UTC().Format(time.RFC3339), c.DataTimestamp.UTC().Format(time.RFC3339), c.SHA256))
			return nil
		}
	}
	if v.Info.BBox == nil || v.Info.Timestamp == nil {
		s.hold(e.Name, ts, CodeNoHeaderBox, "the PBF header lacks a box or a replication timestamp; the bridge does not invent provenance for it")
		return nil
	}
	side, err := s.sidecar(v, acq)
	if err != nil {
		return err
	}
	sidePath := staged.SnapshotPath + ".provenance.json"
	if err := os.WriteFile(sidePath, side, 0o600); err != nil {
		return storageErr(err)
	}
	if v2, err := verify(sidePath); err != nil || !v2.Source.DataTimestamp.Equal(ts) {
		return fmt.Errorf("the generated provenance sidecar does not pass Karta's checks: %v", err)
	}
	// The asset first, under its digest, then the signature.
	asset := filepath.Join(s.cfg.Publish, SnapshotsDir, e.Name+inbox.SnapshotSuffix)
	if err := os.Chmod(staged.SnapshotPath, 0o644); err != nil { // #nosec G302 -- served publicly
		return storageErr(err)
	}
	if err := safefile.WriteAtomic(asset+".provenance.json", side, 0o644); err != nil {
		return storageErr(err)
	}
	if err := os.Rename(staged.SnapshotPath, asset); err != nil {
		return storageErr(err)
	}
	_ = safefile.SyncDir(filepath.Join(s.cfg.Publish, SnapshotsDir))
	failpoint.Hit("bridge.sign_after_asset")
	if err := s.sign(e.Name, v.Info.Size, ts, safefile.SHA256Hex(side), int64(len(side)), "new snapshot"); err != nil {
		return err
	}
	s.state.Processed = append(s.state.Processed, e.Name)
	s.state.Held = nil
	return nil
}

func sha256Name(n string) bool {
	if len(n) != 64 {
		return false
	}
	for _, c := range n {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// sidecar is the provenance the bridge records: Karta's sidecar fields
// (which its checks verify against the bytes) and a karta_bridge object with
// what the bridge observed. It claims nothing the bridge did not observe.
func (s *Signer) sidecar(v *importer.Verified, acq Acquisition) ([]byte, error) {
	box := *v.Info.BBox
	parts := make([]string, 4)
	for i, x := range box {
		parts[i] = strconv.FormatFloat(x, 'f', -1, 64)
	}
	ts := v.Info.Timestamp.UTC().Format(time.RFC3339)
	fileinfo := map[string]any{
		"file":   map[string]any{"size": v.Info.Size},
		"header": map[string]any{"boxes": [][]float64{box[:]}, "option": map[string]string{"osmosis_replication_timestamp": ts}},
	}
	ids := make([]string, 0, len(s.keys))
	for _, k := range s.keys {
		ids = append(ids, k.KeyID)
	}
	doc := map[string]any{
		"source":          acq.Source,
		"source_sha256":   v.Info.SHA256,
		"output_sha256":   v.Info.SHA256,
		"bbox_wgs84":      strings.Join(parts, ","),
		"source_fileinfo": fileinfo,
		"output_fileinfo": fileinfo,
		"license":         "ODbL 1.0",
		"attribution":     "© OpenStreetMap contributors",
		"license_url":     importer.LicenseURL,
		"karta_bridge": map[string]any{
			"format":          ProvenanceFormat,
			"bridge_id":       s.conf.BridgeID,
			"statement":       "the bridge downloaded these exact bytes from the source URL and they passed its checks; this is not the distributor's signature",
			"source_url":      acq.Source,
			"observed_at":     acq.FinishedAt,
			"download":        map[string]any{"started_at": acq.StartedAt, "finished_at": acq.FinishedAt, "user_agent": acq.UserAgent},
			"http":            acq.Hint,
			"distributor_md5": map[string]any{"value": acq.MD5, "matched": acq.MD5Matched},
			"signing_key_ids": ids,
			"versions":        map[string]string{"acquire": acq.Version, "sign": s.cfg.Version},
		},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	if _, err := provenance.Parse(b); err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// sign allocates the next serial, persists the exact envelope with it, and
// only then publishes the manifest.
func (s *Signer) sign(sha string, size int64, ts time.Time, provSHA string, provSize int64, reason string) error {
	serial := s.state.HighWater + 1
	if serial > MaxSerial {
		return codedErr(CodeSerialExhausted, "serial %d is beyond %d, where the fetcher's delivery names stop sorting by serial", serial, int64(MaxSerial))
	}
	cfg, err := region.Load(s.cfg.RegionPath)
	if err != nil {
		return codedErr(CodeSourceConfig, "region file: %v", err)
	}
	now := s.now().UTC().Truncate(time.Second)
	m := online.Manifest{Format: online.ManifestFormat, RegionID: cfg.ID, BBox: online.BBox(cfg.BBox), Serial: serial,
		IssuedAt: now, ExpiresAt: now.Add(s.conf.ManifestValidity.D()),
		Snapshot:   online.Snapshot{URL: SnapshotsDir + "/" + sha + inbox.SnapshotSuffix, SHA256: sha, SizeBytes: size, DataTimestamp: ts.UTC()},
		Provenance: &online.File{URL: SnapshotsDir + "/" + sha + inbox.SnapshotSuffix + ".provenance.json", SHA256: provSHA, SizeBytes: provSize}}
	env, err := online.Sign(m, s.keys...)
	if err != nil {
		return err
	}
	env = append(env, '\n')
	ids := make([]string, 0, len(s.keys))
	for _, k := range s.keys {
		ids = append(ids, k.KeyID)
	}
	s.state.HighWater = serial
	s.state.Current = &Signed{Serial: serial, SHA256: sha, SizeBytes: size, DataTimestamp: ts.UTC(), IssuedAt: m.IssuedAt, ExpiresAt: m.ExpiresAt,
		EnvelopeSHA256: safefile.SHA256Hex(env), KeyIDs: ids, Envelope: string(env), Reason: reason}
	found := false
	for i := range s.state.Assets {
		if s.state.Assets[i].SHA256 == sha {
			s.state.Assets[i].ExpiresAt, found = m.ExpiresAt, true
		}
	}
	if !found {
		s.state.Assets = append(s.state.Assets, AssetRef{SHA256: sha, ExpiresAt: m.ExpiresAt})
	}
	// The serial and the exact envelope are on disk before anyone can see
	// the manifest: a crash never binds the serial to other bytes.
	if err := s.save(); err != nil {
		return err
	}
	failpoint.Hit("bridge.sign_after_state")
	if err := s.promote(); err != nil {
		return err
	}
	failpoint.Hit("bridge.sign_after_manifest")
	s.log.Info("manifest signed and published", "serial", serial, "sha256", sha, "size_bytes", size,
		"data_timestamp", ts.UTC().Format(time.RFC3339), "expires_at", m.ExpiresAt.Format(time.RFC3339), "key_ids", ids, "reason", reason)
	return nil
}

func (s *Signer) promote() error {
	c := s.state.Current
	if err := safefile.WriteAtomic(filepath.Join(s.cfg.Publish, ManifestFile), []byte(c.Envelope), 0o644); err != nil {
		return storageErr(err)
	}
	c.Promoted = true
	h := *c
	h.Envelope = ""
	s.state.History = append(s.state.History, h)
	return s.save()
}

// renew re-signs the held snapshot under a new serial when less than
// renew_before of validity remains (also while the distributor is
// unreachable). It re-reads the asset's bytes first. Renewal never changes
// the data timestamp, so old data still looks old.
func (s *Signer) renew() error {
	c := s.state.Current
	if c == nil || c.ExpiresAt.Sub(s.now()) > s.conf.RenewBefore.D() {
		return nil
	}
	asset := filepath.Join(s.cfg.Publish, SnapshotsDir, c.SHA256+inbox.SnapshotSuffix)
	info, err := osmfile.Inspect(asset, s.cfg.MaxInputBytes)
	if err != nil || info.SHA256 != c.SHA256 {
		return codedErr(CodeAssetMissing, "the published asset %s is missing or no longer has its digest (%v); not renewing", c.SHA256, err)
	}
	side, err := safefile.ReadRegular(asset+".provenance.json", 1<<20)
	if err != nil {
		return codedErr(CodeAssetMissing, "the published provenance of %s is missing: %v", c.SHA256, err)
	}
	return s.sign(c.SHA256, c.SizeBytes, c.DataTimestamp, safefile.SHA256Hex(side), int64(len(side)), "renewal")
}

// pruneAssets removes assets no unexpired manifest names (never the
// current one).
func (s *Signer) pruneAssets() {
	now := s.now()
	keep := s.state.Assets[:0]
	for _, a := range s.state.Assets {
		if (s.state.Current != nil && a.SHA256 == s.state.Current.SHA256) || now.Before(a.ExpiresAt) {
			keep = append(keep, a)
			continue
		}
		p := filepath.Join(s.cfg.Publish, SnapshotsDir, a.SHA256+inbox.SnapshotSuffix)
		_ = os.Remove(p)
		_ = os.Remove(p + ".provenance.json")
		s.log.Info("removed an asset no unexpired manifest names", "sha256", a.SHA256)
	}
	s.state.Assets = keep
}

// PublicKeys returns the signer's public keys (for the fetcher's source file).
func (s *Signer) PublicKeys() map[string]ed25519.PublicKey {
	out := map[string]ed25519.PublicKey{}
	for _, k := range s.keys {
		out[k.KeyID] = k.Key.Public().(ed25519.PublicKey)
	}
	return out
}
