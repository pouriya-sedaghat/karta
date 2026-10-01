package online

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/failpoint"
)

// Requester issues the fetcher's requests to one source.
type Requester struct {
	Client    *http.Client
	Token     string
	UserAgent string
}

func (r *Requester) request(ctx context.Context, u *url.URL) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errorf(CodeURLRefused, "%s: %v", redact(u), err)
	}
	req.Header.Set("User-Agent", r.UserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	return req, nil
}

// checkResponse accepts the expected statuses and an identity encoding.
func checkResponse(u *url.URL, resp *http.Response, ok ...int) error {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return errorf(CodeRedirectRefused, "%s answered %d (redirects are refused)", redact(u), resp.StatusCode)
	}
	good := false
	for _, c := range ok {
		good = good || resp.StatusCode == c
	}
	if !good {
		return errorf(CodeHTTPStatus, "%s answered %d", redact(u), resp.StatusCode)
	}
	if ce := strings.TrimSpace(resp.Header.Get("Content-Encoding")); ce != "" && !strings.EqualFold(ce, "identity") {
		return errorf(CodeEncoding, "%s used Content-Encoding %q; only identity is accepted", redact(u), truncate(ce, 40))
	}
	return nil
}

// FetchSmall downloads a small file (the manifest envelope, a provenance
// sidecar) of at most max bytes within timeout.
func (r *Requester) FetchSmall(ctx context.Context, u *url.URL, max int64, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := r.request(ctx, u)
	if err != nil {
		return nil, err
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, classify(ctx, err)
	}
	defer resp.Body.Close()
	if err := checkResponse(u, resp, http.StatusOK); err != nil {
		return nil, err
	}
	if resp.ContentLength > max {
		return nil, errorf(CodeTooLarge, "%s is %d bytes, above the limit of %d", redact(u), resp.ContentLength, max)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, classify(ctx, err)
	}
	if int64(len(b)) > max {
		return nil, errorf(CodeTooLarge, "%s is larger than %d bytes", redact(u), max)
	}
	if resp.ContentLength >= 0 && int64(len(b)) != resp.ContentLength {
		return nil, errorf(CodeTruncated, "%s: %d of %d bytes received", redact(u), len(b), resp.ContentLength)
	}
	return b, nil
}

// DownloadSpec describes one snapshot download.
type DownloadSpec struct {
	URL    *url.URL
	SHA256 string
	Size   int64
	// Dir is the fetcher's private partial-download directory.
	Dir     string
	Timeout time.Duration
	Stall   time.Duration
	// Progress, if set, receives the bytes held so far, at most once a
	// second and at the end.
	Progress func(have int64)
}

// PartialPath is where a download of digest is kept until it completes.
func PartialPath(dir, digest string) string { return filepath.Join(dir, digest+".part") }

var errStalled = errors.New("stalled")

var contentRange = regexp.MustCompile(`^bytes ([0-9]+)-([0-9]+)/([0-9]+)$`)

