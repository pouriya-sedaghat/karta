// Package online fetches a signed, complete snapshot from one configured
// HTTPS origin. It has no database or public-request responsibilities.
package online

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/inbox"
)

const MaxManifestBytes = 16 << 10
const MaxProvenanceBytes = 1 << 20

// Config is opt-in. Client is a test seam for a controlled local HTTPS
// server; production always uses the restrictive client below.
type Config struct {
	ManifestURL  string
	PublicKey    ed25519.PublicKey
	RegionID     string
	MaxBytes     int64
	Timeout      time.Duration
	ReserveBytes int64
	Client       *http.Client
}

// Manifest is the signed payload. A signature is over the exact canonical
// JSON encoding of these fields; unknown/duplicate fields cannot be signed.
type Manifest struct {
	RegionID            string    `json:"region_id"`
	IssuedAt            time.Time `json:"issued_at"`
	ExpiresAt           time.Time `json:"expires_at"`
	DataTimestamp       time.Time `json:"data_timestamp"`
	SHA256              string    `json:"sha256"`
	SizeBytes           int64     `json:"size_bytes"`
	SnapshotURL         string    `json:"snapshot_url"`
	ProvenanceURL       string    `json:"provenance_url,omitempty"`
	ProvenanceSHA256    string    `json:"provenance_sha256,omitempty"`
	ProvenanceSizeBytes int64     `json:"provenance_size_bytes,omitempty"`
}

type envelope struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// Parse verifies a signed manifest before any URL in it can be requested.
func Parse(data []byte, key ed25519.PublicKey, origin *url.URL, region string, maxBytes int64, now time.Time) (Manifest, error) {
	var m Manifest
	if len(data) == 0 || len(data) > MaxManifestBytes || len(key) != ed25519.PublicKeySize {
		return m, errors.New("invalid manifest size or signing key")
	}
	var e envelope
	if err := strictJSON(data, &e); err != nil {
		return m, fmt.Errorf("manifest envelope: %w", err)
	}
	p, err := base64.StdEncoding.Strict().DecodeString(e.Payload)
	if err != nil || len(p) == 0 || len(p) > MaxManifestBytes {
		return m, errors.New("invalid manifest payload")
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(e.Signature)
	if err != nil || !ed25519.Verify(key, p, sig) {
		return m, errors.New("manifest signature invalid")
	}
	if err := strictJSON(p, &m); err != nil {
		return m, fmt.Errorf("manifest payload: %w", err)
	}
	canonical, err := json.Marshal(m)
	if err != nil || !bytes.Equal(canonical, p) {
		return m, errors.New("manifest payload is not canonical JSON")
	}
	if m.RegionID != region || region == "" {
		return m, errors.New("manifest region mismatch")
	}
	if m.IssuedAt.IsZero() || m.ExpiresAt.IsZero() || m.DataTimestamp.IsZero() ||
		m.IssuedAt.After(now.Add(10*time.Minute)) || !m.ExpiresAt.After(now) ||
		m.ExpiresAt.After(m.IssuedAt.Add(7*24*time.Hour)) || m.DataTimestamp.After(m.IssuedAt) {
		return m, errors.New("manifest timestamps invalid or expired")
	}
	if m.SizeBytes <= 0 || m.SizeBytes > maxBytes || !validDigest(m.SHA256) {
		return m, errors.New("manifest snapshot size or digest invalid")
	}
	if _, err := sameOriginURL(m.SnapshotURL, origin); err != nil {
		return m, fmt.Errorf("snapshot URL: %w", err)
	}
	if m.ProvenanceURL != "" || m.ProvenanceSHA256 != "" || m.ProvenanceSizeBytes != 0 {
		if m.ProvenanceSizeBytes <= 0 || m.ProvenanceSizeBytes > MaxProvenanceBytes || !validDigest(m.ProvenanceSHA256) {
			return m, errors.New("manifest provenance size or digest invalid")
		}
		if _, err := sameOriginURL(m.ProvenanceURL, origin); err != nil {
			return m, fmt.Errorf("provenance URL: %w", err)
		}
	}
	return m, nil
}

func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}
	return true
}

func strictJSON(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func sameOriginURL(raw string, origin *url.URL) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != origin.Host || u.User != nil || u.Fragment != "" ||
		u.RawQuery != "" || u.Opaque != "" || u.Path == "" || strings.HasPrefix(u.Path, "//") {
		return nil, errors.New("only a query-free HTTPS path on the configured origin is allowed")
	}
	return u, nil
}

// ValidateURL checks the configured origin before any request is made.
func ValidateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" ||
		u.RawQuery != "" || u.Opaque != "" || u.Path == "" || strings.HasPrefix(u.Path, "//") {
		return nil, errors.New("manifest URL must be a query-free HTTPS path without credentials")
	}
	if _, _, err := net.SplitHostPort(u.Host); err == nil {
		// An explicit port is allowed for trusted deployments, including CI.
	} else if strings.Contains(u.Host, ":") {
		return nil, errors.New("invalid source authority")
	}
	return u, nil
}

func publicIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	// IsGlobalUnicast includes special-use ranges that are not safe egress
	// destinations (CGNAT, benchmarking, documentation and transition nets).
	for _, p := range []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.88.99.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("224.0.0.0/3"), netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
		netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2002::/16"),
		netip.MustParsePrefix("fc00::/7"),
	} {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

func safeClient(origin *url.URL, timeout time.Duration) *http.Client {
	transport := &http.Transport{Proxy: nil, DisableCompression: true, MaxIdleConnsPerHost: 1,
		TLSHandshakeTimeout: min(timeout, 10*time.Second), ResponseHeaderTimeout: min(timeout, 15*time.Second)}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(host, origin.Hostname()) {
			return nil, errors.New("unexpected destination")
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		// Fail closed if a mixed DNS answer contains even one local address.
		if len(ips) == 0 {
			return nil, errors.New("no source address")
		}
		for _, ip := range ips {
			if !publicIP(ip.IP) {
				return nil, errors.New("source resolves to a disallowed address")
			}
		}
		var last error
		for _, ip := range ips {
			c, err := (&net.Dialer{Timeout: min(timeout, 10*time.Second)}).DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return errors.New("redirect refused")
	}}
}

// FetchManifest checks the signature and source policy with a bounded request.
func FetchManifest(ctx context.Context, c Config, now time.Time) (Manifest, error) {
	origin, err := ValidateURL(c.ManifestURL)
	if err != nil {
		return Manifest{}, err
	}
	client := c.Client
	if client == nil {
		client = safeClient(origin, c.Timeout)
		defer client.CloseIdleConnections()
	}
	b, err := get(ctx, client, c.ManifestURL, MaxManifestBytes)
	if err != nil {
		return Manifest{}, err
	}
	return Parse(b, c.PublicKey, origin, c.RegionID, c.MaxBytes, now)
}

func get(ctx context.Context, client *http.Client, raw string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > limit ||
		resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		return nil, errors.New("source response status, size or encoding invalid")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("source response too large")
	}
	return b, nil
}

// Download writes only completed, hashed bytes into private staging. Caller
// removes the returned directory after the shared publication path finishes.
func Download(ctx context.Context, c Config, m Manifest, stagingDir string) (_ *inbox.Staged, err error) {
	origin, err := ValidateURL(c.ManifestURL)
	if err != nil {
		return nil, err
	}
	if _, err := sameOriginURL(m.SnapshotURL, origin); err != nil {
		return nil, err
	}
	if m.SizeBytes > c.MaxBytes {
		return nil, errors.New("snapshot exceeds configured limit")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(stagingDir, &fs); err != nil {
		return nil, err
	}
	if c.ReserveBytes < 0 || fs.Bsize <= 0 {
		return nil, errors.New("invalid staging reserve or filesystem block size")
	}
	needed := new(big.Int).SetInt64(m.SizeBytes)
	needed.Add(needed, big.NewInt(c.ReserveBytes))
	needed.Add(needed, big.NewInt(max(m.ProvenanceSizeBytes, 0)))
	available := new(big.Int).SetUint64(fs.Bavail)
	available.Mul(available, big.NewInt(fs.Bsize))
	if available.Cmp(needed) < 0 {
		return nil, errors.New("insufficient private staging space")
	}
	dir, err := os.MkdirTemp(stagingDir, "online-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	client := c.Client
	if client == nil {
		client = safeClient(origin, c.Timeout)
		defer client.CloseIdleConnections()
	}
	path := filepath.Join(dir, "snapshot.osm.pbf")
	if err = downloadFile(ctx, client, m.SnapshotURL, path, m.SizeBytes, m.SHA256); err != nil {
		return nil, err
	}
	st := &inbox.Staged{Dir: dir, SnapshotPath: path, SHA256: m.SHA256, Size: m.SizeBytes}
	if m.ProvenanceURL != "" {
		if _, err = sameOriginURL(m.ProvenanceURL, origin); err != nil {
			return nil, err
		}
		st.SidecarPath = path + ".provenance.json"
		if err = downloadFile(ctx, client, m.ProvenanceURL, st.SidecarPath, m.ProvenanceSizeBytes, m.ProvenanceSHA256); err != nil {
			return nil, err
		}
	}
	return st, nil
}

func downloadFile(ctx context.Context, client *http.Client, raw, path string, size int64, digest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > size ||
		resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		return errors.New("source response status, size or encoding invalid")
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.OpenFile(filepath.Base(path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, size+1))
	if err != nil {
		return err
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("source byte count or digest mismatch")
	}
	return f.Sync()
}
