package bridge

import (
	"context"
	"crypto/md5" // #nosec G501 -- the distributor's checksum format; detects transfer accidents, authorizes nothing
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/failpoint"
	"github.com/pouriya-sedaghat/karta/internal/inbox"
	"github.com/pouriya-sedaghat/karta/internal/online"
	"github.com/pouriya-sedaghat/karta/internal/safefile"
)

// Acquisition failure codes (besides the online package's transport codes).
const (
	CodeRateLimited      = "rate_limited"
	CodeSourceChanged    = "source_changed"
	CodeChecksumMismatch = "distributor_checksum_mismatch"
	CodeSourceConfig     = "source_config"
)

// AcquisitionSuffix names the acquisition record next to a spooled snapshot.
const AcquisitionSuffix = ".osm.pbf.acquisition.json"

// keepSpooled is how many complete acquisitions stay in the spool.
const keepSpooled = 2

// AcquireConfig configures `karta bridge acquire`.
type AcquireConfig struct {
	SourcePath string
	// Spool is written by acquire and read (read-only) by sign.
	Spool         string
	MaxInputBytes int64
	ReserveBytes  int64
	Version       string
}

// Acquirer polls the distributor and spools new complete snapshots.
type Acquirer struct {
	cfg    AcquireConfig
	log    *slog.Logger
	state  AcquireState
	now    func() time.Time
	jitter func() float64
	// client overrides the HTTP client (tests).
	client *http.Client
}

// NewAcquirer prepares the spool and loads the state (refusing an unreadable
// one, which would forget what was already downloaded).
func NewAcquirer(cfg AcquireConfig, log *slog.Logger) (*Acquirer, error) {
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{{filepath.Join(cfg.Spool, AcquireDirName), 0o755}, {filepath.Join(cfg.Spool, partialDirName), 0o700}} {
		if err := os.Mkdir(d.path, d.mode); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if fi, err := os.Lstat(d.path); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", d.path)
		}
	}
	st, err := ReadAcquireState(cfg.Spool)
	if err != nil {
		return nil, fmt.Errorf("the acquire state exists but cannot be read (%w); restore it or move it aside", err)
	}
	a := &Acquirer{cfg: cfg, log: log, now: time.Now, jitter: rand.Float64} // #nosec G404 -- backoff jitter
	if st != nil {
		a.state = *st
	}
	a.state.Version = stateVersion
	tmps, _ := filepath.Glob(filepath.Join(cfg.Spool, ".tmp-*"))
	for _, t := range tmps {
		_ = os.Remove(t)
	}
	return a, a.save()
}

// State returns a copy of the state.
func (a *Acquirer) State() AcquireState { return a.state }

func (a *Acquirer) save() error {
	a.state.UpdatedAt = a.now().UTC()
	return writeJSONState(filepath.Join(a.cfg.Spool, AcquireDirName, stateFileName), a.state)
}

