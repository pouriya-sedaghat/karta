package online

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/region"
)

// Manifest format and DSSE payload type.
const (
	ManifestFormat = "karta-snapshot-manifest/1"
	PayloadType    = "application/vnd.karta.snapshot-manifest+json; version=1"
)

// Limits on signed metadata.
const (
	MaxEnvelopeBytes   = 64 << 10
	MaxPayloadBytes    = 16 << 10
	MaxSignatures      = 8
	MaxProvenanceBytes = 1 << 20
)

// Error codes of the online source (recorded in fetcher state, submission
// reasons and audit records).
const (
	CodeConfig             = "source_config"
	CodeManifestInvalid    = "manifest_invalid"
	CodeManifestTooLarge   = "manifest_too_large"
	CodeSignatureInvalid   = "signature_invalid"
	CodeSignatureUntrusted = "signature_untrusted"
	CodeRegionMismatch     = "manifest_region_mismatch"
	CodeManifestExpired    = "manifest_expired"
	CodeManifestNotYet     = "manifest_not_yet_valid"
	CodeValidityTooLong    = "manifest_validity_too_long"
	CodeManifestReplayed   = "manifest_replayed"
	CodeManifestConflict   = "manifest_conflict"
	CodeManifestMissing    = "manifest_missing"
	CodeURLRefused         = "url_refused"
	CodeDestinationRefused = "destination_refused"
	CodeRedirectRefused    = "redirect_refused"
	CodeHTTPStatus         = "http_status"
	CodeTooLarge           = "too_large"
	CodeSizeMismatch       = "size_mismatch"
	CodeTruncated          = "truncated"
	CodeDigestMismatch     = "digest_mismatch"
	CodeTimeout            = "timeout"
	CodeStalled            = "stalled"
	CodeEncoding           = "unexpected_encoding"
	CodeStorage            = "insufficient_storage"
	CodeNetwork            = "network_error"
	CodeTLS                = "tls_error"
	CodeIO                 = "io_error"
	CodeAbandoned          = "download_abandoned"
)

// Error is a failure with a stable code. Messages never contain a token or
// a query string (URLs carry neither).
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

func errorf(code, format string, args ...any) error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// CodeOf returns the code of err, or "" if it has none.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Envelope is a DSSE envelope.
type Envelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     string      `json:"payload"`
	Signatures  []Signature `json:"signatures"`
}

// Signature is one DSSE signature.
type Signature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

