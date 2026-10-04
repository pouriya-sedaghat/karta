// Package bridge is Karta's controlled source bridge (Stage 5): it checks a
// distributor (Geofabrik) for a newer complete snapshot, downloads it within
// strict bounds, verifies it with Karta's own input checks, signs a
// Karta-compatible manifest for the exact bytes, and serves the manifest and
// a content-addressed copy to Karta's existing fetcher. It runs as three
// processes of one binary:
//
//	acquire  the only one with a route to the distributor; no key
//	sign     no network; the only holder of the signing key; it re-reads
//	         the bytes it signs and runs the full PBF, region and timestamp
//	         checks itself
//	serve    read-only HTTPS of the publish directory; no key, no egress
//
// A valid bridge signature means "these bytes passed the bridge's policy";
// it is not the distributor's signature and not proof that OSM facts are
// correct. See docs/adr/0006-stage5-hybrid-intake.md.
package bridge

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/online"
	"github.com/pouriya-sedaghat/karta/internal/safefile"
)

// Source is the acquire process's reviewed configuration: where the
// distributor publishes the snapshot, and how often and how carefully to ask.
type Source struct {
	RegionID string `json:"region_id"`
	// SnapshotURL is the distributor's moving name of the newest complete
	// snapshot (for Geofabrik, .../asia/iran-latest.osm.pbf); it is never
	// what a manifest names (the bridge's own content-addressed copy is).
	SnapshotURL string `json:"snapshot_url"`
	// MD5URL is the distributor's co-hosted checksum (optional): a hint and
	// a transfer-accident check, never an authorization.
	MD5URL string `json:"md5_url,omitempty"`
	// Distributor is recorded in provenance.
	Distributor string `json:"distributor"`
	// UserAgent identifies the bridge and its operator to the distributor.
	UserAgent string `json:"user_agent"`
	// AllowedNetworks lets the bridge connect to non-public addresses (a
	// private mirror, or the test stand-in).
	AllowedNetworks []string `json:"allowed_networks,omitempty"`
	// CAFile, when set, holds the only trusted roots for the distributor.
	CAFile string `json:"ca_file,omitempty"`
	// PollInterval is the time between checks (hints only).
	PollInterval online.Duration `json:"poll_interval"`
	// ReverifyInterval forces a full download even when every hint is
	// unchanged, so missing or stale hints cannot hide a change for long.
	ReverifyInterval online.Duration `json:"reverify_interval"`
	DownloadTimeout  online.Duration `json:"download_timeout,omitempty"`
	StallTimeout     online.Duration `json:"stall_timeout,omitempty"`
	RetryInitial     online.Duration `json:"retry_initial,omitempty"`
	RetryMax         online.Duration `json:"retry_max,omitempty"`

	snapshotURL *url.URL
	md5URL      *url.URL
	networks    []netip.Prefix
}

var regionIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Defaults of a source (conservative initial values, ADR 0006).
const (
	DefaultDownloadTimeout = 2 * time.Hour
	DefaultStallTimeout    = 2 * time.Minute
	DefaultRetryInitial    = 15 * time.Minute
	DefaultRetryMax        = 6 * time.Hour
	maxConfigFile          = 64 << 10
)