func codedErr(code, format string, args ...any) error {
	return &online.Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Run checks the distributor until ctx ends.
func (a *Acquirer) Run(ctx context.Context) {
	first := true
	for {
		wait := time.Duration(0)
		if n := a.state.NextAttemptAt; n != nil {
			wait = n.Sub(a.now())
		}
		if first && wait > time.Minute {
			wait = time.Minute // a restart checks soon, within the backoff it saw
		}
		first = false
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		if ctx.Err() != nil {
			return
		}
		a.RunOnce(ctx)
	}
}

// RunOnce runs one check and schedules the next.
func (a *Acquirer) RunOnce(ctx context.Context) {
	err := a.CheckOnce(ctx)
	if ctx.Err() != nil {
		_ = a.save()
		return
	}
	now := a.now().UTC()
	poll, initial, maxDelay := time.Hour, DefaultRetryInitial, DefaultRetryMax
	if src, lerr := LoadSource(a.cfg.SourcePath); lerr == nil {
		poll, initial, maxDelay = src.PollInterval.D(), src.RetryInitial.D(), src.RetryMax.D()
	}
	var next time.Time
	if err == nil {
		a.state.LastSuccessAt, a.state.LastError, a.state.ConsecutiveFailures = &now, nil, 0
		next = now.Add(time.Duration(float64(poll) * (0.9 + 0.2*a.jitter())))
	} else {
		a.state.ConsecutiveFailures++
		code := online.CodeOf(err)
		if code == "" {
			code = online.CodeIO
		}
		a.state.LastError = &StateError{At: now, Code: code, Message: trunc(err.Error(), 500)}
		next = now.Add(online.Backoff(initial, maxDelay, a.state.ConsecutiveFailures, a.jitter()))
		var ra *retryAfter
		if errors.As(err, &ra) && now.Add(ra.d).After(next) {
			next = now.Add(ra.d)
		}
		a.log.Warn("source check failed", "code", code, "err", err, "consecutive_failures", a.state.ConsecutiveFailures,
			"next_attempt_at", next.Format(time.RFC3339))
	}
	a.state.NextAttemptAt = &next
	if serr := a.save(); serr != nil {
		a.log.Error("could not save the acquire state", "err", serr)
	}
}

// retryAfter carries a distributor's Retry-After.
type retryAfter struct {
	err error
	d   time.Duration
}

func (r *retryAfter) Error() string { return r.err.Error() }
func (r *retryAfter) Unwrap() error { return r.err }

func (a *Acquirer) httpClient(src *Source) (*http.Client, error) {
	if a.client != nil {
		return a.client, nil
	}
	opts := online.ClientOptions{Policy: online.Policy{Allowed: src.networks}, DialTimeout: 10 * time.Second, HeaderTimeout: 30 * time.Second,
		HandshakeLimit: 10 * time.Second}
	if src.CAFile != "" {
		roots, err := online.LoadRoots(src.CAFile)
		if err != nil {
			return nil, codedErr(CodeSourceConfig, "ca_file: %v", err)
		}
		opts.Roots = roots
	}
	return online.NewHTTPClient(opts), nil
}

func (a *Acquirer) request(ctx context.Context, method string, u *url.URL, ua string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, codedErr(online.CodeURLRefused, "%s: %v", redact(u), err)
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept-Encoding", "identity")
	return req, nil
}

// checkStatus accepts the expected statuses; 429 and 503 carry the
// distributor's Retry-After.
func checkStatus(u *url.URL, resp *http.Response, ok ...int) error {
	for _, c := range ok {
		if resp.StatusCode == c {
			if ce := strings.TrimSpace(resp.Header.Get("Content-Encoding")); ce != "" && !strings.EqualFold(ce, "identity") {
				return codedErr(online.CodeEncoding, "%s used Content-Encoding %q; only identity is accepted", redact(u), trunc(ce, 40))
			}
			return nil
		}
	}
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return codedErr(online.CodeRedirectRefused, "%s answered %d (redirects are refused)", redact(u), resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
		e := codedErr(CodeRateLimited, "%s answered %d", redact(u), resp.StatusCode)
		return &retryAfter{err: e, d: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	return codedErr(online.CodeHTTPStatus, "%s answered %d", redact(u), resp.StatusCode)
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return min(time.Duration(n)*time.Second, 24*time.Hour)
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(time.Until(t), 24*time.Hour)
	}
	return 0
}

var strongETag = regexp.MustCompile(`^"[\x21\x23-\x7e]{1,200}"$`)
var md5Pattern = regexp.MustCompile(`^([0-9a-fA-F]{32})\b`)

// hint asks the distributor about its current file: HEAD of the snapshot
// and, if configured, its .md5.
func (a *Acquirer) hint(ctx context.Context, c *http.Client, src *Source) (*Hint, error) {
	hctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := a.request(hctx, http.MethodHead, src.snapshotURL, src.UserAgent)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, online.Classify(hctx, err)
	}
	_ = resp.Body.Close()
	if err := checkStatus(src.snapshotURL, resp, http.StatusOK); err != nil {
		return nil, err
	}
	h := &Hint{ContentLength: resp.ContentLength, ObservedAt: a.now().UTC()}
	if et := resp.Header.Get("ETag"); strongETag.MatchString(et) {
		h.ETag = et
	}
	h.LastModified = trunc(resp.Header.Get("Last-Modified"), 64)
	if src.md5URL != nil {
		req, err := a.request(hctx, http.MethodGet, src.md5URL, src.UserAgent)
		if err != nil {
			return nil, err
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, online.Classify(hctx, err)
		}
		defer resp.Body.Close()
		if err := checkStatus(src.md5URL, resp, http.StatusOK); err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if err != nil {
			return nil, online.Classify(hctx, err)
		}
		m := md5Pattern.FindSubmatch(b)
		if m == nil {
			return nil, codedErr(CodeChecksumMismatch, "%s does not hold an MD5 checksum", redact(src.md5URL))
		}
		h.MD5 = strings.ToLower(string(m[1]))
	}
	return h, nil
}

// CheckOnce runs one check: hints; a full download only when they changed,
// or the last full verification is older than reverify_interval, or a
// download was interrupted; a new spool entry only for new bytes.
func (a *Acquirer) CheckOnce(ctx context.Context) error {
	now := a.now().UTC()
	a.state.LastCheckAt = &now
	src, err := LoadSource(a.cfg.SourcePath)
	if err != nil {
		return codedErr(CodeSourceConfig, "%v", err)
	}
	a.state.Source = redact(src.snapshotURL)
	c, err := a.httpClient(src)
	if err != nil {
		return err
	}
	before, err := a.hint(ctx, c, src)
	if err != nil {
		return err
	}
	last := a.state.Last
	changed := last == nil || !before.same(last.Hint)
	due := last == nil || now.Sub(last.VerifiedAt) >= src.ReverifyInterval.D()
	if !changed && !due && a.state.Download == nil {
		a.state.Hint, a.state.LastOutcome = before, "unchanged: every hint equals the last acquisition's"
		return nil
	}
	maxBytes := a.cfg.MaxInputBytes
	if before.ContentLength > maxBytes {
		return codedErr(online.CodeTooLarge, "the distributor announces %d bytes, above the limit of %d (KARTA_MAX_INPUT_MB)", before.ContentLength, maxBytes)
	}
	started := a.now().UTC()
	part, sum, md5sum, size, err := a.download(ctx, c, src, before)
	if err != nil {
		return err
	}
	after, err := a.hint(ctx, c, src)
	if err != nil {
		return err
	}
	discard := func() { _ = os.Remove(part); a.state.Download = nil }
	if !after.same(before) {
		discard()
		return codedErr(CodeSourceChanged, "the distributor's file changed during the download (hints before %+v, after %+v); downloading again later", *before, *after)
	}
	var matched *bool
	if before.MD5 != "" {
		ok := md5sum == before.MD5
		matched = &ok
		if !ok {
			discard()
			return codedErr(CodeChecksumMismatch, "the download's MD5 %s differs from the distributor's %s; discarded", md5sum, before.MD5)
		}
	}
	finished := a.now().UTC()
	if last != nil && sum == last.SHA256 {
		discard()
		last.VerifiedAt, last.Hint = finished, before
		a.state.Hint = before
		a.state.LastOutcome = "re-verified: the full download has the bytes acquired before"
		a.log.Info("re-verified the distributor's file: unchanged bytes", "sha256", sum, "size_bytes", size)
		return nil
	}
	acq := &Acquisition{SHA256: sum, SizeBytes: size, MD5: md5sum, Source: redact(src.snapshotURL), StartedAt: started, FinishedAt: finished,
		VerifiedAt: finished, Hint: before, MD5Matched: matched, UserAgent: src.UserAgent, Version: a.cfg.Version}
	if err := a.spool(part, acq); err != nil {
		return err
	}
	a.state.Last, a.state.Hint, a.state.Download = acq, before, nil
	a.state.LastOutcome = "new bytes spooled for the signer"
	a.log.Info("new snapshot acquired and spooled", "sha256", sum, "size_bytes", size, "md5_matched", matched)
	a.prune(sum)
	return nil
}

// download fetches the whole file into the partial file, resuming an
// interrupted transfer only with the same strong ETag and size (If-Range).
func (a *Acquirer) download(ctx context.Context, c *http.Client, src *Source, h *Hint) (string, string, string, int64, error) {
	part := filepath.Join(a.cfg.Spool, partialDirName, "current.part")
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600) // #nosec G304 -- our partial directory
	if err != nil {
		return "", "", "", 0, storageErr(err)
	}
	defer f.Close()
	sh, mh := sha256.New(), md5.New() // #nosec G401 -- distributor checksum format, not a security function
	have := int64(0)
	d := a.state.Download
	if fi, err := f.Stat(); err == nil && d != nil && d.ETag != "" && d.ETag == h.ETag && d.SizeBytes == h.ContentLength && fi.Size() > 0 &&
		fi.Size() < d.SizeBytes {
		if n, err := io.Copy(io.MultiWriter(sh, mh), io.LimitReader(f, fi.Size())); err == nil && n == fi.Size() {
			have = n
		}
	}
	if have == 0 {
		sh, mh = sha256.New(), md5.New() // #nosec G401 -- see above
		d = &DownloadProgress{SizeBytes: h.ContentLength, ETag: h.ETag, StartedAt: a.now().UTC()}
		if err := f.Truncate(0); err != nil {
			return "", "", "", 0, storageErr(err)
		}
	}
	d.Attempts++
	a.state.Download = d
	_ = a.save()
	if h.ContentLength > 0 {
		var st syscall.Statfs_t
		if err := syscall.Statfs(a.cfg.Spool, &st); err == nil {
			free := int64(st.Bavail) * int64(st.Bsize) // #nosec G115 -- block counts fit
			if need := h.ContentLength - have + a.cfg.ReserveBytes; free < need {
				return "", "", "", 0, codedErr(online.CodeStorage, "the spool has %d bytes free; the download needs %d plus a reserve of %d",
					free, h.ContentLength-have, a.cfg.ReserveBytes)
			}
		}
	}
	dctx, cancel := context.WithTimeout(ctx, src.DownloadTimeout.D())
	defer cancel()
	var stalled atomic.Bool
	watchdog := time.AfterFunc(src.StallTimeout.D(), func() { stalled.Store(true); cancel() })
	defer watchdog.Stop()
	fail := func(err error) (string, string, string, int64, error) {
		if stalled.Load() {
			return "", "", "", 0, codedErr(online.CodeStalled, "no data received for %s", src.StallTimeout.D())
		}
		var coded *online.Error
		if errors.As(err, &coded) {
			return "", "", "", 0, err
		}
		return "", "", "", 0, online.Classify(dctx, err)
	}
	req, err := a.request(dctx, http.MethodGet, src.snapshotURL, src.UserAgent)
	if err != nil {
		return "", "", "", 0, err
	}
	if have > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
		req.Header.Set("If-Range", d.ETag)
	}
	resp, err := c.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	if err := checkStatus(src.snapshotURL, resp, http.StatusOK, http.StatusPartialContent); err != nil {
		return "", "", "", 0, err
	}
	total := resp.ContentLength
	switch resp.StatusCode {
	case http.StatusOK:
		if have > 0 { // the file changed (If-Range) or the range was ignored: start again
			have, sh, mh = 0, sha256.New(), md5.New() // #nosec G401 -- see above
			if err := f.Truncate(0); err != nil {
				return "", "", "", 0, storageErr(err)
			}
		}
		if total >= 0 && h.ContentLength >= 0 && total != h.ContentLength {
			return "", "", "", 0, codedErr(CodeSourceChanged, "the distributor sends %d bytes after announcing %d", total, h.ContentLength)
		}
		d.SizeBytes, d.ETag = total, ""
		if et := resp.Header.Get("ETag"); strongETag.MatchString(et) {
			d.ETag = et
		}
	case http.StatusPartialContent:
		want := fmt.Sprintf("bytes %d-%d/%d", have, d.SizeBytes-1, d.SizeBytes)
		if have == 0 || resp.Header.Get("Content-Range") != want {
			_ = f.Truncate(0)
			a.state.Download = nil
			return "", "", "", 0, codedErr(online.CodeSizeMismatch, "the distributor answered range %q, not %q; the next attempt starts again",
				trunc(resp.Header.Get("Content-Range"), 80), want)
		}
		total = d.SizeBytes
	}
	if total > a.cfg.MaxInputBytes {
		return "", "", "", 0, codedErr(online.CodeTooLarge, "the snapshot is %d bytes, above the limit of %d", total, a.cfg.MaxInputBytes)
	}
	if _, err := f.Seek(have, io.SeekStart); err != nil {
		return "", "", "", 0, storageErr(err)
	}
	limit := a.cfg.MaxInputBytes
	if total >= 0 {
		limit = total
	}
	got, err := copyBody(f, io.MultiWriter(sh, mh), resp.Body, have, limit, total, watchdog, src.StallTimeout.D(), d)
	if err != nil {
		var coded *online.Error
		if errors.As(err, &coded) && coded.Code == online.CodeTooLarge {
			_ = f.Truncate(0)
			a.state.Download = nil
		}
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return "", "", "", 0, storageErr(err)
	}
	if total >= 0 && got != total {
		return "", "", "", 0, codedErr(online.CodeTruncated, "the transfer ended after %d of %d bytes; the next attempt resumes", got, total)
	}
	return part, hex.EncodeToString(sh.Sum(nil)), hex.EncodeToString(mh.Sum(nil)), got, nil
}

