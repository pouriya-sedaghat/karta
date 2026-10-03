//go:build integration

// Stage 5 acceptance tests, controlled source bridge, co-located topology:
// compose.bridge.yaml with tests/integration/compose.bridge-test.yaml. The
// controlled test source (sourceserver) stands in for the distributor and
// serves /asia/iran-latest.osm.pbf and its .md5 from committed fixtures and
// variants generated here; the bridge's signing key, its TLS CA and the
// test source's CA are generated for the run. Nothing is downloaded from a
// live external host: acquire sits on the internal `sourcenet` only.
package integration

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/md5" // #nosec G501 -- the distributor's checksum format, in the stand-in
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/bridge"
	"github.com/pouriya-sedaghat/karta/internal/online"
)

var (
	bridgeDir         = os.Getenv("KARTA_TEST_BRIDGE_DIR")
	bridgeMetricsBase = "http://127.0.0.1:" + env("KARTA_TEST_BRIDGE_METRICS_PORT", "18465")
)

const bridgeSourceFile = "/config/sources/bridge.json"

func bridgeCompose(t *testing.T, env []string, args ...string) (string, string, int) {
	t.Helper()
	return composeEnv(t, append([]string{"KARTA_TEST_ONLINE_SOURCE_FILE=" + bridgeSourceFile}, env...),
		append([]string{"-f", "compose.bridge.yaml", "-f", "tests/integration/compose.bridge-test.yaml"}, args...)...)
}

func bridgeUp(t *testing.T, services ...string) {
	t.Helper()
	args := append([]string{"--profile", "bridge", "--profile", "online", "up", "-d", "--no-deps", "--force-recreate"}, services...)
	if _, stderr, code := bridgeCompose(t, nil, args...); code != 0 {
		t.Fatalf("bridge up %v: %s", services, stderr)
	}
}

func bridgeContainer(t *testing.T, svc string) string {
	t.Helper()
	out, _, code := bridgeCompose(t, nil, "--profile", "bridge", "--profile", "online", "ps", "-a", "-q", svc)
	if code != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("no %s container", svc)
	}
	return strings.TrimSpace(out)
}

func inspect(t *testing.T, id, format string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", format, id).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", id, err)
	}
	return strings.TrimSpace(string(out))
}

func networksOf(t *testing.T, svc string) string {
	return inspect(t, bridgeContainer(t, svc), "{{range $n, $_ := .NetworkSettings.Networks}}{{$n}} {{end}}")
}

// distribute makes the stand-in serve data as iran-latest, with its .md5.
func distribute(t *testing.T, data []byte) {
	t.Helper()
	sum := md5.Sum(data) // #nosec G401 -- the distributor's checksum format
	writeFileAtomic(t, filepath.Join(sourceDocroot, "asia", "iran-latest.osm.pbf"), data, 0o644)
	writeFileAtomic(t, filepath.Join(sourceDocroot, "asia", "iran-latest.osm.pbf.md5"),
		[]byte(hex.EncodeToString(sum[:])+"  iran-latest.osm.pbf\n"), 0o644)
}