// Download fetches a snapshot into Dir/<sha256>.part and returns that path
// once the file has exactly the signed size and digest.
//
// An interrupted download is resumed only with a verified byte identity:
// the bytes already held are hashed again, the rest is requested with
// Range (and If-Range with the strong ETag of the first response, so a
// changed file is sent whole), the response must cover exactly the missing
// range, and the whole file must still match the signed SHA-256; a file
// that does not is deleted. Responses longer than the signed size are cut
// off and the partial file discarded. Nothing here is visible to the
// publisher: the caller moves the verified file into the outbox.
func (r *Requester) Download(ctx context.Context, s DownloadSpec) (path string, err error) {
	part := PartialPath(s.Dir, s.SHA256)
	etagPath := filepath.Join(s.Dir, s.SHA256+".etag")
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600) // #nosec G304 -- digest-named file in our private directory
	if err != nil {
		return "", storageErr(err)
	}
	defer f.Close()
	discard := func() {
		_ = f.Truncate(0)
		_ = os.Remove(etagPath)
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", errorf(CodeIO, "partial download is not a regular file")
	}
	have := st.Size()
	h := sha256.New()
	if have > s.Size {
		discard()
		have = 0
	}
	if have > 0 {
		if n, err := io.Copy(h, io.LimitReader(f, have)); err != nil || n != have {
			discard()
			have, h = 0, sha256.New()
		}
	}
	if have == s.Size {
		if hex.EncodeToString(h.Sum(nil)) == s.SHA256 {
			_ = os.Remove(etagPath)
			return part, nil
		}
		discard()
		have, h = 0, sha256.New()
	}

	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	var stalled atomic.Bool
	watchdog := time.AfterFunc(s.Stall, func() { stalled.Store(true); cancel() })
	defer watchdog.Stop()
	fail := func(err error) (string, error) {
		if stalled.Load() {
			return "", errorf(CodeStalled, "no data received for %s", s.Stall)
		}
		return "", classify(ctx, err)
	}

	req, err := r.request(ctx, s.URL)
	if err != nil {
		return "", err
	}
	if have > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
		if etag := readETag(etagPath); etag != "" {
			req.Header.Set("If-Range", etag)
		}
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	if err := checkResponse(s.URL, resp, http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable); err != nil {
		return "", err
	}
	switch resp.StatusCode {
	case http.StatusRequestedRangeNotSatisfiable:
		discard()
		return "", errorf(CodeHTTPStatus, "%s refused to resume at byte %d (416); the next attempt starts again", redact(s.URL), have)
	case http.StatusOK:
		if have > 0 { // the server ignored the range or the file changed: start again
			discard()
			have, h = 0, sha256.New()
		}
		if resp.ContentLength >= 0 && resp.ContentLength != s.Size {
			return "", errorf(CodeSizeMismatch, "%s is %d bytes, the manifest signs %d", redact(s.URL), resp.ContentLength, s.Size)
		}
		saveETag(etagPath, resp.Header.Get("ETag"))
	case http.StatusPartialContent:
		m := contentRange.FindStringSubmatch(resp.Header.Get("Content-Range"))
		if have == 0 || m == nil || m[1] != strconv.FormatInt(have, 10) || m[2] != strconv.FormatInt(s.Size-1, 10) ||
			m[3] != strconv.FormatInt(s.Size, 10) || (resp.ContentLength >= 0 && resp.ContentLength != s.Size-have) {
			discard()
			return "", errorf(CodeSizeMismatch, "%s answered a range other than bytes %d-%d/%d; the next attempt starts again",
				redact(s.URL), have, s.Size-1, s.Size)
		}
	}
	if _, err := f.Seek(have, io.SeekStart); err != nil {
		return "", errorf(CodeIO, "seek: %v", err)
	}
	if err := f.Truncate(have); err != nil {
		return "", storageErr(err)
	}
	total, err := copyBody(f, h, resp.Body, have, s, watchdog)
	if s.Progress != nil {
		s.Progress(total)
	}
	if err != nil {
		var coded *Error
		if errors.As(err, &coded) {
			if coded.Code == CodeTooLarge {
				discard()
			}
			return "", err
		}
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return "", storageErr(err)
	}
	if total < s.Size {
		return "", errorf(CodeTruncated, "the transfer ended after %d of %d bytes; the next attempt resumes", total, s.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != s.SHA256 {
		_ = f.Close()
		_ = os.Remove(part)
		_ = os.Remove(etagPath)
		return "", errorf(CodeDigestMismatch, "the downloaded snapshot has SHA-256 %s, the manifest signs %s; discarded", got, s.SHA256)
	}
	_ = os.Remove(etagPath)
	return part, nil
}

// copyBody appends the body to f while hashing, never beyond the signed
// size, resetting the stall watchdog on progress.
func copyBody(f *os.File, h hash.Hash, body io.Reader, have int64, s DownloadSpec, watchdog *time.Timer) (int64, error) {
	buf := make([]byte, 64<<10)
	total := have
	lastReport := time.Now()
	half := s.Size / 2
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			watchdog.Reset(s.Stall)
			if total+int64(n) > s.Size {
				return total, errorf(CodeTooLarge, "the source sent more than the signed %d bytes; discarded", s.Size)
			}
			if _, err := f.Write(buf[:n]); err != nil {
				return total, storageErr(err)
			}
			h.Write(buf[:n])
			prev := total
			total += int64(n)
			if prev < half && total >= half {
				failpoint.Hit("fetch.mid_download")
			}
			if s.Progress != nil && time.Since(lastReport) >= time.Second {
				s.Progress(total)
				lastReport = time.Now()
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

func storageErr(err error) error {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return errorf(CodeStorage, "the fetcher's volume is full: %v", err)
	}
	return errorf(CodeIO, "%v", err)
}

var strongETag = regexp.MustCompile(`^"[\x21\x23-\x7e]{1,200}"$`)

// saveETag keeps a strong ETag for If-Range; weak or malformed ones are
// not kept, and a resumed request then sends no If-Range (the final digest
// check still decides).
func saveETag(path, etag string) {
	if !strongETag.MatchString(etag) {
		_ = os.Remove(path)
		return
	}
	_ = writeFileAtomic(path, []byte(etag), 0o600)
}

func readETag(path string) string {
	b, err := readRegular(path, 256)
	if err != nil || !strongETag.Match(b) {
		return ""
	}
	return string(b)
}

// writeFileAtomic writes b to a temporary file in the same directory and
// renames it over path.
func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(path), ".tmp-"+filepath.Base(path))
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode) // #nosec G304 -- our own directory
	if err != nil {
		return storageErr(err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return storageErr(err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return storageErr(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return storageErr(err)
	}
	if err := os.Chmod(tmp, mode); err != nil { // independent of the umask
		_ = os.Remove(tmp)
		return storageErr(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return storageErr(err)
	}
	return nil
}

// sha256Hex returns the digest of b.
func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