func copyBody(f *os.File, h io.Writer, body io.Reader, have, limit, total int64, watchdog *time.Timer, stall time.Duration, d *DownloadProgress) (int64, error) {
	buf := make([]byte, 1<<20)
	n := have
	half := total / 2
	for {
		k, rerr := body.Read(buf)
		if k > 0 {
			watchdog.Reset(stall)
			if n+int64(k) > limit {
				return n, codedErr(online.CodeTooLarge, "the distributor sent more than %d bytes; discarded", limit)
			}
			if _, err := f.Write(buf[:k]); err != nil {
				return n, storageErr(err)
			}
			_, _ = h.Write(buf[:k])
			prev := n
			n += int64(k)
			d.Bytes = n
			if total > 0 && prev < half && n >= half {
				failpoint.Hit("bridge.acquire_mid_download")
			}
		}
		if rerr == io.EOF {
			return n, nil
		}
		if rerr != nil {
			return n, rerr
		}
	}
}

func storageErr(err error) error {
	if safefile.IsNoSpace(err) {
		return codedErr(online.CodeStorage, "the spool volume is full: %v", err)
	}
	return codedErr(online.CodeIO, "%v", err)
}

// spool hands new bytes to the signer with the inbox completion protocol:
// the snapshot (renamed from the partial file), the acquisition record, and
// the ready marker last.
func (a *Acquirer) spool(part string, acq *Acquisition) error {
	dir := a.cfg.Spool
	name := acq.SHA256
	snap := filepath.Join(dir, name+inbox.SnapshotSuffix)
	for _, suf := range []string{inbox.MarkerSuffix, inbox.SnapshotSuffix, AcquisitionSuffix} {
		_ = os.Remove(filepath.Join(dir, name+suf))
	}
	b, err := safefile.MarshalIndent(acq)
	if err != nil {
		return err
	}
	if err := safefile.WriteAtomic(filepath.Join(dir, name+AcquisitionSuffix), b, 0o644); err != nil {
		return storageErr(err)
	}
	if err := os.Chmod(part, 0o644); err != nil { // #nosec G302 -- the signer (another user) reads the spool
		return storageErr(err)
	}
	if err := os.Rename(part, snap); err != nil {
		return storageErr(err)
	}
	if err := safefile.WriteAtomic(filepath.Join(dir, name+inbox.MarkerSuffix), []byte(name+"  "+name+inbox.SnapshotSuffix+"\n"), 0o644); err != nil {
		return storageErr(err)
	}
	return nil
}

// prune keeps the newest complete acquisitions.
func (a *Acquirer) prune(current string) {
	entries, err := inbox.Scan(a.cfg.Spool, 1000)
	if err != nil {
		return
	}
	var done []inbox.Entry
	for _, e := range entries {
		if e.Complete() && e.Name != current {
			done = append(done, e)
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].Marker.ModTime.Before(done[j].Marker.ModTime) })
	for i := 0; i < len(done)-(keepSpooled-1); i++ {
		n := done[i].Name
		_ = os.Remove(filepath.Join(a.cfg.Spool, n+inbox.MarkerSuffix))
		_ = os.Remove(filepath.Join(a.cfg.Spool, n+inbox.SnapshotSuffix))
		_ = os.Remove(filepath.Join(a.cfg.Spool, n+AcquisitionSuffix))
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}
