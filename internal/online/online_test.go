package online

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func signed(t *testing.T, key ed25519.PrivateKey, m Manifest) []byte {
	t.Helper()
	p, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(envelope{Payload: base64.StdEncoding.EncodeToString(p),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, p))})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixture(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, Manifest) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	b := []byte("complete snapshot")
	sum := sha256.Sum256(b)
	return pub, priv, Manifest{RegionID: "fixture", IssuedAt: now, ExpiresAt: now.Add(time.Hour),
		DataTimestamp: now.Add(-time.Hour), SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(b)),
		SnapshotURL: "https://source.example/snapshot.osm.pbf"}
}

func TestSignedManifestPolicy(t *testing.T) {
	pub, priv, m := fixture(t)
	origin, _ := url.Parse("https://source.example/manifest.json")
	check := func(m Manifest, key ed25519.PrivateKey, want bool) {
		t.Helper()
		_, err := Parse(signed(t, key, m), pub, origin, "fixture", 1024, time.Now())
		if (err == nil) != want {
			t.Fatalf("valid=%v error=%v", want, err)
		}
	}
	check(m, priv, true)
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	check(m, wrong, false)
	bad := m
	bad.RegionID = "other"
	check(bad, priv, false)
	bad = m
	bad.ExpiresAt = time.Now().Add(-time.Second)
	check(bad, priv, false)
	bad = m
	bad.SizeBytes = 1025
	check(bad, priv, false)
	bad = m
	bad.SnapshotURL = "http://source.example/data"
	check(bad, priv, false)
	bad = m
	bad.SnapshotURL = "https://other.example/data"
	check(bad, priv, false)
	bad = m
	bad.SnapshotURL = "https://source.example/data?token=secret"
	check(bad, priv, false)
	bad = m
	bad.ProvenanceURL = "https://source.example/prov"
	check(bad, priv, false)
	p, _ := json.Marshal(m)
	dup := append(append([]byte{}, p[:len(p)-1]...), []byte(`,"region_id":"fixture"}`)...)
	e := envelope{Payload: base64.StdEncoding.EncodeToString(dup), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, dup))}
	b, _ := json.Marshal(e)
	if _, err := Parse(b, pub, origin, "fixture", 1024, time.Now()); err == nil {
		t.Fatal("duplicate signed claim accepted")
	}
}

func TestBoundedDownload(t *testing.T) {
	pub, priv, m := fixture(t)
	snapshot := []byte("complete snapshot")
	var mode atomic.Value
	mode.Store("")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest":
			if mode.Load().(string) == "oversized-manifest" {
				_, _ = io.WriteString(w, strings.Repeat("x", MaxManifestBytes+1))
				return
			}
			_, _ = w.Write(signed(t, priv, m))
		case "/snapshot":
			switch mode.Load().(string) {
			case "redirect":
				http.Redirect(w, r, "https://127.0.0.1/private", http.StatusFound)
			case "short":
				_, _ = w.Write(snapshot[:3])
			case "oversize":
				_, _ = w.Write(append(snapshot, 'x'))
			case "slow":
				<-r.Context().Done()
			default:
				_, _ = w.Write(snapshot)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	m.SnapshotURL = server.URL + "/snapshot"
	c := Config{ManifestURL: server.URL + "/manifest", PublicKey: pub, RegionID: "fixture",
		MaxBytes: 1024, Timeout: time.Second, Client: server.Client()}
	c.Client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if _, err := FetchManifest(context.Background(), c, time.Now()); err != nil {
		t.Fatal(err)
	}
	stageDir := t.TempDir()
	st, err := Download(context.Background(), c, m, stageDir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(st.SnapshotPath)
	if err != nil || string(b) != string(snapshot) {
		t.Fatalf("staged bytes: %q %v", b, err)
	}
	_ = os.RemoveAll(st.Dir)
	for _, test := range []string{"short", "oversize", "redirect"} {
		mode.Store(test)
		if _, err := Download(context.Background(), c, m, stageDir); err == nil {
			t.Fatalf("%s accepted", test)
		}
		entries, _ := os.ReadDir(stageDir)
		if len(entries) != 0 {
			t.Fatalf("partial files after %s: %d", test, len(entries))
		}
	}
	mode.Store("oversized-manifest")
	if _, err := FetchManifest(context.Background(), c, time.Now()); err == nil {
		t.Fatal("oversized manifest accepted")
	}
	mode.Store("slow")
	slowCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := Download(slowCtx, c, m, stageDir); err == nil {
		t.Fatal("timed-out transfer accepted")
	}
}

func TestDestinationPolicy(t *testing.T) {
	for _, raw := range []string{"http://example.com/a", "https://user:pass@example.com/a", "https://example.com/a?key=secret",
		"https://example.com/a#fragment", "https://example.com", "https://example.com:bad/a"} {
		if _, err := ValidateURL(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "0.0.0.1", "100.64.0.1", "192.88.99.1", "198.18.0.1", "::1", "fe80::1", "64:ff9b::7f00:1", "64:ff9b:1::1", "2001:db8::1"} {
		if publicIP(net.ParseIP(ip)) {
			t.Fatalf("accepted private address %s", ip)
		}
	}
	u, _ := ValidateURL("https://127.0.0.1/snapshot")
	client := safeClient(u, time.Second)
	if _, err := client.Get(u.String()); err == nil {
		t.Fatal("production client dialed loopback")
	}
}
