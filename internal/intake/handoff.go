package intake

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/failpoint"
	"github.com/pouriya-sedaghat/karta/internal/inbox"
	"github.com/pouriya-sedaghat/karta/internal/osmfile"
	"github.com/pouriya-sedaghat/karta/internal/region"
	"github.com/pouriya-sedaghat/karta/internal/safefile"
)

// Channel letters in handoff names: the watcher or the command.
const (
	LetterWatch  = "w"
	LetterSubmit = "c"
)

// ErrQueueFull means the credential holds its maximum of open
// authorizations: the delivery waits for a free slot.
var ErrQueueFull = errors.New("the intake credential holds its maximum of open authorizations; the delivery waits")

// HandoffConfig configures the handoff shared by the watcher and the command.
type HandoffConfig struct {
	// Dir is the intake's handoff directory (the publisher's KARTA_INTAKE_DIR).
	Dir        string
	RegionPath string
	Client     *Client
	// Letter is LetterWatch or LetterSubmit.
	Letter string
	// TTL is the validity asked for (0, or above the publisher's cap: the
	// cap).
	TTL           time.Duration
	MaxInputBytes int64
	// ReserveBytes must stay free in the handoff directory after a copy.
	ReserveBytes int64
	Log          *slog.Logger
	Now          func() time.Time
}

// Handoffer hands deliveries to the publisher.
type Handoffer struct {
	cfg HandoffConfig
	now func() time.Time
}

