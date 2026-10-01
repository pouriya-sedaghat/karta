// Package online implements Karta's online snapshot source (Stage 3): a
// configured HTTPS location that publishes signed snapshot manifests, the
// fetcher that polls it and downloads new snapshots within strict limits,
// and the verification the publisher repeats before anything is built.
//
// Trust does not come from the location. A manifest is a DSSE envelope
// (https://github.com/secure-systems-lab/dsse) whose Ed25519 signature must
// verify with a public key pinned in the reviewed source file
// (config/sources/NAME.json). The signed payload binds the region id and
// box, a serial number, a validity window, and the exact SHA-256, size and
// data timestamp of the snapshot (and of its provenance sidecar). An
// unsigned or wrongly signed checksum served next to a snapshot authorizes
// nothing. See docs/adr/0004-stage3-online-updates.md.
//
// The fetcher is the only Karta process with a network route out. It holds
// no database credential: it writes verified files into its outbox using
// the inbox completion protocol, and the publisher (which has no route out)
// processes them like inbox submissions after verifying the envelope again.
package online

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration written as a Go duration string ("90s", "1h").
type Duration time.Duration

// UnmarshalJSON parses a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"1h\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes the duration string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// D returns the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Source is one online source file. It is part of the reviewed deployment
// configuration: changing the trusted keys or the destinations is a
// configuration change, never something the source itself can do.
type Source struct {
	// RegionID must be the id of the region the publisher serves.
	RegionID string `json:"region_id"`
	// ManifestURL is where the signed manifest is published: https, no
	// credentials, query or fragment.
	ManifestURL string `json:"manifest_url"`
	// AllowedHosts are further host[:port] values snapshot and provenance
	// URLs may point to (the manifest's host is always allowed). A host
	// without a port allows port 443 only.
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
	// AllowedNetworks lists CIDR prefixes that may be connected to although
	// they are not public addresses (a private mirror, or the test source).
	// Every other loopback, private, link-local, shared, reserved or
	// multicast address is refused when connecting.
	AllowedNetworks []string `json:"allowed_networks,omitempty"`
	// CAFile, when set, holds the only root certificates trusted for this
	// source (PEM); otherwise the system roots are used.
	CAFile string `json:"ca_file,omitempty"`
	// AuthTokenFile, when set, holds a bearer token sent in the
	// Authorization header to the allowed hosts (never in a URL, never
	// logged). It is a secret file outside Git.
	AuthTokenFile string `json:"auth_token_file,omitempty"`
	// TrustedKeys are the Ed25519 public keys whose signatures authorize
	// snapshots (1 to 8; several during a key rotation).
	TrustedKeys []TrustedKey `json:"trusted_keys"`
	// RequireOperatorAuthorization keeps the Stage 2 rule for online
	// snapshots too: a valid signature is required, and the digest must
	// also be pinned in the region file or authorized by an operator. The
	// default (false) lets a valid signature authorize the signed digest.
	RequireOperatorAuthorization bool `json:"require_operator_authorization,omitempty"`
	// PollInterval is the time between successful checks.
	PollInterval Duration `json:"poll_interval"`
	// MaxManifestValidity bounds expires_at - issued_at of a manifest, so a
	// captured manifest cannot be replayed for long.
	MaxManifestValidity Duration `json:"max_manifest_validity"`
	// DownloadTimeout bounds one snapshot download (default 1h).
	DownloadTimeout Duration `json:"download_timeout,omitempty"`
	// StallTimeout aborts a download that receives no data for this long
	// (default 60s).
	StallTimeout Duration `json:"stall_timeout,omitempty"`
	// RetryInitial and RetryMax bound the exponential backoff (with jitter)
	// after a failed check (defaults 1m and 1h).
	RetryInitial Duration `json:"retry_initial,omitempty"`
	RetryMax     Duration `json:"retry_max,omitempty"`
	// MaxDownloadAttempts is how often one snapshot is downloaded before
	// the fetcher stops trying it for AbandonFor (defaults 5 and 24h).
	MaxDownloadAttempts int      `json:"max_download_attempts,omitempty"`
	AbandonFor          Duration `json:"abandon_for,omitempty"`

	manifestURL *url.URL
	hosts       map[string]bool
	networks    []netip.Prefix
	keys        []key
}

