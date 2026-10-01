package online

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/region"
)

var testNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func testKey(name string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("karta online unit test key " + name))
	return ed25519.NewKeyFromSeed(seed[:])
}

func pubB64(k ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey))
}

var testRegion = region.Config{ID: "fixture", Name: "Fixture", BBox: [4]float64{0, 0, 0.02, 0.015}}

// sourceJSON returns a valid source file for the test region trusting the
// given keys (by id).
func sourceJSON(manifestURL string, extra string, keys map[string]ed25519.PrivateKey) string {
	var ks []string
	for id, k := range keys {
		ks = append(ks, fmt.Sprintf(`{"id":%q,"ed25519_public_key":%q}`, id, pubB64(k)))
	}
	return fmt.Sprintf(`{"region_id":"fixture","manifest_url":%q,"trusted_keys":[%s],"poll_interval":"1h","max_manifest_validity":"168h"%s}`,
		manifestURL, strings.Join(ks, ","), extra)
}

func mustSource(t *testing.T, js string) *Source {
	t.Helper()
	s, err := ParseSource([]byte(js))
	if err != nil {
		t.Fatalf("source: %v\n%s", err, js)
	}
	return s
}

func testManifest() Manifest {
	return Manifest{Format: ManifestFormat, RegionID: "fixture", BBox: BBox{0, 0, 0.02, 0.015}, Serial: 5,
		IssuedAt: testNow.Add(-time.Hour), ExpiresAt: testNow.Add(24 * time.Hour),
		Snapshot: Snapshot{URL: "snap/fixture-b.osm.pbf", SHA256: strings.Repeat("ab", 32), SizeBytes: 2189,
			DataTimestamp: testNow.Add(-48 * time.Hour)},
		Provenance: &File{URL: "snap/fixture-b.osm.pbf.provenance.json", SHA256: strings.Repeat("cd", 32), SizeBytes: 100}}
}

func signed(t *testing.T, m Manifest, signers ...Signer) []byte {
	t.Helper()
	b, err := Sign(m, signers...)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// envelopeWith signs an arbitrary payload (for malformed manifests).
func envelopeWith(payload []byte, k ed25519.PrivateKey, id string) []byte {
	env := Envelope{PayloadType: PayloadType, Payload: base64.StdEncoding.EncodeToString(payload),
		Signatures: []Signature{{KeyID: id, Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(k, PAE(PayloadType, payload)))}}}
	b, _ := json.Marshal(env)
	return b
}

func TestPAEMatchesTheDSSESpecification(t *testing.T) {
	got := string(PAE("http://example.com/HelloWorld", []byte("hello world")))
	if want := "DSSEv1 29 http://example.com/HelloWorld 11 hello world"; got != want {
		t.Fatalf("PAE = %q, want %q", got, want)
	}
}