// NewHandoffer prepares the handoff directory's hidden intake directory.
func NewHandoffer(cfg HandoffConfig) (*Handoffer, error) {
	if cfg.Letter != LetterWatch && cfg.Letter != LetterSubmit {
		return nil, fmt.Errorf("handoff letter %q", cfg.Letter)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	d := filepath.Join(cfg.Dir, StateDirName)
	// #nosec G301 -- the publisher (another UID) reads the watcher state
	if err := os.Mkdir(d, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if fi, err := os.Lstat(d); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", d)
	}
	return &Handoffer{cfg: cfg, now: now}, nil
}

// lock serializes handoffs and reconciliation between the watcher and
// command runs sharing the handoff directory.
func (h *Handoffer) lock(ctx context.Context) (func(), error) {
	path := filepath.Join(h.cfg.Dir, StateDirName, LockFileName)
	for {
		f, err := safefile.Lock(path)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// delivery is one delivery to hand off.
type delivery struct {
	// path is the snapshot to copy; want its listed state (watcher), or nil
	// (command: the file as it is when opened).
	path        string
	want        *fileState
	sidecarPath string
	sidecarWant *fileState
	// expectSHA256 and expectSize come from the producer (completion
	// marker) or the operator (command); empty and 0 only for an explicit
	// operator attestation.
	expectSHA256 string
	expectSize   int64
	reason       string
	// beforeAuthorize runs once the handoff name and digest are known,
	// before the digest is authorized (the watcher records its pending
	// handoff there, for crash recovery).
	beforeAuthorize func(name, digest string, size int64) error
}

// handedOff is a delivery the publisher now has.
type handedOff struct {
	Name            string
	SHA256          string
	Size            int64
	AuthorizationID int64
	Channel         string
	At              time.Time
}

func (h *Handoffer) paths(name string) (vis, marker, side, hidden, hiddenSide string) {
	d := h.cfg.Dir
	return filepath.Join(d, name+inbox.SnapshotSuffix), filepath.Join(d, name+inbox.MarkerSuffix), filepath.Join(d, name+inbox.SidecarSuffix),
		filepath.Join(d, "."+name+".hashed"+inbox.SnapshotSuffix), filepath.Join(d, "."+name+".hashed.provenance.json")
}

// deliver hands one delivery to the publisher: copy and hash into a hidden
// file, refuse a mismatch with the expectation, check the PBF header
// against the region (advisory), authorize the digest, rename into place and
// write the ready marker last.
func (h *Handoffer) deliver(ctx context.Context, d delivery) (*handedOff, error) {
	unlock, err := h.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	cfg, err := region.Load(h.cfg.RegionPath)
	if err != nil {
		return nil, fmt.Errorf("region file: %w", err)
	}
	list, err := h.cfg.Client.List(ctx)
	if err != nil {
		return nil, err
	}
	if list.Limits.RegionID != cfg.ID {
		return nil, fmt.Errorf("the publisher serves region %q, the region file is %q", list.Limits.RegionID, cfg.ID)
	}
	open := 0
	for _, r := range list.Records {
		if r.Authorization.Open(h.now()) {
			open++
		}
	}
	if open >= list.Limits.MaxOpen {
		return nil, ErrQueueFull
	}
	maxInput := min(h.cfg.MaxInputBytes, list.Limits.MaxSnapshotBytes)

	src, before, err := h.openSource(d)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	size := before.Size()
	switch {
	case size == 0:
		return nil, refuse(CodeEmpty, "%s is empty", filepath.Base(d.path))
	case size > maxInput:
		return nil, refuse(CodeTooLarge, "%s is %d bytes, above the limit of %d", filepath.Base(d.path), size, maxInput)
	case d.expectSize != 0 && size != d.expectSize:
		return nil, refuse(CodeSizeMismatch, "%s is %d bytes, the expected size is %d: an incomplete or different copy", filepath.Base(d.path), size, d.expectSize)
	}
	need := size + h.cfg.ReserveBytes
	if d.sidecarWant != nil {
		need += d.sidecarWant.Size
	}
	if free, err := inbox.FreeBytes(h.cfg.Dir); err == nil && free < need {
		return nil, refuse(CodeStorage, "the handoff directory has %d bytes free; the delivery needs %d plus a reserve of %d", free, size, h.cfg.ReserveBytes)
	}

	tmp := filepath.Join(h.cfg.Dir, ".part-"+h.cfg.Letter+"-"+randomHex(8)+inbox.SnapshotSuffix)
	digest, err := copyHashed(ctx, src, tmp, size)
	if err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := h.unchanged(d, src, before); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if d.expectSHA256 != "" && digest != d.expectSHA256 {
		_ = os.Remove(tmp)
		return nil, refuse(CodeDigestMismatch, "%s has SHA-256 %s, the expected digest is %s: an incomplete, corrupted or different copy",
			filepath.Base(d.path), digest, d.expectSHA256)
	}
	name := h.newName(cfg.ID, digest)
	_, _, _, hidden, hiddenSide := h.paths(name)
	discard := func() {
		for _, p := range []string{tmp, hidden, hiddenSide} {
			_ = os.Remove(p)
		}
	}
	if err := os.Rename(tmp, hidden); err != nil {
		discard()
		return nil, err
	}
	_ = safefile.SyncDir(h.cfg.Dir)
	failpoint.Hit("intake.after_hash")
	if err := precheck(hidden, cfg, d.sidecarWant != nil); err != nil {
		discard()
		return nil, err
	}
	if d.sidecarWant != nil {
		b, err := readSmall(d.sidecarPath, d.sidecarWant, MaxSidecarBytes)
		if err != nil {
			discard()
			return nil, err
		}
		if err := safefile.WriteAtomic(hiddenSide, b, 0o644); err != nil {
			discard()
			return nil, storage(err)
		}
	}
	if d.beforeAuthorize != nil {
		if err := d.beforeAuthorize(name, digest, size); err != nil {
			discard()
			return nil, err
		}
	}
	ttl := h.cfg.TTL
	if capTTL := time.Duration(list.Limits.MaxTTLSeconds * float64(time.Second)); ttl <= 0 || ttl > capTTL {
		ttl = capTTL
	}
	a, _, err := h.cfg.Client.Authorize(ctx, AuthorizeRequest{SHA256: digest, SizeBytes: size, RegionID: cfg.ID,
		TTLSeconds: int64(ttl / time.Second), Name: name, Reason: truncateRunes(d.reason, 500)})
	if err != nil {
		discard()
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == "intake_limit_reached" {
			return nil, ErrQueueFull
		}
		return nil, err
	}
	failpoint.Hit("intake.after_authorize")
	if err := h.complete(name, digest); err != nil {
		return nil, err
	}
	return &handedOff{Name: name, SHA256: digest, Size: size, AuthorizationID: a.ID, Channel: a.Channel, At: h.now().UTC()}, nil
}

// complete renames the hidden copy (and sidecar) into place and writes the
// ready marker last.
func (h *Handoffer) complete(name, digest string) error {
	vis, marker, side, hidden, hiddenSide := h.paths(name)
	if _, err := os.Lstat(hiddenSide); err == nil {
		if err := os.Rename(hiddenSide, side); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(hidden); err == nil {
		if err := os.Rename(hidden, vis); err != nil {
			return err
		}
	}
	_ = safefile.SyncDir(h.cfg.Dir)
	failpoint.Hit("intake.before_marker")
	if err := safefile.WriteAtomic(marker, []byte(digest+"  "+name+inbox.SnapshotSuffix+"\n"), 0o644); err != nil {
		return storage(err)
	}
	failpoint.Hit("intake.after_marker")
	return nil
}

func (h *Handoffer) openSource(d delivery) (*os.File, os.FileInfo, error) {
	if d.want != nil {
		f, err := openListed(d.path, d.want)
		if err != nil {
			return nil, nil, err
		}
		fi, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, nil, err
		}
		return f, fi, nil
	}
	fi, err := os.Lstat(d.path)
	switch {
	case err != nil:
		return nil, nil, refuse(CodeChanged, "%v", err)
	case fi.Mode()&os.ModeSymlink != 0:
		return nil, nil, refuse(CodeSymlink, "%s is a symbolic link", d.path)
	case !fi.Mode().IsRegular():
		return nil, nil, refuse(CodeNotRegular, "%s is not a regular file", d.path)
	}
	f, err := safefile.OpenNoFollow(d.path)
	if err != nil {
		return nil, nil, refuse(CodeChanged, "%v", err)
	}
	ofi, err := f.Stat()
	if err != nil || !ofi.Mode().IsRegular() || ofi.Size() != fi.Size() || !ofi.ModTime().Equal(fi.ModTime()) {
		_ = f.Close()
		return nil, nil, refuse(CodeChanged, "%s changed while it was opened", d.path)
	}
	return f, ofi, nil
}

// unchanged checks the source kept its listed identity during the copy. A
// command's input may be on an untrusted share whose inode numbers are not
// reliable; its integrity rests on the expected digest, so only size and
// modification time are compared there.
func (h *Handoffer) unchanged(d delivery, src *os.File, before os.FileInfo) error {
	after, err := src.Stat()
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return refuse(CodeChanged, "%s changed while it was copied", filepath.Base(d.path))
	}
	if d.want != nil {
		if stateOf(after).key() != d.want.key() {
			return refuse(CodeChanged, "%s changed while it was copied", filepath.Base(d.path))
		}
		fi, err := os.Lstat(d.path)
		if err != nil || stateOf(fi).key() != d.want.key() {
			return refuse(CodeChanged, "%s was replaced while it was copied", filepath.Base(d.path))
		}
	}
	return nil
}

func (h *Handoffer) newName(regionID, digest string) string {
	base := fmt.Sprintf("%s-%s%s-%s", regionID, h.cfg.Letter, h.now().UTC().Format("20060102T150405Z"), digest[:12])
	name := base
	for i := 1; i < 100; i++ {
		vis, marker, _, hidden, _ := h.paths(name)
		if !exists(vis) && !exists(marker) && !exists(hidden) {
			break
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

// precheck is the advisory check before authorizing: the PBF header must
// parse (no history file), its box must be the region's, and without a
// sidecar it must carry a box and a timestamp. The publisher still verifies
// everything on its staged copy.
func precheck(path string, cfg region.Config, hasSidecar bool) error {
	info, err := osmfile.Header(path)
	switch {
	case err != nil:
		return refuse(CodeMalformed, "the PBF header does not parse: %v", err)
	case info.BBox != nil && !region.SameBBox(*info.BBox, cfg.BBox):
		return refuse(CodeRegionMismatch, "the header box %v is not region %q's box %v", *info.BBox, cfg.ID, cfg.BBox)
	case info.BBox == nil && !hasSidecar:
		return refuse(CodeRegionMismatch, "the snapshot has no header box and no provenance sidecar, so it cannot be matched to region %q", cfg.ID)
	case info.Timestamp == nil && !hasSidecar:
		return refuse(CodeTimestampMissing, "the snapshot header has no data timestamp and there is no provenance sidecar")
	}
	return nil
}

// copyHashed copies size bytes of src into a new file at dst (0644 at the
// end) while hashing, stopping when ctx ends.
func copyHashed(ctx context.Context, src io.Reader, dst string, size int64) (string, error) {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600) // #nosec G304 -- our handoff directory
	if err != nil {
		return "", storage(err)
	}
	h := sha256.New()
	n, err := copyCtx(ctx, io.MultiWriter(out, h), io.LimitReader(src, size+1))
	if err == nil && n != size {
		err = refuse(CodeChanged, "%d bytes were copied, the file was %d bytes", n, size)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(dst, 0o644) // #nosec G302 -- the publisher (another user) reads handoffs
	}
	if err != nil {
		if RefusalCode(err) != "" || ctx.Err() != nil {
			return "", err
		}
		return "", storage(err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyCtx(ctx context.Context, w io.Writer, r io.Reader) (int64, error) {
	buf := make([]byte, 1<<20)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		k, rerr := r.Read(buf)
		if k > 0 {
			if _, err := w.Write(buf[:k]); err != nil {
				return n, err
			}
			n += int64(k)
		}
		if rerr == io.EOF {
			return n, nil
		}
		if rerr != nil {
			return n, rerr
		}
	}
}

// fileDigest hashes a regular file of ours (never through a symlink).
func fileDigest(path string) (string, int64, error) {
	f, err := safefile.OpenNoFollow(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	var h hash.Hash = sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func storage(err error) error {
	if safefile.IsNoSpace(err) {
		return refuse(CodeStorage, "the handoff directory is full: %v", err)
	}
	return err
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Reconciled is the result of settling the caller's own handoffs.
type Reconciled struct {
	Limits  Limits
	Records []Record
	// Finished are the handoffs whose submission became final (cleaned up
	// and their authorizations closed) in this pass.
	Finished []Record
}

// reconcile settles the caller's own handoffs, under the handoff lock:
//
//   - a handoff whose submission is final: files removed (marker first),
//     authorization closed;
//   - a handoff interrupted between authorization and marker: completed if
//     resume allows it and the copy still has the authorized digest and
//     size, otherwise removed and its authorization closed;
//   - an open authorization without any handoff file: closed (orphan);
//   - temporary copies left by a crash: removed; hidden copies no
//     authorization of the caller names are removed once older than the
//     publisher's validity cap (every authorization naming them has
//     expired by then).
func (h *Handoffer) reconcile(ctx context.Context, resume func(Record) bool) (Reconciled, error) {
	unlock, err := h.lock(ctx)
	if err != nil {
		return Reconciled{}, err
	}
	defer unlock()
	list, err := h.cfg.Client.List(ctx)
	if err != nil {
		return Reconciled{}, err
	}
	res := Reconciled{Limits: list.Limits, Records: list.Records}
	now := h.now()
	mine := map[string]bool{}
	for _, rec := range list.Records {
		a := rec.Authorization
		if a.IntakeName == nil || !NamePattern.MatchString(*a.IntakeName) {
			continue
		}
		name := *a.IntakeName
		mine[name] = true
		vis, marker, side, hidden, hiddenSide := h.paths(name)
		closeIt := func(why string) {
			if !a.Open(now) && a.RevokedAt != nil {
				return
			}
			if _, err := h.cfg.Client.Close(ctx, a.ID, why); err != nil {
				h.cfg.Log.Warn("could not close an intake authorization; it expires by itself", "authorization_id", a.ID, "err", err)
			}
		}
		removeAll := func() {
			for _, p := range []string{marker, vis, side, hidden, hiddenSide} {
				_ = os.Remove(p)
			}
		}
		switch {
		case exists(marker):
			if rec.Submission.Final() {
				removeAll()
				code := ""
				if rec.Submission.ReasonCode != nil {
					code = " (" + *rec.Submission.ReasonCode + ")"
				}
				closeIt("the submission is final: " + rec.Submission.State + code)
				res.Finished = append(res.Finished, rec)
			}
		case exists(hidden) || exists(vis):
			p := hidden
			if !exists(hidden) {
				p = vis
			}
			sum, n, err := fileDigest(p)
			ok := err == nil && sum == a.SHA256 && a.SizeBytes != nil && n == *a.SizeBytes && a.Open(now) && resume != nil && resume(rec)
			if ok {
				if err := h.complete(name, sum); err != nil {
					return res, err
				}
				h.cfg.Log.Info("completed a handoff interrupted after its authorization", "name", name, "authorization_id", a.ID)
				continue
			}
			removeAll()
			closeIt("the handoff was interrupted before its marker and is not resumed")
			h.cfg.Log.Warn("discarded a handoff interrupted after its authorization", "name", name, "authorization_id", a.ID)
		default:
			if a.Open(now) {
				closeIt("orphan: no handoff files (interrupted before the handoff)")
			}
		}
	}
	h.sweep(mine, time.Duration(list.Limits.MaxTTLSeconds*float64(time.Second)))
	return res, nil
}

// sweep removes crash leftovers: temporary copies (nobody copies while the
// lock is held) and hidden copies older than the validity cap that no
// authorization of the caller names.
func (h *Handoffer) sweep(mine map[string]bool, capTTL time.Duration) {
	entries, err := os.ReadDir(h.cfg.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		n := e.Name()
		p := filepath.Join(h.cfg.Dir, n)
		switch {
		case strings.HasPrefix(n, ".part-"):
			_ = os.Remove(p)
		case strings.HasPrefix(n, ".") && strings.Contains(n, ".hashed"):
			base := strings.TrimPrefix(n, ".")
			base = base[:strings.Index(base, ".hashed")]
			if mine[base] {
				continue
			}
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > capTTL {
				_ = os.Remove(p)
			}
		}
	}
}

// waitOutcome follows a handoff until its submission is final, then cleans
// it up (as reconcile does) and returns the record.
func (h *Handoffer) waitOutcome(ctx context.Context, name string, every time.Duration, progress func(Record)) (Record, error) {
	t := time.NewTicker(every)
	defer t.Stop()
	last := ""
	for {
		list, err := h.cfg.Client.List(ctx)
		if err == nil {
			for _, rec := range list.Records {
				if rec.Authorization.IntakeName == nil || *rec.Authorization.IntakeName != name {
					continue
				}
				st := ""
				if rec.Submission != nil {
					st = rec.Submission.State
				}
				if st != last && progress != nil {
					progress(rec)
					last = st
				}
				if rec.Submission.Final() {
					if _, err := h.reconcile(ctx, nil); err != nil {
						h.cfg.Log.Warn("cleanup after the outcome failed; the next run retries it", "err", err)
					}
					return rec, nil
				}
				if !rec.Authorization.Open(h.now()) && rec.Submission == nil {
					return rec, fmt.Errorf("the authorization of %s ended before the publisher took the delivery", name)
				}
			}
		} else {
			h.cfg.Log.Warn("cannot read the intake records; retrying", "err", err)
		}
		select {
		case <-ctx.Done():
			return Record{}, ctx.Err()
		case <-t.C:
		}
	}
}