// LoadSource reads and validates a bridge source file (strict JSON).
func LoadSource(path string) (*Source, error) {
	b, err := safefile.ReadRegular(path, maxConfigFile)
	if err != nil {
		return nil, err
	}
	var s Source
	if err := safefile.StrictDecode(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

func checkHTTPS(raw string) (*url.URL, error) {
	if strings.ContainsAny(raw, " \t\r\n\\#?") {
		return nil, errors.New("must not contain whitespace, a backslash, a query or a fragment")
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, err
	case u.Scheme != "https" || u.Host == "" || u.Opaque != "":
		return nil, errors.New("must be an absolute https URL")
	case u.User != nil:
		return nil, errors.New("must not carry credentials")
	case u.Path == "" || u.Path == "/":
		return nil, errors.New("needs a path")
	}
	return u, nil
}

func (s *Source) validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !regionIDPattern.MatchString(s.RegionID) {
		add("region_id %q must match %s", s.RegionID, regionIDPattern)
	}
	u, err := checkHTTPS(s.SnapshotURL)
	if err != nil {
		add("snapshot_url: %v", err)
	} else if !strings.HasSuffix(u.Path, ".osm.pbf") {
		add("snapshot_url must name a .osm.pbf file")
	}
	s.snapshotURL = u
	if s.MD5URL != "" {
		m, err := checkHTTPS(s.MD5URL)
		if err != nil {
			add("md5_url: %v", err)
		} else if u != nil && !strings.EqualFold(m.Host, u.Host) {
			add("md5_url must be on the snapshot's host (%s)", u.Host)
		}
		s.md5URL = m
	}
	if strings.TrimSpace(s.Distributor) == "" || len(s.Distributor) > 200 {
		add("distributor is required (at most 200 characters)")
	}
	if len(s.UserAgent) < 8 || len(s.UserAgent) > 200 || strings.ContainsAny(s.UserAgent, "\r\n") {
		add("user_agent is required: a descriptive name with a contact (8 to 200 characters)")
	}
	for i, n := range s.AllowedNetworks {
		p, err := netip.ParsePrefix(n)
		if err != nil || p.Bits() == 0 {
			add("allowed_networks[%d]: %q is not a narrow CIDR prefix", i, n)
			continue
		}
		s.networks = append(s.networks, p.Masked())
	}
	if s.CAFile != "" && !filepath.IsAbs(s.CAFile) {
		add("ca_file must be an absolute path")
	}
	inRange := func(name string, d *online.Duration, def, lo, hi time.Duration) {
		if *d == 0 {
			if def == 0 {
				add("%s is required", name)
				return
			}
			*d = online.Duration(def)
		}
		if d.D() < lo || d.D() > hi {
			add("%s %s must be between %s and %s", name, d.D(), lo, hi)
		}
	}
	inRange("poll_interval", &s.PollInterval, 0, time.Second, 7*24*time.Hour)
	inRange("reverify_interval", &s.ReverifyInterval, 0, time.Minute, 90*24*time.Hour)
	inRange("download_timeout", &s.DownloadTimeout, DefaultDownloadTimeout, 10*time.Second, 24*time.Hour)
	inRange("stall_timeout", &s.StallTimeout, DefaultStallTimeout, time.Second, time.Hour)
	inRange("retry_initial", &s.RetryInitial, DefaultRetryInitial, time.Second, 24*time.Hour)
	inRange("retry_max", &s.RetryMax, DefaultRetryMax, time.Second, 7*24*time.Hour)
	if s.RetryMax < s.RetryInitial {
		add("retry_max must not be shorter than retry_initial")
	}
	return errors.Join(errs...)
}

// redact renders a URL for status and logs.
func redact(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

// SignerConfig is the sign process's reviewed configuration.
type SignerConfig struct {
	// BridgeID names this bridge in provenance.
	BridgeID string `json:"bridge_id"`
	// Keys sign every manifest (several during a rotation).
	Keys []KeyFile `json:"keys"`
	// ManifestValidity is expires_at - issued_at of each manifest; it must
	// not exceed the fetcher's max_manifest_validity.
	ManifestValidity online.Duration `json:"manifest_validity"`
	// RenewBefore re-signs the held snapshot under a new serial when less
	// than this much validity remains (default: half the validity).
	RenewBefore online.Duration `json:"renew_before,omitempty"`
}

// KeyFile is one signing key: its key id in signatures and its PKCS#8 file.
type KeyFile struct {
	ID   string `json:"id"`
	File string `json:"file"`
}

var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// LoadSignerConfig reads and validates a signer configuration (strict JSON).
func LoadSignerConfig(path string) (*SignerConfig, error) {
	b, err := safefile.ReadRegular(path, maxConfigFile)
	if err != nil {
		return nil, err
	}
	var c SignerConfig
	if err := safefile.StrictDecode(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var errs []error
	if !keyIDPattern.MatchString(c.BridgeID) {
		errs = append(errs, fmt.Errorf("bridge_id %q must match %s", c.BridgeID, keyIDPattern))
	}
	if n := len(c.Keys); n == 0 || n > 4 {
		errs = append(errs, errors.New("keys must list 1 to 4 signing keys"))
	}
	ids := map[string]bool{}
	for i, k := range c.Keys {
		if !keyIDPattern.MatchString(k.ID) || ids[k.ID] {
			errs = append(errs, fmt.Errorf("keys[%d].id %q is invalid or repeated", i, k.ID))
		}
		ids[k.ID] = true
		if !filepath.IsAbs(k.File) {
			errs = append(errs, fmt.Errorf("keys[%d].file must be an absolute path", i))
		}
	}
	v := c.ManifestValidity.D()
	if v < time.Minute || v > 366*24*time.Hour {
		errs = append(errs, errors.New("manifest_validity must be between 1m and 366d"))
	}
	if c.RenewBefore == 0 {
		c.RenewBefore = online.Duration(v / 2)
	}
	if c.RenewBefore.D() <= 0 || c.RenewBefore.D() >= v {
		errs = append(errs, errors.New("renew_before must be shorter than manifest_validity"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}