// TrustedKey is one pinned signing key.
type TrustedKey struct {
	// ID names the key in signatures (DSSE keyid).
	ID string `json:"id"`
	// Ed25519PublicKey is the raw 32-byte public key, base64 (standard).
	Ed25519PublicKey string `json:"ed25519_public_key"`
	// NotAfter, when set, ends the key's validity: after it, signatures by
	// the key are not accepted (a scheduled rotation).
	NotAfter *time.Time `json:"not_after,omitempty"`
}

type key struct {
	id       string
	pub      []byte
	notAfter *time.Time
}

// Defaults and bounds.
const (
	DefaultDownloadTimeout     = time.Hour
	DefaultStallTimeout        = time.Minute
	DefaultRetryInitial        = time.Minute
	DefaultRetryMax            = time.Hour
	DefaultMaxDownloadAttempts = 5
	DefaultAbandonFor          = 24 * time.Hour
	maxSourceFile              = 64 << 10
	maxTrustedKeys             = 8
)

var (
	regionIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	keyIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	hostPattern     = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
)

// LoadSource reads and validates a source file. Unknown fields and repeated
// keys are rejected so a typo cannot silently weaken a check.
func LoadSource(path string) (*Source, error) {
	b, err := readRegular(path, maxSourceFile)
	if err != nil {
		return nil, err
	}
	s, err := ParseSource(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// ParseSource decodes and validates source file content.
func ParseSource(b []byte) (*Source, error) {
	var s Source
	if err := strictDecode(b, &s); err != nil {
		return nil, err
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Source) validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !regionIDPattern.MatchString(s.RegionID) {
		add("region_id %q must match %s", s.RegionID, regionIDPattern)
	}
	u, err := checkURL(s.ManifestURL)
	if err != nil {
		add("manifest_url: %v", err)
	} else {
		s.manifestURL = u
	}
	s.hosts = map[string]bool{}
	if u != nil {
		s.hosts[hostKey(u)] = true
	}
	for i, h := range s.AllowedHosts {
		k, err := normalizeHost(h)
		if err != nil {
			add("allowed_hosts[%d]: %v", i, err)
			continue
		}
		s.hosts[k] = true
	}
	for i, n := range s.AllowedNetworks {
		p, err := netip.ParsePrefix(n)
		if err != nil {
			add("allowed_networks[%d]: %v", i, err)
			continue
		}
		if p.Bits() == 0 {
			add("allowed_networks[%d]: %s would allow every address", i, n)
			continue
		}
		s.networks = append(s.networks, p.Masked())
	}
	for _, f := range []struct{ name, v string }{{"ca_file", s.CAFile}, {"auth_token_file", s.AuthTokenFile}} {
		if f.v != "" && !filepath.IsAbs(f.v) {
			add("%s must be an absolute path", f.name)
		}
	}
	if n := len(s.TrustedKeys); n == 0 || n > maxTrustedKeys {
		add("trusted_keys must list 1 to %d keys", maxTrustedKeys)
	}
	ids, pubs := map[string]bool{}, map[string]bool{}
	for i, k := range s.TrustedKeys {
		if !keyIDPattern.MatchString(k.ID) {
			add("trusted_keys[%d].id %q must match %s", i, k.ID, keyIDPattern)
		}
		if ids[k.ID] {
			add("trusted_keys[%d].id %q is repeated", i, k.ID)
		}
		ids[k.ID] = true
		pub, err := base64.StdEncoding.Strict().DecodeString(k.Ed25519PublicKey)
		if err != nil || len(pub) != 32 {
			add("trusted_keys[%d].ed25519_public_key must be 32 bytes in standard base64", i)
			continue
		}
		if pubs[string(pub)] {
			add("trusted_keys[%d] repeats a key", i)
		}
		pubs[string(pub)] = true
		s.keys = append(s.keys, key{id: k.ID, pub: pub, notAfter: k.NotAfter})
	}
	inRange := func(name string, d *Duration, def, lo, hi time.Duration) {
		if *d == 0 {
			if def == 0 {
				add("%s is required", name)
				return
			}
			*d = Duration(def)
		}
		if d.D() < lo || d.D() > hi {
			add("%s %s must be between %s and %s", name, d.D(), lo, hi)
		}
	}
	inRange("poll_interval", &s.PollInterval, 0, time.Second, 7*24*time.Hour)
	inRange("max_manifest_validity", &s.MaxManifestValidity, 0, time.Minute, 366*24*time.Hour)
	inRange("download_timeout", &s.DownloadTimeout, DefaultDownloadTimeout, 10*time.Second, 24*time.Hour)
	inRange("stall_timeout", &s.StallTimeout, DefaultStallTimeout, time.Second, time.Hour)
	inRange("retry_initial", &s.RetryInitial, DefaultRetryInitial, time.Second, 24*time.Hour)
	inRange("retry_max", &s.RetryMax, DefaultRetryMax, time.Second, 7*24*time.Hour)
	inRange("abandon_for", &s.AbandonFor, DefaultAbandonFor, time.Minute, 30*24*time.Hour)
	if s.RetryMax < s.RetryInitial {
		add("retry_max must not be shorter than retry_initial")
	}
	if s.MaxDownloadAttempts == 0 {
		s.MaxDownloadAttempts = DefaultMaxDownloadAttempts
	}
	if s.MaxDownloadAttempts < 1 || s.MaxDownloadAttempts > 100 {
		add("max_download_attempts must be 1..100")
	}
	return errors.Join(errs...)
}

// ManifestURLParsed returns the parsed manifest URL.
func (s *Source) ManifestURLParsed() *url.URL { u := *s.manifestURL; return &u }

// HostAllowed reports whether u's host (and port) may be contacted.
func (s *Source) HostAllowed(u *url.URL) bool { return s.hosts[hostKey(u)] }

// Networks returns the allowed non-public prefixes.
func (s *Source) Networks() []netip.Prefix { return append([]netip.Prefix(nil), s.networks...) }

// KeyIDs lists the trusted key ids and fingerprints (for status).
func (s *Source) KeyIDs() []KeyInfo {
	out := make([]KeyInfo, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, KeyInfo{ID: k.id, Fingerprint: Fingerprint(k.pub), NotAfter: k.notAfter})
	}
	return out
}

// KeyInfo describes a trusted key without the key itself.
type KeyInfo struct {
	ID          string     `json:"id"`
	Fingerprint string     `json:"fingerprint"`
	NotAfter    *time.Time `json:"not_after"`
}

// checkURL accepts an absolute https URL without credentials, query or
// fragment.
func checkURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("required")
	}
	if strings.ContainsAny(raw, " \t\r\n\\") || strings.IndexFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return nil, errors.New("contains whitespace, a backslash or a control character")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	switch {
	case u.Scheme != "https":
		return nil, fmt.Errorf("scheme %q is not https", u.Scheme)
	case u.Opaque != "" || u.Host == "":
		return nil, errors.New("not an absolute URL with a host")
	case u.User != nil:
		return nil, errors.New("credentials are not allowed in URLs (use auth_token_file)")
	case u.RawQuery != "" || u.ForceQuery:
		return nil, errors.New("a query is not allowed (credentials belong in auth_token_file)")
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return nil, errors.New("a fragment is not allowed")
	case u.Path == "" || u.Path == "/":
		return nil, errors.New("a path is required")
	}
	if _, err := normalizeHost(u.Host); err != nil {
		return nil, err
	}
	return u, nil
}

