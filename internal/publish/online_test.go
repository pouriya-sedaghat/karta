package publish

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/importer"
	"github.com/pouriya-sedaghat/karta/internal/online"
	"github.com/pouriya-sedaghat/karta/internal/region"
)

const fixtureRegion = "../../config/regions/fixture.json"

func publishTestKey(name string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("karta publish unit test key " + name))
	return ed25519.NewKeyFromSeed(seed[:])
}

// writeSourceFile writes a source file for the fixture region trusting keys
// (id → key; a non-empty notAfter[id] sets that key's not_after).
func writeSourceFile(t *testing.T, path string, keys map[string]ed25519.PrivateKey, notAfter map[string]string, requireAuth bool, regionID string) {
	t.Helper()
	var ks []map[string]string
	for id, k := range keys {
		e := map[string]string{"id": id, "ed25519_public_key": base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey))}
		if na := notAfter[id]; na != "" {
			e["not_after"] = na
		}
		ks = append(ks, e)
	}
	b, _ := json.Marshal(map[string]any{"region_id": regionID, "manifest_url": "https://source.example/karta/manifest.json",
		"trusted_keys": ks, "poll_interval": "1h", "max_manifest_validity": "48h", "require_operator_authorization": requireAuth})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The activation gate verifies the delivery's signed manifest again, under
// the source and region files in force at the switch, and fails closed.
func TestReauthorizeOnlineAtActivation(t *testing.T) {
	ctx := context.Background()
	cfg, err := region.Load(fixtureRegion)
	if err != nil {
		t.Fatal(err)
	}
	k1, k2 := publishTestKey("k1"), publishTestKey("k2")
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	sign := func(digest string) *online.Verified {
		t.Helper()
		m := online.Manifest{Format: online.ManifestFormat, RegionID: cfg.ID, BBox: online.BBox(cfg.BBox), Serial: 7,
			IssuedAt: t0.Add(-time.Minute), ExpiresAt: t0.Add(time.Hour),
			Snapshot: online.Snapshot{URL: "snap.osm.pbf", SHA256: digest, SizeBytes: 1000, DataTimestamp: t0.Add(-2 * time.Hour)}}
		raw, err := online.Sign(m, online.Signer{KeyID: "k1", Key: k1})
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		writeSourceFile(t, filepath.Join(dir, "s.json"), map[string]ed25519.PrivateKey{"k1": k1}, nil, false, cfg.ID)
		src, err := online.LoadSource(filepath.Join(dir, "s.json"))
		if err != nil {
			t.Fatal(err)
		}
		v, err := online.Verify(raw, online.VerifyOptions{Source: src, Region: cfg, Now: t0, Skew: time.Minute, MaxSnapshotBytes: 1 << 30})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	unpinned := sign("1111111111111111111111111111111111111111111111111111111111111111")
	pinned := sign(cfg.Source.PinnedDigests()[0])

	type tc struct {
		name        string
		signed      *online.Verified
		keys        map[string]ed25519.PrivateKey
		notAfter    map[string]string
		requireAuth bool
		regionID    string
		regionPath  string
		noSource    bool
		at          time.Time
		authorized  string
		authErr     error
		want        string // reason code; "" means allowed
	}
	cases := []tc{
		{name: "still valid", want: ""},
		{name: "expired during the build", at: t0.Add(time.Hour), want: online.CodeManifestExpired},
		{name: "key removed", keys: map[string]ed25519.PrivateKey{"k2": k2}, want: online.CodeSignatureUntrusted},
		{name: "key retired by not_after", notAfter: map[string]string{"k1": t0.Add(time.Minute).Format(time.RFC3339)},
			at: t0.Add(2 * time.Minute), want: online.CodeSignatureUntrusted},
		{name: "rotation keeps the old key", keys: map[string]ed25519.PrivateKey{"k1": k1, "k2": k2}, want: ""},
		{name: "source file unreadable", noSource: true, want: online.CodeConfig},
		{name: "source now for another region", regionID: "elsewhere", want: online.CodeConfig},
		{name: "region file unreadable", regionPath: "does-not-exist.json", want: importer.CodeRegionConfig},
		{name: "operator authorization now required, none", requireAuth: true, want: importer.CodeUnauthorizedDigest},
		{name: "operator authorization now required, authorized", requireAuth: true, authorized: "operator authorization 3 by ops", want: ""},
		{name: "operator authorization now required, pinned", signed: pinned, requireAuth: true, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			srcPath := filepath.Join(dir, "source.json")
			if !c.noSource {
				keys := c.keys
				if keys == nil {
					keys = map[string]ed25519.PrivateKey{"k1": k1}
				}
				rid := c.regionID
				if rid == "" {
					rid = cfg.ID
				}
				writeSourceFile(t, srcPath, keys, c.notAfter, c.requireAuth, rid)
			}
			sv := c.signed
			if sv == nil {
				sv = unpinned
			}
			at := c.at
			if at.IsZero() {
				at = t0.Add(30 * time.Minute)
			}
			rp := c.regionPath
			if rp == "" {
				rp = fixtureRegion
			}
			calls := 0
			err := reauthorizeOnline(ctx, reauthorizeOptions{SourcePath: srcPath, RegionPath: rp, Signed: sv, Now: at, Skew: time.Minute,
				MaxInputBytes: 1 << 30, Authorized: func(_ context.Context, regionID, digest string, size int64) (string, error) {
					calls++
					if regionID != cfg.ID || digest != sv.Manifest.Snapshot.SHA256 || size != sv.Manifest.Snapshot.SizeBytes {
						t.Errorf("authorizer asked for %s %s %d", regionID, digest, size)
					}
					return c.authorized, c.authErr
				}})
			if got := importer.InputCode(err); got != c.want || (c.want == "" && err != nil) {
				t.Fatalf("code %q, want %q (%v)", got, c.want, err)
			}
			if c.signed == pinned && calls != 0 {
				t.Errorf("authorizer consulted for a pinned digest")
			}
		})
	}

	// A registry error refuses the switch too (not as a verdict on the input).
	dir := t.TempDir()
	writeSourceFile(t, filepath.Join(dir, "s.json"), map[string]ed25519.PrivateKey{"k1": k1}, nil, true, cfg.ID)
	boom := errors.New("registry unavailable")
	err = reauthorizeOnline(ctx, reauthorizeOptions{SourcePath: filepath.Join(dir, "s.json"), RegionPath: fixtureRegion, Signed: unpinned,
		Now: t0, Skew: time.Minute, MaxInputBytes: 1 << 30,
		Authorized: func(context.Context, string, string, int64) (string, error) { return "", boom }})
	if !errors.Is(err, boom) || importer.InputCode(err) != "" {
		t.Errorf("registry error: %v", err)
	}
}