func TestVerifyAcceptsASignedManifest(t *testing.T) {
	a := testKey("a")
	src := mustSource(t, sourceJSON("https://source.test/karta/manifest.json", "", map[string]ed25519.PrivateKey{"a": a}))
	raw := signed(t, testManifest(), Signer{"a", a})
	v, err := Verify(raw, VerifyOptions{Source: src, Region: testRegion, Now: testNow, Skew: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if v.KeyID != "a" || v.Manifest.Serial != 5 || v.EnvelopeSHA256 != sha256Hex(raw) || v.KeyFingerprint != Fingerprint(a.Public().(ed25519.PublicKey)) {
		t.Errorf("verified %+v", v)
	}
	u, err := ResolveURL(src, v.Manifest.Snapshot.URL)
	if err != nil || u.String() != "https://source.test/karta/snap/fixture-b.osm.pbf" {
		t.Errorf("snapshot URL %v %v", u, err)
	}
}

func TestVerifyKeyRotation(t *testing.T) {
	old, next := testKey("old"), testKey("new")
	oldOnly := mustSource(t, sourceJSON("https://source.test/m.json", "", map[string]ed25519.PrivateKey{"old": old}))
	both := mustSource(t, sourceJSON("https://source.test/m.json", "", map[string]ed25519.PrivateKey{"old": old, "new": next}))
	newOnly := mustSource(t, sourceJSON("https://source.test/m.json", "", map[string]ed25519.PrivateKey{"new": next}))
	dual := signed(t, testManifest(), Signer{"old", old}, Signer{"new", next})
	newSigned := signed(t, testManifest(), Signer{"new", next})
	opts := func(s *Source) VerifyOptions {
		return VerifyOptions{Source: s, Region: testRegion, Now: testNow, Skew: time.Minute}
	}
	// During the overlap the producer signs with both keys; every
	// configuration step of the rotation accepts it.
	for name, s := range map[string]*Source{"old only": oldOnly, "both": both, "new only": newOnly} {
		if _, err := Verify(dual, opts(s)); err != nil {
			t.Errorf("dual-signed, %s trusted: %v", name, err)
		}
	}
	if _, err := Verify(newSigned, opts(oldOnly)); CodeOf(err) != CodeSignatureUntrusted {
		t.Errorf("new key not yet trusted: %v", err)
	}
	// A key past its not_after is no longer trusted.
	ended := testNow.Add(-time.Minute)
	expired := mustSource(t, fmt.Sprintf(`{"region_id":"fixture","manifest_url":"https://source.test/m.json","poll_interval":"1h",
		"max_manifest_validity":"168h","trusted_keys":[{"id":"old","ed25519_public_key":%q,"not_after":%q}]}`,
		pubB64(old), ended.Format(time.RFC3339)))
	if _, err := Verify(signed(t, testManifest(), Signer{"old", old}), opts(expired)); CodeOf(err) != CodeSignatureUntrusted {
		t.Errorf("expired key: %v", err)
	}
}

func TestVerifyRejects(t *testing.T) {
	a, b := testKey("a"), testKey("b")
	src := mustSource(t, sourceJSON("https://source.test/karta/manifest.json", `,"allowed_hosts":["mirror.test"]`, map[string]ed25519.PrivateKey{"a": a}))
	good := testManifest()
	edit := func(f func(*Manifest)) []byte {
		m := good
		p := *good.Provenance
		m.Provenance = &p
		f(&m)
		return signed(t, m, Signer{"a", a})
	}
	goodPayload, _ := json.Marshal(good)
	var env Envelope
	_ = json.Unmarshal(signed(t, good, Signer{"a", a}), &env)
	tampered := env
	tampered.Payload = base64.StdEncoding.EncodeToString([]byte(strings.Replace(string(goodPayload), `"serial":5`, `"serial":6`, 1)))
	tamperedRaw, _ := json.Marshal(tampered)
	badSigOnTrusted := env
	badSigOnTrusted.Signatures = append([]Signature{{KeyID: "a", Sig: base64.StdEncoding.EncodeToString(make([]byte, 64))}}, env.Signatures...)
	badSigRaw, _ := json.Marshal(badSigOnTrusted)
	wrongType := env
	wrongType.PayloadType = "application/json"
	wrongTypeRaw, _ := json.Marshal(wrongType)
	urlB64 := env
	urlB64.Payload = base64.URLEncoding.EncodeToString(goodPayload) + "%"
	urlB64Raw, _ := json.Marshal(urlB64)
	many := env
	for i := 0; i < MaxSignatures; i++ {
		many.Signatures = append(many.Signatures, Signature{KeyID: "x", Sig: "AA=="})
	}
	manyRaw, _ := json.Marshal(many)

	cases := []struct {
		name string
		raw  []byte
		code string
	}{
		{"tampered payload", tamperedRaw, CodeSignatureInvalid},
		{"signed by an untrusted key", signed(t, good, Signer{"b", b}), CodeSignatureUntrusted},
		{"untrusted key using a trusted id", signed(t, good, Signer{"a", b}), CodeSignatureInvalid},
		{"an invalid signature claiming a trusted key beside a valid one", badSigRaw, CodeSignatureInvalid},
		{"wrong payload type", wrongTypeRaw, CodeManifestInvalid},
		{"payload not standard base64", urlB64Raw, CodeManifestInvalid},
		{"too many signatures", manyRaw, CodeManifestInvalid},
		{"oversized envelope", append(signed(t, good, Signer{"a", a}), []byte(strings.Repeat(" ", MaxEnvelopeBytes))...), CodeManifestTooLarge},
		{"not JSON", []byte("not json"), CodeManifestInvalid},
		{"trailing data", append(signed(t, good, Signer{"a", a}), []byte("{}")...), CodeManifestInvalid},
		{"repeated envelope key", []byte(`{"payloadType":"x","payloadType":"y","payload":"","signatures":[]}`), CodeManifestInvalid},
		{"repeated manifest key", envelopeWith([]byte(strings.Replace(string(goodPayload), `"serial":5`, `"serial":5,"serial":6`, 1)), a, "a"), CodeManifestInvalid},
		{"unknown manifest field", envelopeWith([]byte(strings.Replace(string(goodPayload), `"serial":5`, `"serial":5,"extra":1`, 1)), a, "a"), CodeManifestInvalid},
		{"five-number box", envelopeWith([]byte(strings.Replace(string(goodPayload), `"bbox":[0,0,0.02,0.015]`, `"bbox":[0,0,0.02,0.015,9]`, 1)), a, "a"), CodeManifestInvalid},
		{"wrong format", edit(func(m *Manifest) { m.Format = "karta-snapshot-manifest/2" }), CodeManifestInvalid},
		{"another region", edit(func(m *Manifest) { m.RegionID = "tehran-chitgar" }), CodeRegionMismatch},
		{"another box", edit(func(m *Manifest) { m.BBox[2] = 0.03 }), CodeRegionMismatch},
		{"serial zero", edit(func(m *Manifest) { m.Serial = 0 }), CodeManifestInvalid},
		{"expired", edit(func(m *Manifest) { m.ExpiresAt = testNow }), CodeManifestExpired},
		{"issued in the future", edit(func(m *Manifest) { m.IssuedAt = testNow.Add(time.Hour) }), CodeManifestNotYet},
		{"valid for too long", edit(func(m *Manifest) { m.ExpiresAt = m.IssuedAt.Add(169 * time.Hour) }), CodeValidityTooLong},
		{"expires before issued", edit(func(m *Manifest) { m.ExpiresAt = m.IssuedAt.Add(-time.Second) }), CodeManifestInvalid},
		{"data newer than the manifest", edit(func(m *Manifest) { m.Snapshot.DataTimestamp = m.IssuedAt.Add(time.Hour) }), CodeManifestConflict},
		{"data before OSM existed", edit(func(m *Manifest) { m.Snapshot.DataTimestamp = time.Date(2003, 1, 1, 0, 0, 0, 0, time.UTC) }), CodeManifestInvalid},
		{"uppercase digest", edit(func(m *Manifest) { m.Snapshot.SHA256 = strings.ToUpper(m.Snapshot.SHA256) }), CodeManifestInvalid},
		{"empty snapshot", edit(func(m *Manifest) { m.Snapshot.SizeBytes = 0 }), CodeManifestInvalid},
		{"snapshot over the input limit", edit(func(m *Manifest) { m.Snapshot.SizeBytes = 1 << 33 }), CodeTooLarge},
		{"http snapshot URL", edit(func(m *Manifest) { m.Snapshot.URL = "http://source.test/s.osm.pbf" }), CodeURLRefused},
		{"snapshot on another host", edit(func(m *Manifest) { m.Snapshot.URL = "https://evil.test/s.osm.pbf" }), CodeURLRefused},
		{"snapshot on another port", edit(func(m *Manifest) { m.Snapshot.URL = "https://source.test:8443/s.osm.pbf" }), CodeURLRefused},
		{"credentials in the URL", edit(func(m *Manifest) { m.Snapshot.URL = "https://user:pw@source.test/s.osm.pbf" }), CodeURLRefused},
		{"query in the URL", edit(func(m *Manifest) { m.Snapshot.URL = "s.osm.pbf?token=x" }), CodeURLRefused},
		{"fragment in the URL", edit(func(m *Manifest) { m.Snapshot.URL = "s.osm.pbf#x" }), CodeURLRefused},
		{"scheme-relative URL to another host", edit(func(m *Manifest) { m.Snapshot.URL = "//evil.test/s.osm.pbf" }), CodeURLRefused},
		{"file URL", edit(func(m *Manifest) { m.Snapshot.URL = "file:///etc/passwd" }), CodeURLRefused},
		{"provenance too large", edit(func(m *Manifest) { m.Provenance.SizeBytes = MaxProvenanceBytes + 1 }), CodeManifestInvalid},
		{"provenance on another host", edit(func(m *Manifest) { m.Provenance.URL = "https://evil.test/p.json" }), CodeURLRefused},
	}
	for _, c := range cases {
		_, err := Verify(c.raw, VerifyOptions{Source: src, Region: testRegion, Now: testNow, Skew: time.Minute, MaxSnapshotBytes: 1 << 32})
		if CodeOf(err) != c.code {
			t.Errorf("%s: got %v, want code %s", c.name, err, c.code)
		}
	}
	// An allowed second host is accepted.
	if _, err := Verify(edit(func(m *Manifest) { m.Snapshot.URL = "https://mirror.test/s.osm.pbf" }),
		VerifyOptions{Source: src, Region: testRegion, Now: testNow, Skew: time.Minute}); err != nil {
		t.Errorf("allowed host: %v", err)
	}
}

func TestParseSource(t *testing.T) {
	a := testKey("a")
	ok := sourceJSON("https://source.test/m.json", "", map[string]ed25519.PrivateKey{"a": a})
	s := mustSource(t, ok)
	if s.DownloadTimeout.D() != DefaultDownloadTimeout || s.StallTimeout.D() != DefaultStallTimeout || s.MaxDownloadAttempts != DefaultMaxDownloadAttempts ||
		s.RetryInitial.D() != DefaultRetryInitial || s.RetryMax.D() != DefaultRetryMax || s.AbandonFor.D() != DefaultAbandonFor {
		t.Errorf("defaults not applied: %+v", s)
	}
	key := fmt.Sprintf(`{"id":"a","ed25519_public_key":%q}`, pubB64(a))
	base := func(fields string) string {
		return `{"region_id":"fixture","poll_interval":"1h","max_manifest_validity":"168h",` + fields + `}`
	}
	for name, js := range map[string]string{
		"unknown field":         base(`"manifest_url":"https://s.test/m","trusted_keys":[` + key + `],"pol_interval":"1h"`),
		"repeated key":          base(`"manifest_url":"https://s.test/m","manifest_url":"https://t.test/m","trusted_keys":[` + key + `]`),
		"http":                  base(`"manifest_url":"http://s.test/m","trusted_keys":[` + key + `]`),
		"query":                 base(`"manifest_url":"https://s.test/m?token=1","trusted_keys":[` + key + `]`),
		"credentials":           base(`"manifest_url":"https://u:p@s.test/m","trusted_keys":[` + key + `]`),
		"fragment":              base(`"manifest_url":"https://s.test/m#f","trusted_keys":[` + key + `]`),
		"no path":               base(`"manifest_url":"https://s.test","trusted_keys":[` + key + `]`),
		"bad host":              base(`"manifest_url":"https://s_t.test/m","trusted_keys":[` + key + `]`),
		"no keys":               base(`"manifest_url":"https://s.test/m","trusted_keys":[]`),
		"short key":             base(`"manifest_url":"https://s.test/m","trusted_keys":[{"id":"a","ed25519_public_key":"AAAA"}]`),
		"repeated key id":       base(`"manifest_url":"https://s.test/m","trusted_keys":[` + key + `,` + key + `]`),
		"bad key id":            base(`"manifest_url":"https://s.test/m","trusted_keys":[{"id":"a b","ed25519_public_key":` + fmt.Sprintf("%q", pubB64(a)) + `}]`),
		"every address allowed": base(`"manifest_url":"https://s.test/m","trusted_keys":[` + key + `],"allowed_networks":["0.0.0.0/0"]`),
		"bad network":           base(`"manifest_url":"https://s.test/m","trusted_keys":[` + key + `],"allowed_networks":["10.0.0.0/33"]`),
		"relative ca file":      base(`"manifest_url":"https://s.test/m","trusted_keys":[` + key + `],"ca_file":"ca.pem"`),
		"retry max below initial": base(`"manifest_url":"https://s.test/m","trusted_keys":[` + key +
			`],"retry_initial":"10m","retry_max":"1m"`),
		"no poll interval":      `{"region_id":"fixture","max_manifest_validity":"1h","manifest_url":"https://s.test/m","trusted_keys":[` + key + `]}`,
		"no validity bound":     `{"region_id":"fixture","poll_interval":"1h","manifest_url":"https://s.test/m","trusted_keys":[` + key + `]}`,
		"bad region":            `{"region_id":"Fixture!","poll_interval":"1h","max_manifest_validity":"1h","manifest_url":"https://s.test/m","trusted_keys":[` + key + `]}`,
		"too many attempts":     base(`"manifest_url":"https://s.test/m","trusted_keys":[` + key + `],"max_download_attempts":1000`),
		"stall timeout too low": base(`"manifest_url":"https://s.test/m","trusted_keys":[` + key + `],"stall_timeout":"10ms"`),
	} {
		if _, err := ParseSource([]byte(js)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	h := mustSource(t, sourceJSON("https://Source.Test/m.json", `,"allowed_hosts":["mirror.test:8443","[2001:db8::1]"]`, map[string]ed25519.PrivateKey{"a": a}))
	for u, want := range map[string]bool{
		"https://source.test/x":        true,
		"https://source.test:443/x":    true,
		"https://source.test:8443/x":   false,
		"https://mirror.test:8443/x":   true,
		"https://mirror.test/x":        false,
		"https://[2001:db8::1]/x":      true,
		"https://[2001:db8::1]:8443/x": false,
		"https://other.test/x":         false,
	} {
		got, err := ResolveURL(h, u)
		if (err == nil) != want {
			t.Errorf("%s: %v %v", u, got, err)
		}
	}
}