// normalizeHost returns "host:port" in lowercase with port 443 by default.
func normalizeHost(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(h))
	host, port := h, "443"
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[i:], "]") {
		host, port = h[:i], h[i+1:]
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("host %q has an invalid port", h)
	}
	if _, err := netip.ParseAddr(host); err != nil && !hostPattern.MatchString(host) {
		return "", fmt.Errorf("host %q is not a host name or address", h)
	}
	return net.JoinHostPort(host, port), nil
}

func hostKey(u *url.URL) string {
	k, err := normalizeHost(u.Host)
	if err != nil {
		return ""
	}
	return k
}

// readRegular reads a regular file (never through a symlink) of at most max
// bytes.
func readRegular(path string, max int64) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if st.Size() > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	b := make([]byte, 0, st.Size())
	buf := bytes.NewBuffer(b)
	if _, err := buf.ReadFrom(limitReader(f, max+1)); err != nil {
		return nil, err
	}
	if int64(buf.Len()) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	return buf.Bytes(), nil
}

// ReadToken reads the bearer token file: one line of printable ASCII.
func ReadToken(path string) (string, error) {
	b, err := readRegular(path, 4096)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" || strings.IndexFunc(t, func(r rune) bool { return r <= 0x20 || r >= 0x7f }) >= 0 {
		return "", errors.New("the token file must hold one token of printable ASCII characters")
	}
	return t, nil
}