func signerStatus(t *testing.T) *bridge.SignerState {
	t.Helper()
	s, err := bridge.ReadSignerStatus(filepath.Join(bridgeDir, "publish"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func waitSigner(t *testing.T, what string, timeout time.Duration, ok func(*bridge.SignerState) bool) *bridge.SignerState {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *bridge.SignerState
	for time.Now().Before(deadline) {
		if s, err := bridge.ReadSignerStatus(filepath.Join(bridgeDir, "publish")); err == nil && s != nil {
			last = s
			if ok(s) {
				return s
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	b, _ := json.MarshalIndent(last, "", "  ")
	logs, _, _ := bridgeCompose(t, nil, "--profile", "bridge", "logs", "--no-color", "--tail=30", "bridge-acquire", "bridge-sign")
	t.Fatalf("signer: %s not reached within %s; last %s\n%s", what, timeout, b, logs)
	return nil
}

func waitOnlineDigest(t *testing.T, digest string, timeout time.Duration) submission {
	t.Helper()
	sub, src := waitDigest(t, digest, timeout)
	if src != "online" {
		t.Errorf("submission %+v came from %s, not online", sub, src)
	}
	return sub
}

// writeBridgeTLS creates a CA for the run and bridge-serve's certificate.
func writeBridgeTLS(t *testing.T) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Karta bridge test CA (test only)"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	srvKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srvTpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "bridge-serve"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"bridge-serve"}}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTpl, ca, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(srvKey)
	writeFileAtomic(t, filepath.Join(bridgeDir, "tls", "tls.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}), 0o644)
	writeFileAtomic(t, filepath.Join(bridgeDir, "tls", "tls-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o644)
	writeFileAtomic(t, filepath.Join(sourceCfgDir, "bridge-ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644)
}

func writeJSON(t *testing.T, p string, v any) {
	t.Helper()
	b, _ := json.MarshalIndent(v, "", "  ")
	writeFileAtomic(t, p, b, 0o644)
}

func TestBridge(t *testing.T) {
	if bridgeDir == "" || sourceCfgDir == "" || sourceDocroot == "" {
		t.Fatal("the Stage 5 bridge directories are not set; run `make test-integration`")
	}
	monitor := secret(t, "operator_monitor_token")
	resetAll(t)
	resetOutbox(t)
	cleanDir(t, bridgeDir)
	t.Cleanup(func() {
		bridgeCompose(t, nil, "--profile", "bridge", "--profile", "online", "rm", "-s", "-f", "bridge-acquire", "bridge-sign", "bridge-serve", "fetcher")
		compose(t, "--profile", "online", "stop", "-t", "2", "source")
		_ = exec.Command("docker", "network", "rm", "karta-test_bridge", "karta-test_bridge-egress").Run()
		resetOutbox(t)
		cleanDir(t, bridgeDir)
		resetAll(t)
	})
	for _, d := range []string{"spool", "publish", "config", "keys", "tls"} {
		if err := os.MkdirAll(filepath.Join(bridgeDir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(bridgeDir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}

	// The distributor stand-in, its CA trusted by acquire only.
	writeCA(t)
	ca, err := os.ReadFile(filepath.Join(sourceCfgDir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	writeFileAtomic(t, filepath.Join(bridgeDir, "config", "source-ca.pem"), ca, 0o644)
	startSource(t)
	setRules(t, nil)
	writeJSON(t, filepath.Join(bridgeDir, "config", "source.json"), map[string]any{
		"region_id": "fixture", "snapshot_url": "https://source.test:8443/asia/iran-latest.osm.pbf",
		"md5_url": "https://source.test:8443/asia/iran-latest.osm.pbf.md5", "distributor": "Geofabrik stand-in (test only)",
		"user_agent": "karta-bridge-integration-test (test only)", "allowed_networks": []string{env("KARTA_TEST_SOURCE_SUBNET", "10.231.0.0/24")},
		"ca_file": "/config/bridge/source-ca.pem", "poll_interval": "2s", "reverify_interval": "1m",
		"download_timeout": "60s", "stall_timeout": "5s", "retry_initial": "1s", "retry_max": "4s"})
	// The signing key: generated for this run, test material only.
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	writeFileAtomic(t, filepath.Join(bridgeDir, "keys", "signing-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o644)
	writeJSON(t, filepath.Join(bridgeDir, "config", "signer.json"), map[string]any{
		"bridge_id": "bridge-test", "keys": []map[string]string{{"id": "bridge-test-a", "file": "/run/secrets/bridge_signing_key"}},
		"manifest_validity": "2h", "renew_before": "1h"})
	writeBridgeTLS(t)
	// The fetcher's source: the bridge only, its key and CA.
	writeJSON(t, filepath.Join(sourceCfgDir, "bridge.json"), map[string]any{
		"region_id": "fixture", "manifest_url": "https://bridge-serve:8443/manifest.json",
		"allowed_networks": []string{env("KARTA_BRIDGE_SUBNET", "10.233.0.0/24")}, "ca_file": "/config/sources/bridge-ca.pem",
		"trusted_keys":  []map[string]string{{"id": "bridge-test-a", "ed25519_public_key": b64(key.Public().(ed25519.PublicKey))}},
		"poll_interval": "2s", "max_manifest_validity": "24h", "retry_initial": "1s", "retry_max": "4s"})

	// Base release A through the inbox; the distributor first has B.
	dataA, dataB := readRepo(t, snapA), readRepo(t, snapB)
	submit(t, "a", dataA, nil, "")
	if s := waitSubmission(t, "a", 90*time.Second); s.State != "published" {
		t.Fatalf("A: %+v", s)
	}
	distribute(t, dataB)
	restartPublisher(t, map[string]string{"KARTA_TEST_ONLINE_SOURCE_FILE": bridgeSourceFile, "KARTA_TEST_STALE_AFTER": "8760h"})
	bridgeUp(t, "bridge-serve", "bridge-sign", "bridge-acquire", "fetcher")

	t.Run("only the downloader has a route out; the signer has no network", func(t *testing.T) {
		if got := networksOf(t, "fetcher"); got != "karta-test_bridge" {
			t.Errorf("fetcher networks %q, want only the bridge network", got)
		}
		if got := inspect(t, bridgeContainer(t, "bridge-sign"), "{{.HostConfig.NetworkMode}}"); got != "none" {
			t.Errorf("bridge-sign network mode %q", got)
		}
		if got := networksOf(t, "bridge-acquire"); got != "karta-test_sourcenet" {
			t.Errorf("bridge-acquire networks %q", got)
		}
		if got := networksOf(t, "bridge-serve"); got != "karta-test_bridge" {
			t.Errorf("bridge-serve networks %q", got)
		}
		if strings.Contains(networksOf(t, "publisher"), "bridge") {
			t.Error("the publisher is on the bridge network")
		}
		out, err := exec.Command("docker", "network", "inspect", "-f", `{{index .Options "com.docker.network.bridge.enable_ip_masquerade"}}`,
			"karta-test_bridge").Output()
		if err != nil || strings.TrimSpace(string(out)) != "false" {
			t.Errorf("the bridge network masquerades: %q %v", out, err)
		}
	})

	t.Run("a new distributor file is acquired once, signed, fetched and published", func(t *testing.T) {
		sub := waitOnlineDigest(t, digestOf(dataB), 120*time.Second)
		if sub.State != "published" || status(t).activeID() != sub.release() {
			t.Fatalf("B: %+v", sub)
		}
		v := onlineStatus(t)
		if v.Online.Verified == nil || v.Online.Verified.Serial != 1 || v.Online.Verified.KeyID != "bridge-test-a" {
			t.Errorf("verified %+v", v.Online.Verified)
		}
		if n := countRequests(t, "/asia/iran-latest.osm.pbf"); n < 1 {
			t.Errorf("%d requests for the snapshot", n)
		}
		for _, r := range sourceRequests(t) {
			if r.UserAgent != "karta-bridge-integration-test (test only)" {
				t.Errorf("request without the configured User-Agent: %+v", r)
			}
		}
	})

	dataV := variant(t, at("2026-03-01T00:00:00Z"), nil, nil)
	t.Run("changed bytes with newer data are signed under the next serial", func(t *testing.T) {
		gets := 0
		for _, r := range sourceRequests(t) {
			if r.Method == http.MethodGet && r.Path == "/asia/iran-latest.osm.pbf" {
				gets++
			}
		}
		time.Sleep(5 * time.Second) // several polls with unchanged hints
		after := 0
		for _, r := range sourceRequests(t) {
			if r.Method == http.MethodGet && r.Path == "/asia/iran-latest.osm.pbf" {
				after++
			}
		}
		if after != gets {
			t.Errorf("unchanged hints downloaded the snapshot again (%d -> %d GETs)", gets, after)
		}
		distribute(t, dataV)
		sub := waitOnlineDigest(t, digestOf(dataV), 120*time.Second)
		if sub.State != "published" {
			t.Fatalf("V: %+v", sub)
		}
		if v := onlineStatus(t); v.Online.Verified == nil || v.Online.Verified.Serial != 2 {
			t.Errorf("verified %+v", v.Online.Verified)
		}
	})
	state2, err := os.ReadFile(filepath.Join(bridgeDir, "state", "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("older data is held, not signed, and reported", func(t *testing.T) {
		distribute(t, dataA)
		st := waitSigner(t, "a held download", 60*time.Second, func(s *bridge.SignerState) bool { return s.Held != nil })
		if st.Held.Code != bridge.CodeNotNewer || st.HighWater != 2 || st.Held.SHA256 != digestOf(dataA) {
			t.Errorf("held %+v high water %d", st.Held, st.HighWater)
		}
		req, _ := http.NewRequest(http.MethodGet, bridgeMetricsBase+"/metrics", nil)
		req.Header.Set("Authorization", "Bearer "+monitor)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "karta_bridge_held 1") ||
			!strings.Contains(string(b), "karta_bridge_manifest_serial 2") {
			t.Errorf("bridge metrics %d:\n%s", resp.StatusCode, b)
		}
		resp, err = client.Get(bridgeMetricsBase + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("bridge metrics without a credential: %d", resp.StatusCode)
		}
	})

	t.Run("a signer state older than the publication fails closed until the high-water serial is raised", func(t *testing.T) {
		// Make the signer issue serial 3 (newer data), then restore the
		// state saved at serial 2.
		dataW := variant(t, at("2026-04-01T00:00:00Z"), nil, nil)
		distribute(t, dataW)
		if sub := waitOnlineDigest(t, digestOf(dataW), 120*time.Second); sub.State != "published" {
			t.Fatalf("W: %+v", sub)
		}
		bridgeCompose(t, nil, "--profile", "bridge", "stop", "-t", "5", "bridge-sign")
		writeFileAtomic(t, filepath.Join(bridgeDir, "state", "state.json"), state2, 0o644)
		bridgeUp(t, "bridge-sign")
		id := bridgeContainer(t, "bridge-sign")
		deadline := time.Now().Add(30 * time.Second)
		for inspect(t, id, "{{.State.Status}}") != "exited" && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
		if code := inspect(t, id, "{{.State.ExitCode}}"); code != "4" {
			t.Fatalf("the signer with an older state exited %s, want 4 (fail closed)", code)
		}
		logs, _, _ := bridgeCompose(t, nil, "--profile", "bridge", "logs", "--no-color", "bridge-sign")
		if !strings.Contains(logs, bridge.CodeStateBehind) {
			t.Errorf("the refusal does not name %s:\n%s", bridge.CodeStateBehind, logs)
		}
		// Raising below the published serial is refused; to it, accepted.
		if _, _, code := bridgeCompose(t, nil, "--profile", "bridge", "run", "--rm", "-T", "--no-deps", "bridge-sign",
			"bridge", "sign", "--raise-high-water", "2", "--reason", "'too low'"); code == 0 {
			t.Error("a raise below the published serial was accepted")
		}
		if _, stderr, code := bridgeCompose(t, nil, "--profile", "bridge", "run", "--rm", "-T", "--no-deps", "bridge-sign",
			"bridge", "sign", "--raise-high-water", "3", "--reason", "'Karta verified serial 3'"); code != 0 {
			t.Fatalf("raise: exit %d %s", code, stderr)
		}
		bridgeUp(t, "bridge-sign")
		dataX := variant(t, at("2026-05-01T00:00:00Z"), nil, nil)
		distribute(t, dataX)
		if sub := waitOnlineDigest(t, digestOf(dataX), 120*time.Second); sub.State != "published" {
			t.Fatalf("X after the raise: %+v", sub)
		}
		// The restored state did not know it signed W as serial 3: it signs
		// W again (same bytes, serial 4) and then X (serial 5). No serial is
		// reused, none is lowered.
		if v := onlineStatus(t); v.Online.Verified == nil || v.Online.Verified.Serial != 5 || v.Online.Verified.SnapshotSHA256 != digestOf(dataX) {
			t.Errorf("verified %+v, want X under serial 5", v.Online.Verified)
		}
		var hist []int64
		for _, h := range signerStatus(t).History {
			hist = append(hist, h.Serial)
		}
		for i := 1; i < len(hist); i++ {
			if hist[i] <= hist[i-1] {
				t.Errorf("serials not increasing: %v", hist)
			}
		}
		if st := signerStatus(t); len(st.Raises) != 1 || st.Raises[0].To != 3 {
			t.Errorf("raises %+v", st.Raises)
		}
	})

	t.Run("with the bridge down, manual publication still works", func(t *testing.T) {
		bridgeCompose(t, nil, "--profile", "bridge", "stop", "-t", "5", "bridge-acquire", "bridge-sign", "bridge-serve")
		dataY := variant(t, at("2026-06-01T00:00:00Z"), nil, nil)
		authorize(t, dataY, "manual delivery during a bridge outage")
		submit(t, "y", dataY, nil, "")
		if s := waitSubmission(t, "y", 90*time.Second); s.State != "published" {
			t.Fatalf("Y: %+v", s)
		}
		waitFetcher(t, "a failed check against the stopped bridge", 30*time.Second, func(s *online.State) bool { return s.LastError != nil })
	})
}