// Manifest is the signed description of one snapshot.
type Manifest struct {
	Format    string     `json:"format"`
	RegionID  string     `json:"region_id"`
	BBox      BBox       `json:"bbox"`
	Serial    int64      `json:"serial"`
	IssuedAt  time.Time  `json:"issued_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	Snapshot  Snapshot   `json:"snapshot"`
	// Provenance is the optional provenance sidecar.
	Provenance *File `json:"provenance,omitempty"`
}

// BBox is west, south, east, north; exactly four numbers.
type BBox [4]float64

// UnmarshalJSON requires exactly four numbers (encoding/json would silently
// drop extra elements of a Go array).
func (b *BBox) UnmarshalJSON(data []byte) error {
	var v []float64
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	if len(v) != 4 {
		return fmt.Errorf("bbox must hold exactly 4 numbers, not %d", len(v))
	}
	copy(b[:], v)
	return nil
}

// Snapshot is the signed snapshot reference.
type Snapshot struct {
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	// DataTimestamp is the OSM data timestamp the importer must derive
	// from the snapshot (provenance source header or PBF header).
	DataTimestamp time.Time `json:"data_timestamp"`
}

// File is a signed reference to a small file.
type File struct {
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// PAE is DSSE's pre-authentication encoding: the bytes that are signed.
func PAE(payloadType string, payload []byte) []byte {
	b := []byte("DSSEv1 " + strconv.Itoa(len(payloadType)) + " " + payloadType + " " + strconv.Itoa(len(payload)) + " ")
	return append(b, payload...)
}

// Fingerprint identifies a public key: SHA-256 of the raw key, hex.
func Fingerprint(pub []byte) string {
	sum := sha256.Sum256(pub)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Signer signs manifests (the producer side; see cmd/karta-sign).
type Signer struct {
	KeyID string
	Key   ed25519.PrivateKey
}

// Sign encodes m and returns the envelope bytes, signed by every signer.
func Sign(m Manifest, signers ...Signer) ([]byte, error) {
	if len(signers) == 0 {
		return nil, errors.New("no signer")
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	env := Envelope{PayloadType: PayloadType, Payload: base64.StdEncoding.EncodeToString(payload)}
	for _, s := range signers {
		if !keyIDPattern.MatchString(s.KeyID) || len(s.Key) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("signer %q: invalid key id or key", s.KeyID)
		}
		env.Signatures = append(env.Signatures, Signature{KeyID: s.KeyID,
			Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(s.Key, PAE(PayloadType, payload)))})
	}
	return json.MarshalIndent(env, "", "  ")
}

// VerifyOptions configure manifest verification.
type VerifyOptions struct {
	Source *Source
	Region region.Config
	Now    time.Time
	// Skew is the clock difference tolerated for issued_at and data
	// timestamps in the future.
	Skew time.Duration
	// MaxSnapshotBytes bounds the signed snapshot size.
	MaxSnapshotBytes int64
}

// Verified is a manifest whose envelope verified.
type Verified struct {
	Manifest Manifest
	// KeyID is the trusted key that signed it (the first, if several did).
	KeyID          string
	KeyFingerprint string
	// EnvelopeSHA256 identifies the exact envelope bytes.
	EnvelopeSHA256 string
	Raw            []byte
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// minDataTimestamp: no OSM data predates the project.
var minDataTimestamp = time.Date(2004, 8, 9, 0, 0, 0, 0, time.UTC)

// Verify checks an envelope: strict structure and limits, a valid signature
// by a trusted key (and no invalid signature claiming a trusted key), and a
// manifest bound to the region, inside its validity window, with a sane
// snapshot reference. Serial ordering is checked by the caller against its
// persisted state.
func Verify(raw []byte, o VerifyOptions) (*Verified, error) {
	if len(raw) > MaxEnvelopeBytes {
		return nil, errorf(CodeManifestTooLarge, "the manifest envelope is larger than %d bytes", MaxEnvelopeBytes)
	}
	var env Envelope
	if err := strictDecode(raw, &env); err != nil {
		return nil, errorf(CodeManifestInvalid, "envelope: %v", err)
	}
	if env.PayloadType != PayloadType {
		return nil, errorf(CodeManifestInvalid, "payloadType %q is not %q", truncate(env.PayloadType, 80), PayloadType)
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(env.Payload)
	if err != nil {
		return nil, errorf(CodeManifestInvalid, "payload is not standard base64")
	}
	if len(payload) > MaxPayloadBytes {
		return nil, errorf(CodeManifestTooLarge, "the manifest payload is larger than %d bytes", MaxPayloadBytes)
	}
	if n := len(env.Signatures); n == 0 || n > MaxSignatures {
		return nil, errorf(CodeManifestInvalid, "the envelope must carry 1 to %d signatures", MaxSignatures)
	}
	msg := PAE(env.PayloadType, payload)
	var signedBy *key
	for i, sig := range env.Signatures {
		var k *key
		for j := range o.Source.keys {
			if o.Source.keys[j].id == sig.KeyID {
				k = &o.Source.keys[j]
			}
		}
		if k == nil {
			continue // not a trusted key (for example a key being introduced)
		}
		if k.notAfter != nil && o.Now.After(*k.notAfter) {
			continue // the key's validity ended
		}
		s, err := base64.StdEncoding.Strict().DecodeString(sig.Sig)
		if err != nil || len(s) != ed25519.SignatureSize || !ed25519.Verify(k.pub, msg, s) {
			return nil, errorf(CodeSignatureInvalid, "signature %d claims trusted key %q but does not verify", i, sig.KeyID)
		}
		if signedBy == nil {
			signedBy = k
		}
	}
	if signedBy == nil {
		return nil, errorf(CodeSignatureUntrusted, "no signature by a trusted, currently valid key")
	}
	var m Manifest
	if err := strictDecode(payload, &m); err != nil {
		return nil, errorf(CodeManifestInvalid, "manifest: %v", err)
	}
	if err := checkManifest(m, o); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return &Verified{Manifest: m, KeyID: signedBy.id, KeyFingerprint: Fingerprint(signedBy.pub),
		EnvelopeSHA256: hex.EncodeToString(sum[:]), Raw: raw}, nil
}

func checkManifest(m Manifest, o VerifyOptions) error {
	bad := func(format string, args ...any) error { return errorf(CodeManifestInvalid, format, args...) }
	switch {
	case m.Format != ManifestFormat:
		return bad("format %q is not %q", truncate(m.Format, 80), ManifestFormat)
	case m.RegionID != o.Region.ID:
		return errorf(CodeRegionMismatch, "manifest is for region %q, this deployment serves %q", truncate(m.RegionID, 80), o.Region.ID)
	case !region.SameBBox([4]float64(m.BBox), o.Region.BBox):
		return errorf(CodeRegionMismatch, "manifest box %v differs from region %q box %v", m.BBox, o.Region.ID, o.Region.BBox)
	case m.Serial < 1:
		return bad("serial must be a positive integer")
	case m.IssuedAt.IsZero() || m.ExpiresAt.IsZero():
		return bad("issued_at and expires_at are required")
	case !m.ExpiresAt.After(m.IssuedAt):
		return bad("expires_at must be after issued_at")
	case m.ExpiresAt.Sub(m.IssuedAt) > o.Source.MaxManifestValidity.D():
		return errorf(CodeValidityTooLong, "the manifest is valid for %s, longer than max_manifest_validity %s",
			m.ExpiresAt.Sub(m.IssuedAt), o.Source.MaxManifestValidity.D())
	case m.IssuedAt.After(o.Now.Add(o.Skew)):
		return errorf(CodeManifestNotYet, "issued_at %s is in the future", m.IssuedAt.UTC().Format(time.RFC3339))
	case !o.Now.Before(m.ExpiresAt):
		return errorf(CodeManifestExpired, "the manifest expired at %s", m.ExpiresAt.UTC().Format(time.RFC3339))
	}
	s := m.Snapshot
	switch {
	case !sha256Pattern.MatchString(s.SHA256):
		return bad("snapshot.sha256 must be 64 lowercase hex digits")
	case s.SizeBytes < 1:
		return bad("snapshot.size_bytes must be positive")
	case o.MaxSnapshotBytes > 0 && s.SizeBytes > o.MaxSnapshotBytes:
		return errorf(CodeTooLarge, "the snapshot is %d bytes, above the limit of %d", s.SizeBytes, o.MaxSnapshotBytes)
	case s.DataTimestamp.Before(minDataTimestamp):
		return bad("snapshot.data_timestamp %s is missing or before OpenStreetMap existed", s.DataTimestamp.UTC().Format(time.RFC3339))
	case s.DataTimestamp.After(m.IssuedAt.Add(o.Skew)):
		return errorf(CodeManifestConflict, "snapshot.data_timestamp %s is after the manifest was issued (%s)",
			s.DataTimestamp.UTC().Format(time.RFC3339), m.IssuedAt.UTC().Format(time.RFC3339))
	}
	if _, err := ResolveURL(o.Source, s.URL); err != nil {
		return err
	}
	if p := m.Provenance; p != nil {
		switch {
		case !sha256Pattern.MatchString(p.SHA256):
			return bad("provenance.sha256 must be 64 lowercase hex digits")
		case p.SizeBytes < 1 || p.SizeBytes > MaxProvenanceBytes:
			return bad("provenance.size_bytes must be 1..%d", MaxProvenanceBytes)
		}
		if _, err := ResolveURL(o.Source, p.URL); err != nil {
			return err
		}
	}
	return nil
}

// ResolveURL resolves a URL from a manifest against the manifest URL. The
// result must be https, on an allowed host, without credentials, query or
// fragment.
func ResolveURL(src *Source, ref string) (*url.URL, error) {
	if ref == "" || len(ref) > 2048 {
		return nil, errorf(CodeURLRefused, "a URL of 1 to 2048 characters is required")
	}
	r, err := url.Parse(ref)
	if err != nil {
		return nil, errorf(CodeURLRefused, "unparsable URL")
	}
	if r.User != nil || r.RawQuery != "" || r.ForceQuery || r.Fragment != "" {
		return nil, errorf(CodeURLRefused, "URLs must not carry credentials, a query or a fragment")
	}
	u := src.manifestURL.ResolveReference(r)
	if _, err := checkURL(u.String()); err != nil {
		return nil, errorf(CodeURLRefused, "%s: %v", redact(u), err)
	}
	if !src.HostAllowed(u) {
		return nil, errorf(CodeURLRefused, "host %s is not an allowed host of this source", u.Host)
	}
	return u, nil
}

// redact renders a URL for logs and status: scheme, host and path only.
func redact(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host + truncate(u.EscapedPath(), 200)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}
