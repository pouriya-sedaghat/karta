//go:build integration

// Stage 3 acceptance tests: online updates against the running stack with
// the fetcher and a local controlled HTTPS source (compose.test.yaml,
// profile `online`; tests/integration/sourceserver). Snapshots are the
// committed fixture and variants generated from it here; manifests are
// signed with a key generated for the run; the source's certificate comes
// from a CA generated for the run. The fetcher sits on an internal Docker
// network with only the test source, so nothing is downloaded from a live
// external host.
//
// Environment (set by `make test-integration`):
//
//	KARTA_ONLINE_HOST_DIR         the fetcher's outbox (host directory)
//	KARTA_TEST_SOURCE_CONFIG_DIR  mounted as /config/sources (source file, CA)
//	KARTA_TEST_SOURCE_DOCROOT     the test source's document root
//	KARTA_TEST_SOURCE_TLS_DIR     the test source's certificate and key
package integration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/online"
	"github.com/pouriya-sedaghat/karta/internal/pbfwrite"
)

var (
	onlineDir     = os.Getenv("KARTA_ONLINE_HOST_DIR")
	sourceCfgDir  = os.Getenv("KARTA_TEST_SOURCE_CONFIG_DIR")
	sourceDocroot = os.Getenv("KARTA_TEST_SOURCE_DOCROOT")
	sourceTLSDir  = os.Getenv("KARTA_TEST_SOURCE_TLS_DIR")
	sourceCtl     = env("KARTA_TEST_SOURCE_CTL", "http://127.0.0.1:"+env("KARTA_TEST_SOURCE_CTL_PORT", "18082"))
)

const (
	manifestPath = "/karta/manifest.json"
	sourceFile   = "/config/sources/online.json"
	sourceNet    = "karta-test_sourcenet"
)

// onlineSuite holds the run's signing key and source configuration.
type onlineSuite struct {
	key    ed25519.PrivateKey
	other  ed25519.PrivateKey
	cfg    map[string]any
	serial int64
}

// sourceRule mirrors the test source's per-path behaviour.
type sourceRule struct {
	Status          int    `json:"status,omitempty"`
	Location        string `json:"location,omitempty"`
	DelayHeaders    string `json:"delay_headers,omitempty"`
	ThrottleBPS     int    `json:"throttle_bps,omitempty"`
	StallAfter      int64  `json:"stall_after,omitempty"`
	CloseAfter      int64  `json:"close_after,omitempty"`
	CloseTimes      int    `json:"close_times,omitempty"`
	Corrupt         bool   `json:"corrupt,omitempty"`
	ExtraBytes      int    `json:"extra_bytes,omitempty"`
	NoContentLength bool   `json:"no_content_length,omitempty"`
	ContentEncoding string `json:"content_encoding,omitempty"`
	IgnoreRange     bool   `json:"ignore_range,omitempty"`
}

type sourceRequest struct {
	Method  string `json:"method"`
	Host    string `json:"host"`
	Path    string `json:"path"`
	Query   string `json:"query"`
	Range   string `json:"range"`
	IfRange string `json:"if_range"`
	Auth    string `json:"authorization"`
	Status  int    `json:"status"`
}

// --- the test source -----------------------------------------------------------

func writeFileAtomic(t *testing.T, path string, b []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// writeCA creates a CA and a server certificate for the test source's names.
func writeCA(t *testing.T) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Karta test source CA (test only)"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	srvKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srvTpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "source.test"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"source.test", "mirror.test", "other.test", "loopback.test", "metadata.test", "lan.test"}}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTpl, ca, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(srvKey)
	writeFileAtomic(t, filepath.Join(sourceTLSDir, "server.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}), 0o644)
	writeFileAtomic(t, filepath.Join(sourceTLSDir, "server-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o644)
	writeFileAtomic(t, filepath.Join(sourceCfgDir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644)
}

func (o *onlineSuite) writeSource(t *testing.T) {
	t.Helper()
	b, _ := json.MarshalIndent(o.cfg, "", "  ")
	writeFileAtomic(t, filepath.Join(sourceCfgDir, "online.json"), b, 0o644)
}

// setSource changes the source file (the fetcher and publisher reread it).
func (o *onlineSuite) setSource(t *testing.T, changes map[string]any) {
	t.Helper()
	for k, v := range changes {
		if v == nil {
			delete(o.cfg, k)
		} else {
			o.cfg[k] = v
		}
	}
	o.writeSource(t)
}

func (o *onlineSuite) defaultSource() map[string]any {
	return map[string]any{
		"region_id":             "fixture",
		"manifest_url":          "https://source.test:8443" + manifestPath,
		"allowed_hosts":         []string{"mirror.test:8443"},
		"allowed_networks":      []string{env("KARTA_TEST_SOURCE_SUBNET", "10.231.0.0/24")},
		"ca_file":               "/config/sources/ca.pem",
		"trusted_keys":          []map[string]string{{"id": "test-2026a", "ed25519_public_key": b64(o.key.Public().(ed25519.PublicKey))}},
		"poll_interval":         "2s",
		"max_manifest_validity": "24h",
		"download_timeout":      "60s",
		"stall_timeout":         "3s",
		"retry_initial":         "1s",
		"retry_max":             "4s",
		"max_download_attempts": 4,
		"abandon_for":           "1h",
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func base64StdDecode(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

func setRules(t *testing.T, rules map[string]sourceRule) {
	t.Helper()
	if rules == nil {
		rules = map[string]sourceRule{}
	}
	b, _ := json.Marshal(rules)
	req, _ := http.NewRequest(http.MethodPut, sourceCtl+"/rules", bytes.NewReader(b))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set rules: %d", resp.StatusCode)
	}
}

func sourceRequests(t *testing.T) []sourceRequest {
	t.Helper()
	resp, err := client.Get(sourceCtl + "/requests")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []sourceRequest
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func clearRequests(t *testing.T) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, sourceCtl+"/requests", nil)
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	}
}

func countRequests(t *testing.T, path string) int {
	t.Helper()
	n := 0
	for _, r := range sourceRequests(t) {
		if r.Path == path {
			n++
		}
	}
	return n
}

func startSource(t *testing.T) {
	t.Helper()
	if _, stderr, code := compose(t, "--profile", "online", "up", "-d", "--no-deps", "--force-recreate", "source"); code != 0 {
		t.Fatalf("start source: %s", stderr)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := client.Get(sourceCtl + "/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("test source did not start")
}

// --- the fetcher ---------------------------------------------------------------

func startFetcher(t *testing.T, settings map[string]string) {
	t.Helper()
	var envs []string
	for k, v := range settings {
		envs = append(envs, k+"="+v)
	}
	if _, stderr, code := composeEnv(t, envs, "--profile", "online", "up", "-d", "--no-deps", "--force-recreate", "fetcher"); code != 0 {
		t.Fatalf("start fetcher: %s", stderr)
	}
}

func stopFetcher(t *testing.T) {
	t.Helper()
	compose(t, "--profile", "online", "stop", "-t", "5", "fetcher")
}

func fetcherContainer(t *testing.T) string {
	t.Helper()
	out, _, code := compose(t, "--profile", "online", "ps", "-a", "-q", "fetcher")
	if code != 0 || strings.TrimSpace(out) == "" {
		t.Fatal("no fetcher container")
	}
	return strings.TrimSpace(out)
}

func waitFetcherExit(t *testing.T, timeout time.Duration) int {
	t.Helper()
	id := fetcherContainer(t)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "inspect", "-f", "{{.State.Status}} {{.State.ExitCode}}", id).Output()
		if err == nil {
			f := strings.Fields(string(out))
			if len(f) == 2 && f[0] == "exited" {
				code, _ := strconv.Atoi(f[1])
				return code
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("fetcher did not exit within %s", timeout)
	return -1
}

// resetOutbox stops the fetcher and empties its outbox and state.
func resetOutbox(t *testing.T) {
	t.Helper()
	stopFetcher(t)
	entries, _ := os.ReadDir(onlineDir)
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(onlineDir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

// waitFetcher waits until the fetcher's state satisfies ok.
func waitFetcher(t *testing.T, what string, timeout time.Duration, ok func(*online.State) bool) *online.State {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *online.State
	for time.Now().Before(deadline) {
		if s, err := online.ReadState(onlineDir); err == nil && s != nil {
			last = s
			if ok(s) {
				return s
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	b, _ := json.Marshal(last)
	t.Fatalf("fetcher: %s not reached within %s; state %s", what, timeout, b)
	return nil
}

// failedWith waits for a check that failed with code after since.
func failedWith(code string, since time.Time) func(*online.State) bool {
	return func(s *online.State) bool {
		return s.LastError != nil && s.LastError.Code == code && !s.LastError.At.Before(since.Add(-time.Second))
	}
}

func succeededSince(since time.Time) func(*online.State) bool {
	return func(s *online.State) bool {
		return s.LastSuccessAt != nil && !s.LastSuccessAt.Before(since.Add(-time.Second)) && s.LastError == nil
	}
}

func deliveredSerial(serial int64) func(*online.State) bool {
	return func(s *online.State) bool { return s.Delivered != nil && s.Delivered.Serial == serial }
}

// --- publishing on the test source ------------------------------------------------

// artifact is one file the manifest signs.
type published struct {
	serial   int64
	data     []byte
	raw      []byte
	name     string // delivery name
	manifest online.Manifest
}

type pubOpt func(*online.Manifest)

// publishOnline writes data (and an optional sidecar) to the test source
// and serves a manifest with the next serial signed by the suite's key; ts
// is the data timestamp the snapshot's header carries.
func (o *onlineSuite) publishOnline(t *testing.T, data []byte, ts time.Time, sidecar []byte, opts ...pubOpt) published {
	t.Helper()
	p := o.stage(t, data, ts, sidecar, opts...)
	setManifest(t, p.raw)
	return p
}

// stage writes the files and signs the manifest of the next serial without
// serving it yet.
func (o *onlineSuite) stage(t *testing.T, data []byte, ts time.Time, sidecar []byte, opts ...pubOpt) published {
	t.Helper()
	o.serial++
	name := fmt.Sprintf("snap-%d.osm.pbf", o.serial)
	writeFileAtomic(t, filepath.Join(sourceDocroot, "karta", name), data, 0o644)
	now := time.Now().UTC().Truncate(time.Second)
	m := online.Manifest{Format: online.ManifestFormat, RegionID: "fixture", BBox: online.BBox(fixtureBox), Serial: o.serial,
		IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(12 * time.Hour),
		Snapshot: online.Snapshot{URL: name, SHA256: digestOf(data), SizeBytes: int64(len(data)), DataTimestamp: ts.UTC()}}
	if sidecar != nil {
		writeFileAtomic(t, filepath.Join(sourceDocroot, "karta", name+".provenance.json"), sidecar, 0o644)
		m.Provenance = &online.File{URL: name + ".provenance.json", SHA256: digestOf(sidecar), SizeBytes: int64(len(sidecar))}
	}
	for _, f := range opts {
		f(&m)
	}
	raw, err := online.Sign(m, online.Signer{KeyID: "test-2026a", Key: o.key})
	if err != nil {
		t.Fatal(err)
	}
	return published{serial: o.serial, data: data, raw: raw, name: online.DeliveryName("fixture", m.Serial, m.Snapshot.SHA256), manifest: m}
}

// setManifest serves raw as the manifest.
func setManifest(t *testing.T, raw []byte) {
	t.Helper()
	writeFileAtomic(t, filepath.Join(sourceDocroot, "karta", "manifest.json"), raw, 0o644)
}

// --- operator status, online part ---------------------------------------------------

type onlineStatusView struct {
	Online struct {
		Enabled      bool `json:"enabled"`
		AutoActivate bool `json:"auto_activate"`
		Policy       *struct {
			AutoActivate bool    `json:"auto_activate"`
			ChangedBy    *string `json:"changed_by"`
			ChangeReason *string `json:"change_reason"`
		} `json:"policy"`
		Verified *struct {
			Serial         int64  `json:"serial"`
			SnapshotSHA256 string `json:"snapshot_sha256"`
			KeyID          string `json:"key_id"`
		} `json:"verified"`
		Fetcher struct {
			State           *online.State `json:"state"`
			StateAgeSeconds *float64      `json:"state_age_seconds"`
			Error           string        `json:"error"`
		} `json:"fetcher"`
	} `json:"online"`
	Freshness struct {
		UpdateMode           string   `json:"update_mode"`
		ActiveDataAgeSeconds *float64 `json:"active_data_age_seconds"`
		StaleAfterSeconds    *float64 `json:"stale_after_seconds"`
		Stale                *bool    `json:"stale"`
	} `json:"freshness"`
	Submissions []struct {
		ID             int64   `json:"id"`
		Source         string  `json:"source"`
		Name           string  `json:"name"`
		State          string  `json:"state"`
		ReasonCode     *string `json:"reason_code"`
		ManifestSerial *int64  `json:"manifest_serial"`
	} `json:"submissions"`
}

func onlineStatus(t *testing.T) onlineStatusView {
	t.Helper()
	r := op(t, http.MethodGet, "/v1/operator/status", secret(t, "operator_monitor_token"), nil)
	expectStatus(t, r, http.StatusOK)
	var v onlineStatusView
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// noSubmission checks that no submission of that name exists.
func noSubmission(t *testing.T, name string) {
	t.Helper()
	for _, s := range status(t).Submissions {
		if s.Name == name {
			t.Errorf("unexpected submission %s: %+v", name, s)
		}
	}
}

type manifestView struct {
	Release struct {
		ReleaseID string `json:"release_id"`
	} `json:"release"`
	Capabilities map[string]bool `json:"capabilities"`
	Freshness    struct {
		UpdateMode        string `json:"update_mode"`
		StaleAfterSeconds *int64 `json:"stale_after_seconds"`
		Stale             *bool  `json:"stale"`
	} `json:"freshness"`
}

func publicManifest(t *testing.T) (manifestView, []byte) {
	t.Helper()
	r := get(t, "/v1/manifest")
	expectStatus(t, r, http.StatusOK)
	var m manifestView
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatal(err)
	}
	return m, r.body
}

// waitManifestMode waits for the API to pick up the update mode (it reads
// the registry every poll interval).
func waitManifestMode(t *testing.T, mode string) manifestView {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var m manifestView
	for time.Now().Before(deadline) {
		m, _ = publicManifest(t)
		if m.Freshness.UpdateMode == mode {
			return m
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("manifest update_mode %q, want %q", m.Freshness.UpdateMode, mode)
	return m
}

func (o *onlineSuite) restartOnlinePublisher(t *testing.T, extra map[string]string) {
	t.Helper()
	s := map[string]string{"KARTA_TEST_ONLINE_SOURCE_FILE": sourceFile}
	for k, v := range extra {
		s[k] = v
	}
	restartPublisher(t, s)
}

func fetcherLogs(t *testing.T) string {
	t.Helper()
	out, _, _ := compose(t, "--profile", "online", "logs", "--no-color", "fetcher")
	return out
}

func publisherLogs(t *testing.T) string {
	t.Helper()
	out, _, _ := compose(t, "logs", "--no-color", "publisher")
	return out
}

// sampleFetcherMemory samples the fetcher container's memory use until
// stop is closed and returns the largest sample in bytes.
func sampleFetcherMemory(id string, stop <-chan struct{}) <-chan int64 {
	out := make(chan int64, 1)
	go func() {
		var peak int64
		for {
			select {
			case <-stop:
				out <- peak
				return
			default:
			}
			b, err := exec.Command("docker", "stats", "--no-stream", "--format", "{{.MemUsage}}", id).Output()
			if err == nil {
				if v := parseMem(strings.TrimSpace(strings.SplitN(string(b), "/", 2)[0])); v > peak {
					peak = v
				}
			}
			time.Sleep(300 * time.Millisecond)
		}
	}()
	return out
}

// cgroupPeak returns a container's peak memory use as its cgroup records it
// (cgroup v2 memory.peak or v1 memory.max_usage_in_bytes; it includes page
// cache), or 0 if neither is readable from the test host.
func cgroupPeak(id string) int64 {
	for _, p := range []string{
		"/sys/fs/cgroup/system.slice/docker-" + id + ".scope/memory.peak",
		"/sys/fs/cgroup/docker/" + id + "/memory.peak",
		"/sys/fs/cgroup/memory/docker/" + id + "/memory.max_usage_in_bytes",
		"/sys/fs/cgroup/memory/system.slice/docker-" + id + ".scope/memory.max_usage_in_bytes",
	} {
		if b, err := os.ReadFile(p); err == nil {
			if v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
				return v
			}
		}
	}
	return 0
}

func parseMem(s string) int64 {
	units := []struct {
		suffix string
		mult   float64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"B", 1}}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), 64)
			if err != nil {
				return 0
			}
			return int64(v * u.mult)
		}
	}
	return 0
}

// --- the test --------------------------------------------------------------------

func TestOnline(t *testing.T) {
	if onlineDir == "" || sourceCfgDir == "" || sourceDocroot == "" || sourceTLSDir == "" {
		t.Fatal("the Stage 3 directories are not set; run `make test-integration`")
	}
	admin, monitor := secret(t, "operator_token"), secret(t, "operator_monitor_token")
	resetAll(t)
	resetOutbox(t)
	t.Cleanup(func() {
		stopFetcher(t)
		compose(t, "--profile", "online", "stop", "-t", "2", "source")
		resetOutbox(t)
		resetAll(t)
	})
	seed := make([]byte, ed25519.SeedSize)
	_, _ = rand.Read(seed)
	o := &onlineSuite{key: ed25519.NewKeyFromSeed(seed), other: testOtherKey()}
	o.cfg = o.defaultSource()
	writeCA(t)
	o.writeSource(t)
	_ = os.MkdirAll(filepath.Join(sourceDocroot, "karta"), 0o755)
	startSource(t)
	setRules(t, nil)

	// Base release A through the inbox, as a deployment would have.
	dataA := readRepo(t, snapA)
	submit(t, "a", dataA, nil, "")
	if s := waitSubmission(t, "a", 90*time.Second); s.State != "published" {
		t.Fatalf("A: %+v", s)
	}
	relA := status(t).activeID()
	waitManifest(t, relA)

	t.Run("online updates are off unless configured", func(t *testing.T) {
		v := onlineStatus(t)
		if v.Online.Enabled || v.Freshness.UpdateMode != "manual" {
			t.Errorf("status online %+v freshness %+v", v.Online, v.Freshness)
		}
		m := waitManifestMode(t, "manual")
		if m.Capabilities["online_updates"] || m.Freshness.Stale != nil || m.Freshness.StaleAfterSeconds != nil {
			t.Errorf("manifest %+v", m)
		}
		r := op(t, http.MethodPost, "/v1/operator/online/pause", admin, map[string]any{"reason": "check refusal"})
		if code, _ := r.errorCode(t); r.status != http.StatusConflict || code != "online_disabled" {
			t.Errorf("pause while disabled: %d %s", r.status, r.body)
		}
		if a := lastAudit(t, "online_pause", "check refusal"); a.Outcome != "rejected" {
			t.Errorf("refusal audit %+v", a)
		}
		// The publisher (which parses snapshots) is not on the fetcher's
		// network, and has no route to the source.
		out, _ := exec.Command("docker", "inspect", "-f", "{{range $n, $_ := .NetworkSettings.Networks}}{{$n}} {{end}}", publisherContainer(t)).Output()
		if strings.Contains(string(out), "sourcenet") || strings.Contains(string(out), "egress") {
			t.Errorf("publisher networks: %s", out)
		}
	})

	o.restartOnlinePublisher(t, map[string]string{"KARTA_TEST_STALE_AFTER": "8760h"})
	startFetcher(t, nil)

	t.Run("the fetcher has no route out of Docker", func(t *testing.T) {
		waitFetcher(t, "first check", 30*time.Second, func(s *online.State) bool { return s.LastCheckAt != nil })
		cmd := exec.Command(filepath.Join(repoRoot, "scripts/check-isolated.sh"), fetcherContainer(t), sourceNet, "karta-importer:"+env("KARTA_IMAGE_TAG", "local"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fetcher isolation: %v\n%s", err, out)
		} else {
			t.Logf("%s", out)
		}
	})

	var relOnline1 string
	var onlineData1 []byte
	var onlineTS1 time.Time
	t.Run("a newer signed snapshot is downloaded slowly, verified and activated while serving", func(t *testing.T) {
		o.restartOnlinePublisher(t, map[string]string{"KARTA_TEST_OSM2PGSQL": "/usr/local/bin/slow-osm2pgsql", "KARTA_TEST_STALE_AFTER": "8760h"})
		clearRequests(t)
		pinned := startPinned(relA)
		l := startLoad(4)
		// B's data with a later timestamp: a digest that is not pinned in
		// the region file, so only the signature authorizes it.
		ts := *at("2026-02-10T00:00:00Z")
		newer := variant(t, &ts, nil, nil)
		// About 11 s of download at 200 bytes/s, then a 15 s import.
		setRules(t, map[string]sourceRule{"/karta/snap-1.osm.pbf": {ThrottleBPS: 200}})
		p := o.publishOnline(t, newer, ts, nil)
		stopMem := make(chan struct{})
		mem := sampleFetcherMemory(fetcherContainer(t), stopMem)
		start := time.Now()
		st := waitFetcher(t, "download in progress", 60*time.Second, func(s *online.State) bool {
			return s.Download != nil && s.Download.Bytes > 0 && s.Download.Bytes < int64(len(newer))
		})
		v := onlineStatus(t)
		if v.Online.Fetcher.State == nil || v.Online.Fetcher.State.Download == nil {
			t.Errorf("operator status does not show the download: %+v", v.Online.Fetcher)
		}
		t.Logf("download progress %d of %d bytes", st.Download.Bytes, len(newer))
		waitFetcher(t, "delivery", 90*time.Second, deliveredSerial(p.serial))
		downloadSecs := time.Since(start).Seconds()
		close(stopMem)
		peak := <-mem
		s := waitSubmission(t, p.name, 120*time.Second)
		if s.State != "published" {
			t.Fatalf("online submission: %+v %s", s, strOr(s.Reason))
		}
		relOnline1, onlineData1, onlineTS1 = s.release(), newer, ts
		waitManifest(t, relOnline1)
		time.Sleep(2 * time.Second)
		sum := l.finish(t, "slow online download, import and switch")
		pinned.finish(t)
		recordMeasurement(t, "online_slow_download_import_switch", map[string]any{"load": sum, "download_seconds": downloadSecs,
			"snapshot_bytes": len(newer), "fetcher_peak_memory_bytes_sampled": peak, "total_seconds": time.Since(start).Seconds()})
		if len(sum.Releases) != 2 {
			t.Errorf("load saw releases %v, want A then the online release", sum.Releases)
		}
		v = onlineStatus(t)
		if !v.Online.Enabled || !v.Online.AutoActivate || v.Online.Verified == nil || v.Online.Verified.Serial != p.serial ||
			v.Online.Verified.SnapshotSHA256 != digestOf(newer) || v.Online.Verified.KeyID != "test-2026a" {
			t.Errorf("online status after publication: %+v", v.Online)
		}
		fs := v.Online.Fetcher.State
		if fs == nil || fs.LastSuccessAt == nil || fs.NextAttemptAt == nil || fs.Delivered == nil || fs.Delivered.Serial != p.serial ||
			v.Online.Fetcher.StateAgeSeconds == nil {
			t.Errorf("fetcher report: %+v", v.Online.Fetcher)
		}
		var sub *struct {
			ID             int64   `json:"id"`
			Source         string  `json:"source"`
			Name           string  `json:"name"`
			State          string  `json:"state"`
			ReasonCode     *string `json:"reason_code"`
			ManifestSerial *int64  `json:"manifest_serial"`
		}
		for i := range v.Submissions {
			if v.Submissions[i].Name == p.name {
				sub = &v.Submissions[i]
			}
		}
		if sub == nil || sub.Source != "online" || sub.ManifestSerial == nil || *sub.ManifestSerial != p.serial {
			t.Errorf("submission record %+v", sub)
		}
		m := waitManifestMode(t, "online")
		if !m.Capabilities["online_updates"] || m.Freshness.StaleAfterSeconds == nil || *m.Freshness.StaleAfterSeconds != 8760*3600 ||
			m.Freshness.Stale == nil || *m.Freshness.Stale {
			t.Errorf("public freshness %+v caps %v", m.Freshness, m.Capabilities)
		}
		// The import report records the signature as the authorization.
		ctx := context.Background()
		db := superuser(t, "karta_"+relOnline1)
		var authorizedBy string
		if err := db.QueryRow(ctx, `SELECT report->'source'->>'authorized_by' FROM karta.release_info`).Scan(&authorizedBy); err != nil {
			t.Fatal(err)
		}
		db.Close(ctx)
		if !strings.HasPrefix(authorizedBy, fmt.Sprintf("signed manifest serial %d (key test-2026a, sha256:", p.serial)) {
			t.Errorf("authorized_by %q", authorizedBy)
		}
		// Only the manifest and the snapshot were requested, without a
		// query, over the allowed host.
		for _, r := range sourceRequests(t) {
			if r.Query != "" || !strings.HasPrefix(r.Host, "source.test") || r.Auth != "" {
				t.Errorf("request %+v", r)
			}
		}
		setRules(t, nil)
		o.restartOnlinePublisher(t, map[string]string{"KARTA_TEST_STALE_AFTER": "8760h"})
	})
	if relOnline1 == "" {
		t.Fatal("no online release; stopping")
	}

	t.Run("duplicate and older inputs change nothing", func(t *testing.T) {
		// The same snapshot re-signed under a newer serial is not
		// downloaded or delivered again.
		clearRequests(t)
		since := time.Now()
		p := o.publishOnline(t, onlineData1, onlineTS1, nil)
		waitFetcher(t, "check of the re-signed manifest", 30*time.Second, func(s *online.State) bool {
			return s.HighestSerial == p.serial && succeededSince(since)(s)
		})
		if n := countRequests(t, "/karta/"+fmt.Sprintf("snap-%d.osm.pbf", p.serial)); n != 0 {
			t.Errorf("the duplicate snapshot was downloaded %d times", n)
		}
		noSubmission(t, p.name)
		// An older snapshot (signed, newer serial) is delivered but refused
		// by the forward rule; nothing changes.
		older := variant(t, at("2026-01-15T00:00:00Z"), nil, nil)
		p = o.publishOnline(t, older, *at("2026-01-15T00:00:00Z"), nil)
		if s := waitSubmission(t, p.name, 90*time.Second); s.State != "rejected" || s.code() != "older_than_active" {
			t.Errorf("older: %+v", s)
		}
		// Going back to an earlier manifest is a replay: refused before any
		// download.
		replay := p.raw
		p2 := o.publishOnline(t, variant(t, at("2026-02-20T00:00:00Z"), nil, nil), *at("2026-02-20T00:00:00Z"), nil)
		if s := waitSubmission(t, p2.name, 90*time.Second); s.State != "published" {
			t.Fatalf("serial %d: %+v", p2.serial, s)
		}
		since = time.Now()
		setManifest(t, replay)
		waitFetcher(t, "replay refused", 30*time.Second, failedWith(online.CodeManifestReplayed, since))
		if got := status(t).activeID(); got == relA || got == relOnline1 {
			t.Errorf("active changed back to %s", got)
		}
		setManifest(t, p2.raw)
		waitFetcher(t, "recovered", 30*time.Second, succeededSince(time.Now()))
	})

	t.Run("a compromised fetcher cannot publish unsigned, forged, replayed or swapped snapshots", func(t *testing.T) {
		// Write deliveries straight into the outbox, as a compromised or
		// buggy fetcher could. The publisher verifies each itself.
		stopFetcher(t)
		cur := onlineStatus(t).Online.Verified
		forge := func(name string, snap, manifest []byte, marker string) {
			if manifest != nil {
				writeFileAtomic(t, filepath.Join(onlineDir, name+".osm.pbf.manifest.json"), manifest, 0o644)
			}
			writeFileAtomic(t, filepath.Join(onlineDir, name+".osm.pbf"), snap, 0o644)
			if marker == "" {
				marker = digestOf(snap) + "  " + name + ".osm.pbf\n"
			}
			writeFileAtomic(t, filepath.Join(onlineDir, name+".osm.pbf.ready"), []byte(marker), 0o644)
		}
		newer := variant(t, at("2026-06-01T00:00:00Z"), nil, nil)
		mk := func(serial int64, snap []byte, ts time.Time, key ed25519.PrivateKey, id string) []byte {
			now := time.Now().UTC().Truncate(time.Second)
			raw, err := online.Sign(online.Manifest{Format: online.ManifestFormat, RegionID: "fixture", BBox: online.BBox(fixtureBox),
				Serial: serial, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
				Snapshot: online.Snapshot{URL: "x.osm.pbf", SHA256: digestOf(snap), SizeBytes: int64(len(snap)), DataTimestamp: ts}},
				online.Signer{KeyID: id, Key: key})
			if err != nil {
				t.Fatal(err)
			}
			return raw
		}
		before := status(t).activeID()
		cases := []struct{ name, code string }{
			{"forged-unsigned", "manifest_missing"},
			{"forged-untrusted", online.CodeSignatureUntrusted},
			{"forged-wrongkey", online.CodeSignatureInvalid},
			{"forged-swapped", online.CodeDigestMismatch},
			{"forged-replay", online.CodeManifestReplayed},
			{"forged-conflict", online.CodeManifestConflict},
		}
		forge("forged-unsigned", newer, nil, "")
		forge("forged-untrusted", newer, mk(cur.Serial+10, newer, *at("2026-06-01T00:00:00Z"), o.other, "intruder"), "")
		forge("forged-wrongkey", newer, mk(cur.Serial+11, newer, *at("2026-06-01T00:00:00Z"), o.other, "test-2026a"), "")
		// A correctly signed manifest with other bytes behind it: the
		// marker's digest (written by the fetcher) authorizes nothing.
		forge("forged-swapped", newer, mk(cur.Serial+12, onlineData1, onlineTS1, o.key, "test-2026a"), "")
		forge("forged-replay", newer, mk(cur.Serial-1, newer, *at("2026-06-01T00:00:00Z"), o.key, "test-2026a"), "")
		forge("forged-conflict", newer, mk(cur.Serial, newer, *at("2026-06-01T00:00:00Z"), o.key, "test-2026a"), "")
		for _, c := range cases {
			s := waitSubmission(t, c.name, 60*time.Second)
			if s.State != "rejected" || s.code() != c.code {
				t.Errorf("%s: %+v %s", c.name, s, strOr(s.Reason))
			}
		}
		if got := status(t).activeID(); got != before {
			t.Errorf("active changed to %s", got)
		}
		// None of them advanced the verified serial: the swapped delivery's
		// genuine manifest was refused before its serial was recorded.
		if v := onlineStatus(t).Online.Verified; v.Serial != cur.Serial {
			t.Errorf("verified serial %d, want %d", v.Serial, cur.Serial)
		}
		resetOutbox(t)
		startFetcher(t, nil)
	})

	t.Run("invalid signatures, digests, provenance and source claims are rejected", func(t *testing.T) {
		before := status(t).activeID()
		since := time.Now()
		// Signed by a key the source file does not trust: nothing is downloaded.
		clearRequests(t)
		p := o.stage(t, variant(t, at("2026-06-02T00:00:00Z"), nil, nil), *at("2026-06-02T00:00:00Z"), nil)
		raw, _ := online.Sign(p.manifest, online.Signer{KeyID: "intruder", Key: o.other})
		setManifest(t, raw)
		waitFetcher(t, "untrusted key refused", 30*time.Second, failedWith(online.CodeSignatureUntrusted, since))
		// A tampered payload.
		var env online.Envelope
		_ = json.Unmarshal(p.raw, &env)
		pl, _ := base64StdDecode(env.Payload)
		env.Payload = base64Std(bytes.Replace(pl, []byte(`"serial":`), []byte(`"serial":9`), 1))
		tampered, _ := json.Marshal(env)
		since = time.Now()
		setManifest(t, tampered)
		waitFetcher(t, "tampered manifest refused", 30*time.Second, failedWith(online.CodeSignatureInvalid, since))
		// A manifest for another region.
		since = time.Now()
		other := p.manifest
		other.RegionID, other.Serial = "tehran-chitgar", o.serial+1
		setManifest(t, mustSign(t, other, o.key))
		waitFetcher(t, "other region refused", 30*time.Second, failedWith(online.CodeRegionMismatch, since))
		// An expired manifest: the source has gone stale.
		since = time.Now()
		exp := p.manifest
		exp.Serial, exp.IssuedAt, exp.ExpiresAt = o.serial+1, time.Now().Add(-3*time.Hour).Truncate(time.Second), time.Now().Add(-time.Hour).Truncate(time.Second)
		setManifest(t, mustSign(t, exp, o.key))
		waitFetcher(t, "expired manifest refused", 30*time.Second, failedWith(online.CodeManifestExpired, since))
		// An oversized manifest.
		since = time.Now()
		setManifest(t, append(mustSign(t, p.manifest, o.key), bytes.Repeat([]byte(" "), online.MaxEnvelopeBytes)...))
		waitFetcher(t, "oversized manifest refused", 30*time.Second, failedWith(online.CodeManifestTooLarge, since))
		// Not JSON at all.
		since = time.Now()
		setManifest(t, []byte("<html>maintenance</html>"))
		waitFetcher(t, "malformed manifest refused", 30*time.Second, failedWith(online.CodeManifestInvalid, since))
		for _, r := range sourceRequests(t) {
			if r.Path == "/karta/"+fmt.Sprintf("snap-%d.osm.pbf", p.serial) {
				t.Errorf("a snapshot was downloaded for a manifest that does not verify: %+v", r)
			}
		}
		// Corrupt bytes behind a valid manifest: the fetcher discards them.
		since = time.Now()
		setRules(t, map[string]sourceRule{"/karta/" + fmt.Sprintf("snap-%d.osm.pbf", p.serial): {Corrupt: true}})
		setManifest(t, p.raw)
		waitFetcher(t, "corrupt download discarded", 30*time.Second, failedWith(online.CodeDigestMismatch, since))
		if _, err := os.Stat(online.PartialPath(filepath.Join(onlineDir, online.PartialDirName), p.manifest.Snapshot.SHA256)); !os.IsNotExist(err) {
			t.Error("the corrupt download was kept")
		}
		noSubmission(t, p.name)
		if got := status(t).activeID(); got != before {
			t.Errorf("active changed to %s while the source misbehaved", got)
		}
		setRules(t, nil)
		if s := waitSubmission(t, p.name, 90*time.Second); s.State != "published" {
			t.Fatalf("after the source was repaired: %+v", s)
		}
		before = status(t).activeID()
		// A signed data timestamp that disagrees with the snapshot.
		snap := variant(t, at("2026-06-03T00:00:00Z"), nil, nil)
		p = o.publishOnline(t, snap, *at("2026-06-04T00:00:00Z"), nil)
		if s := waitSubmission(t, p.name, 90*time.Second); s.State != "rejected" || s.code() != online.CodeManifestConflict {
			t.Errorf("conflicting timestamp: %+v %s", s, strOr(s.Reason))
		}
		// A signed provenance sidecar that does not describe the snapshot.
		snap = variant(t, at("2026-06-05T00:00:00Z"), nil, nil)
		badProv := []byte(`{"output_sha256":"` + strings.Repeat("0", 64) + `","bbox_wgs84":"0,0,0.02,0.015","license":"ODbL 1.0",` +
			`"source_fileinfo":{"header":{"option":{"timestamp":"2026-06-05T00:00:00Z"}}},"output_fileinfo":{"file":{"size":1}}}`)
		p = o.publishOnline(t, snap, *at("2026-06-05T00:00:00Z"), badProv)
		if s := waitSubmission(t, p.name, 90*time.Second); s.State != "rejected" || s.code() != "provenance_invalid" {
			t.Errorf("invalid provenance: %+v %s", s, strOr(s.Reason))
		}
		// A malformed snapshot behind a valid signature.
		junk := append([]byte(nil), snap...)
		junk = junk[:len(junk)-7]
		p = o.publishOnline(t, junk, *at("2026-06-05T00:00:00Z"), nil)
		if s := waitSubmission(t, p.name, 90*time.Second); s.State != "rejected" || s.code() != "malformed_snapshot" {
			t.Errorf("malformed: %+v", s)
		}
		if got := status(t).activeID(); got != before {
			t.Errorf("active changed to %s", got)
		}
		waitReady(t, true, "ready")
	})

	t.Run("redirects, other hosts and local or private destinations are refused", func(t *testing.T) {
		since := time.Now()
		setRules(t, map[string]sourceRule{manifestPath: {Status: http.StatusFound, Location: "https://mirror.test:8443" + manifestPath}})
		waitFetcher(t, "redirect refused", 30*time.Second, failedWith(online.CodeRedirectRefused, since))
		setRules(t, nil)
		for _, u := range []string{"https://other.test:8443/karta/s.osm.pbf", "http://source.test:8443/karta/s.osm.pbf",
			"https://source.test:8443/karta/s.osm.pbf?token=x"} {
			since = time.Now()
			o.publishOnline(t, variant(t, at("2026-06-10T00:00:00Z"), nil, nil), *at("2026-06-10T00:00:00Z"), nil,
				func(m *online.Manifest) { m.Snapshot.URL = u })
			waitFetcher(t, "URL "+u+" refused", 30*time.Second, failedWith(online.CodeURLRefused, since))
		}
		for _, host := range []string{"loopback.test", "metadata.test", "lan.test"} {
			since = time.Now()
			o.setSource(t, map[string]any{"manifest_url": "https://" + host + ":8443" + manifestPath})
			waitFetcher(t, host+" refused", 30*time.Second, failedWith(online.CodeDestinationRefused, since))
		}
		// An allowed second host serves the snapshot.
		o.setSource(t, map[string]any{"manifest_url": "https://source.test:8443" + manifestPath})
		clearRequests(t)
		p := o.publishOnline(t, variant(t, at("2026-06-11T00:00:00Z"), nil, nil), *at("2026-06-11T00:00:00Z"), nil,
			func(m *online.Manifest) { m.Snapshot.URL = "https://mirror.test:8443/karta/" + m.Snapshot.URL })
		if s := waitSubmission(t, p.name, 90*time.Second); s.State != "published" {
			t.Fatalf("snapshot from the allowed mirror: %+v", s)
		}
		seen := false
		for _, r := range sourceRequests(t) {
			seen = seen || (strings.HasPrefix(r.Host, "mirror.test") && strings.HasSuffix(r.Path, ".osm.pbf"))
		}
		if !seen {
			t.Error("the snapshot was not fetched from the mirror")
		}
	})

	t.Run("size and time bounds", func(t *testing.T) {
		snapName := func() string { return fmt.Sprintf("/karta/snap-%d.osm.pbf", o.serial+1) }
		data := variant(t, at("2026-06-12T00:00:00Z"), nil, nil)
		ts := *at("2026-06-12T00:00:00Z")
		cases := []struct {
			name string
			rule sourceRule
			code string
		}{
			{"declared longer than signed", sourceRule{ExtraBytes: 10}, online.CodeSizeMismatch},
			{"longer than signed, unknown length", sourceRule{ExtraBytes: 10, NoContentLength: true}, online.CodeTooLarge},
			{"compressed body", sourceRule{ContentEncoding: "gzip"}, online.CodeEncoding},
			{"stalled transfer", sourceRule{StallAfter: 100}, online.CodeStalled},
			{"server error", sourceRule{Status: http.StatusServiceUnavailable}, online.CodeHTTPStatus},
		}
		for i, c := range cases {
			since := time.Now()
			setRules(t, map[string]sourceRule{snapName(): c.rule})
			p := o.publishOnline(t, data, ts, nil)
			waitFetcher(t, c.name, 45*time.Second, failedWith(c.code, since))
			noSubmission(t, p.name)
			ts = *at(fmt.Sprintf("2026-06-12T01:%02d:00Z", i))
			data = variant(t, &ts, nil, nil)
		}
		// The whole download is bounded in time even while data flows.
		o.setSource(t, map[string]any{"download_timeout": "10s"})
		since := time.Now()
		setRules(t, map[string]sourceRule{snapName(): {ThrottleBPS: 100}})
		o.publishOnline(t, data, ts, nil)
		waitFetcher(t, "download timeout", 60*time.Second, failedWith(online.CodeTimeout, since))
		o.setSource(t, map[string]any{"download_timeout": "60s"})
		// A snapshot above the size limit is refused without downloading.
		since = time.Now()
		clearRequests(t)
		setRules(t, nil)
		o.publishOnline(t, data, ts, nil, func(m *online.Manifest) { m.Snapshot.SizeBytes = 65 << 20 })
		waitFetcher(t, "size limit", 30*time.Second, failedWith(online.CodeTooLarge, since))
		if n := countRequests(t, snapName()); n != 0 {
			t.Errorf("an oversized snapshot was requested %d times", n)
		}
		// Slow headers hit the response header timeout.
		since = time.Now()
		setRules(t, map[string]sourceRule{manifestPath: {DelayHeaders: "40s"}})
		waitFetcher(t, "slow headers", 60*time.Second, failedWith(online.CodeTimeout, since))
		setRules(t, nil)
		p := o.publishOnline(t, data, ts, nil)
		if s := waitSubmission(t, p.name, 90*time.Second); s.State != "published" {
			t.Fatalf("after the bounds: %+v", s)
		}
	})

	t.Run("a broken transfer resumes; network loss backs off; manual publication keeps working", func(t *testing.T) {
		// The first transfer is cut after 1000 bytes; the next resumes.
		data := variant(t, at("2026-06-13T00:00:00Z"), nil, nil)
		clearRequests(t)
		setRules(t, map[string]sourceRule{fmt.Sprintf("/karta/snap-%d.osm.pbf", o.serial+1): {CloseAfter: 1000, CloseTimes: 1}})
		p := o.publishOnline(t, data, *at("2026-06-13T00:00:00Z"), nil)
		if s := waitSubmission(t, p.name, 90*time.Second); s.State != "published" {
			t.Fatalf("resumed: %+v", s)
		}
		var ranges []string
		for _, r := range sourceRequests(t) {
			if r.Path == fmt.Sprintf("/karta/snap-%d.osm.pbf", p.serial) {
				ranges = append(ranges, r.Range+"|"+r.IfRange)
			}
		}
		if len(ranges) != 2 || ranges[0] != "|" || !strings.HasPrefix(ranges[1], "bytes=1000-|\"") {
			t.Errorf("snapshot requests %v, want a full request then a resumed one with If-Range", ranges)
		}
		setRules(t, nil)
		active := status(t).activeID()

		// Network loss: the fetcher's network is cut.
		id := fetcherContainer(t)
		if out, err := exec.Command("docker", "network", "disconnect", sourceNet, id).CombinedOutput(); err != nil {
			t.Fatalf("disconnect: %v %s", err, out)
		}
		since := time.Now()
		st := waitFetcher(t, "three failed checks", 60*time.Second, func(s *online.State) bool {
			return s.ConsecutiveFailures >= 3 && s.LastError != nil && !s.LastError.At.Before(since)
		})
		if st.NextAttemptAt == nil || st.NextAttemptAt.Sub(st.LastError.At) > 4*time.Second+time.Second {
			t.Errorf("backoff beyond retry_max: %+v", st)
		}
		// The previous release stays healthy, the public manifest does not
		// claim anything about the source, and the operator sees the error.
		waitReady(t, true, "ready")
		m, body := publicManifest(t)
		if m.Release.ReleaseID != active || bytes.Contains(body, []byte("source.test")) {
			t.Errorf("manifest during network loss: %s", body)
		}
		v := onlineStatus(t)
		if v.Online.Fetcher.State == nil || v.Online.Fetcher.State.LastError == nil || v.Online.Fetcher.State.ConsecutiveFailures < 3 {
			t.Errorf("operator status during network loss: %+v", v.Online.Fetcher)
		}
		mr := op(t, http.MethodGet, "/v1/operator/metrics", monitor, nil)
		expectStatus(t, mr, http.StatusOK)
		if !bytes.Contains(mr.body, []byte(`karta_online_last_error{code="network_error"} 1`)) && !bytes.Contains(mr.body, []byte(`karta_online_last_error{code="timeout"} 1`)) {
			t.Errorf("metrics during network loss:\n%s", mr.body)
		}
		// Manual publication works with the source unreachable.
		manual := variant(t, at("2026-06-14T00:00:00Z"), nil, nil)
		authorize(t, manual, "manual snapshot during network loss")
		submit(t, "manual-during-outage", manual, nil, "")
		if s := waitSubmission(t, "manual-during-outage", 90*time.Second); s.State != "published" {
			t.Fatalf("manual during outage: %+v", s)
		}
		if out, err := exec.Command("docker", "network", "connect", "--alias", "fetcher", sourceNet, id).CombinedOutput(); err != nil {
			t.Fatalf("reconnect: %v %s", err, out)
		}
		waitFetcher(t, "recovery after network loss", 30*time.Second, func(s *online.State) bool {
			return s.ConsecutiveFailures == 0 && s.LastError == nil
		})
		// The source itself goes away (stopped), and comes back.
		compose(t, "--profile", "online", "stop", "-t", "1", "source")
		since = time.Now()
		waitFetcher(t, "source down", 60*time.Second, func(s *online.State) bool {
			return s.ConsecutiveFailures >= 1 && s.LastError != nil && !s.LastError.At.Before(since)
		})
		down := variant(t, at("2026-06-15T00:00:00Z"), nil, nil)
		authorize(t, down, "manual snapshot with the source down")
		submit(t, "manual-source-down", down, nil, "")
		if s := waitSubmission(t, "manual-source-down", 90*time.Second); s.State != "published" {
			t.Fatalf("manual with the source down: %+v", s)
		}
		// The command-line importer too (its container has no route out).
		cli := variant(t, at("2026-06-16T00:00:00Z"), nil, nil)
		authorize(t, cli, "CLI import with the source down")
		cliPath := filepath.Join(repoRoot, "data", "local", "online-test-cli.osm.pbf")
		writeFileAtomic(t, cliPath, cli, 0o644)
		res, code, errOut := runImport(t, "/data/local/online-test-cli.osm.pbf", "/config/regions/fixture.json")
		_ = os.Remove(cliPath)
		if code != 0 || res.ReleaseID == "" {
			t.Fatalf("CLI import with the source down: exit %d\n%s", code, errOut)
		}
		waitManifest(t, res.ReleaseID)
		startSource(t)
		waitFetcher(t, "recovery after the source returned", 30*time.Second, func(s *online.State) bool {
			return s.ConsecutiveFailures == 0 && s.LastError == nil
		})
	})

	t.Run("crash and restart at every critical transition", func(t *testing.T) {
		day := 17
		next := func() (published, []byte) {
			day++
			ts := *at(fmt.Sprintf("2026-06-%02dT00:00:00Z", day))
			data := variant(t, &ts, nil, nil)
			return o.publishOnline(t, data, ts, nil), data
		}
		for _, fp := range []string{"fetch.after_manifest", "fetch.mid_download", "fetch.after_download", "fetch.before_marker", "fetch.after_marker"} {
			t.Run(fp, func(t *testing.T) {
				stopFetcher(t)
				clearRequests(t)
				rules := map[string]sourceRule{}
				if fp == "fetch.mid_download" {
					// Slow enough to stop in the middle.
					rules[fmt.Sprintf("/karta/snap-%d.osm.pbf", o.serial+1)] = sourceRule{ThrottleBPS: 400}
				}
				setRules(t, rules)
				var older published
				if fp == "fetch.after_manifest" {
					// A valid manifest with a serial between the last
					// delivered one and the one the crash interrupts; it is
					// served after the crash, as a replay.
					ots := *at(fmt.Sprintf("2026-06-%02dT12:00:00Z", day))
					older = o.stage(t, variant(t, &ots, nil, nil), ots, nil)
				}
				p, data := next()
				startFetcher(t, map[string]string{"KARTA_TEST_FETCH_FAILPOINTS": fp})
				if code := waitFetcherExit(t, 60*time.Second); code != 99 {
					t.Fatalf("fetcher exit %d", code)
				}
				setRules(t, nil)
				if fp == "fetch.after_manifest" {
					// The interrupted check persisted serial p before the
					// crash, so after a restart the older serial is a replay,
					// refused before any snapshot request.
					setManifest(t, older.raw)
					since := time.Now()
					startFetcher(t, nil)
					waitFetcher(t, "the older manifest refused as a replay", 60*time.Second, failedWith(online.CodeManifestReplayed, since))
					stopFetcher(t)
					if st, _ := online.ReadState(onlineDir); st == nil || st.HighestSerial != p.serial {
						t.Errorf("highest serial after the replay: %+v", st)
					}
					olderSnap := fmt.Sprintf("/karta/snap-%d.osm.pbf", older.serial)
					if n := countRequests(t, olderSnap); n != 0 {
						t.Errorf("%d requests for the replayed manifest's snapshot", n)
					}
					noSubmission(t, older.name)
					setManifest(t, p.raw)
				}
				startFetcher(t, nil)
				s := waitSubmission(t, p.name, 120*time.Second)
				if s.State != "published" {
					t.Fatalf("after %s: %+v", fp, s)
				}
				snapPath := fmt.Sprintf("/karta/snap-%d.osm.pbf", p.serial)
				var gets []string
				for _, r := range sourceRequests(t) {
					if r.Path == snapPath {
						gets = append(gets, r.Range)
					}
				}
				switch fp {
				case "fetch.after_manifest":
					if len(gets) != 1 {
						t.Errorf("snapshot requests %v", gets)
					}
				case "fetch.mid_download":
					if len(gets) != 2 || !strings.HasPrefix(gets[1], "bytes=") {
						t.Errorf("snapshot requests %v, want a resumed second request", gets)
					}
				default:
					// The verified bytes survived the crash: one download only.
					if len(gets) != 1 {
						t.Errorf("snapshot requests %v, want one", gets)
					}
				}
				n := 0
				for _, sub := range status(t).Submissions {
					if sub.Name == p.name {
						n++
					}
				}
				if n != 1 {
					t.Errorf("%d submissions for one delivery", n)
				}
				_ = data
			})
		}
		for _, fp := range []string{"stage.after_copy", "online.after_verify", "build.after_import", "activate.before_commit", "activate.after_commit"} {
			t.Run("publisher "+fp, func(t *testing.T) {
				o.restartOnlinePublisher(t, map[string]string{"KARTA_TEST_FAILPOINTS": fp})
				p, _ := next()
				if code := waitPublisherExit(t, 90*time.Second); code != 99 {
					t.Fatalf("publisher exit %d", code)
				}
				o.restartOnlinePublisher(t, nil)
				s := waitSubmission(t, p.name, 120*time.Second)
				want := "published"
				if fp == "activate.after_commit" {
					// The switch committed before the crash; the retry finds it active.
					want = "duplicate"
				}
				if s.State != want {
					t.Fatalf("after %s: %+v %s", fp, s, strOr(s.Reason))
				}
				if v := onlineStatus(t).Online.Verified; v == nil || v.Serial != p.serial {
					t.Errorf("verified %+v", v)
				}
				waitIdle(t)
				assertRegistryConsistent(t)
			})
		}
	})

	t.Run("simultaneous manual and online submissions have a deterministic outcome", func(t *testing.T) {
		// Both are waiting when the publisher scans: the manual inbox is
		// processed first, then the online delivery; the forward rule
		// decides the second.
		race := func(t *testing.T, manual, onlineData []byte, ots time.Time) (submission, submission, string) {
			t.Helper()
			authorize(t, manual, "simultaneous manual")
			compose(t, "stop", "-t", "5", "publisher")
			name := fmt.Sprintf("manual-%d", time.Now().UnixNano())
			submit(t, name, manual, nil, "")
			p := o.publishOnline(t, onlineData, ots, nil)
			waitFetcher(t, "delivery", 60*time.Second, deliveredSerial(p.serial))
			o.restartOnlinePublisher(t, nil)
			ms := waitSubmission(t, name, 120*time.Second)
			osub := waitSubmission(t, p.name, 120*time.Second)
			return ms, osub, status(t).activeID()
		}
		base := 20
		ts := func(d int) time.Time { return *at(fmt.Sprintf("2026-07-%02dT00:00:00Z", d)) }
		// Same snapshot from both sources.
		same := variant(t, ptr(ts(base)), nil, nil)
		ms, os1, _ := race(t, same, same, ts(base))
		if ms.State != "published" || os1.State != "duplicate" || os1.code() != "duplicate_active" {
			t.Errorf("same snapshot: manual %+v, online %+v", ms, os1)
		}
		// Different snapshots with the same timestamp: the manual one wins.
		m2 := variant(t, ptr(ts(base+1)), nil, func(d *pbfwrite.Data) {
			d.Nodes = append(d.Nodes, pbfwrite.Node{ID: 910001, Lat: 70000, Lon: 70000,
				Tags: []pbfwrite.Tag{{K: "amenity", V: "cafe"}, {K: "name", V: "Manual Edition Cafe"}}})
		})
		o2 := variant(t, ptr(ts(base+1)), nil, nil)
		ms, os1, active := race(t, m2, o2, ts(base+1))
		if ms.State != "published" || os1.State != "rejected" || os1.code() != "not_newer" || active != ms.release() {
			t.Errorf("same timestamp: manual %+v, online %+v, active %s", ms, os1, active)
		}
		// The online snapshot is newer: both are built in turn, it ends up active.
		ms, os1, active = race(t, variant(t, ptr(ts(base+2)), nil, nil), variant(t, ptr(ts(base+3)), nil, nil), ts(base+3))
		if ms.State != "published" || os1.State != "published" || active != os1.release() {
			t.Errorf("online newer: manual %+v, online %+v, active %s", ms, os1, active)
		}
		// The manual snapshot is newer: the online one is refused as older.
		ms, os1, active = race(t, variant(t, ptr(ts(base+5)), nil, nil), variant(t, ptr(ts(base+4)), nil, nil), ts(base+4))
		if ms.State != "published" || os1.State != "rejected" || os1.code() != "older_than_active" || active != ms.release() {
			t.Errorf("manual newer: manual %+v, online %+v, active %s", ms, os1, active)
		}
		// A command-line import builds concurrently with an online build:
		// the build lock serializes them, the newest ends up active.
		o.restartOnlinePublisher(t, map[string]string{"KARTA_TEST_OSM2PGSQL": "/usr/local/bin/slow-osm2pgsql"})
		onlineNew := variant(t, ptr(ts(base+7)), nil, nil)
		p := o.publishOnline(t, onlineNew, ts(base+7), nil)
		waitJobPhase(t, "building", 60*time.Second)
		cliData := variant(t, ptr(ts(base+6)), nil, nil)
		authorize(t, cliData, "concurrent CLI import")
		writeFileAtomic(t, filepath.Join(repoRoot, "data", "local", "online-test-concurrent.osm.pbf"), cliData, 0o644)
		_, code, errOut := runImport(t, "/data/local/online-test-concurrent.osm.pbf", "/config/regions/fixture.json")
		_ = os.Remove(filepath.Join(repoRoot, "data", "local", "online-test-concurrent.osm.pbf"))
		s := waitSubmission(t, p.name, 120*time.Second)
		final := status(t).activeID()
		switch {
		case s.State == "published" && code == 4 && strings.Contains(errOut, "older_than_active"):
			// The online build finished first; the older CLI snapshot is refused.
		case s.State == "published" && code == 0:
			// The CLI import took the lock first and published; the newer online one followed.
			if final != s.release() {
				t.Errorf("the newer online release is not active: %s", final)
			}
		default:
			t.Errorf("concurrent CLI import: exit %d, online %+v\n%s", code, s, errOut)
		}
		if final != s.release() {
			t.Errorf("final active %s, want the newest (online) %s", final, s.release())
		}
		o.restartOnlinePublisher(t, nil)
		waitIdle(t)
		assertRegistryConsistent(t)
	})

	t.Run("a rollback during an online build wins and pauses automatic activation", func(t *testing.T) {
		st := status(t)
		var target string
		for _, r := range st.Releases {
			if r.RollbackEligible && r.State == "retired" {
				target = r.ID
				break
			}
		}
		if target == "" {
			t.Fatal("no rollback target")
		}
		o.restartOnlinePublisher(t, map[string]string{"KARTA_TEST_OSM2PGSQL": "/usr/local/bin/slow-osm2pgsql"})
		p := o.publishOnline(t, variant(t, at("2026-08-01T00:00:00Z"), nil, nil), *at("2026-08-01T00:00:00Z"), nil)
		waitJobPhase(t, "building", 60*time.Second)
		r := op(t, http.MethodPost, "/v1/operator/rollback", admin, map[string]any{"release_id": target, "reason": "bad labels in the online data"})
		expectStatus(t, r, http.StatusOK)
		s := waitSubmission(t, p.name, 120*time.Second)
		if s.State != "ready" || s.code() != "active_changed" {
			t.Errorf("online build after the rollback: %+v", s)
		}
		if got := status(t).activeID(); got != target {
			t.Errorf("active %s, want the rollback target %s", got, target)
		}
		v := onlineStatus(t)
		if v.Online.Policy == nil || v.Online.Policy.AutoActivate || v.Online.AutoActivate {
			t.Errorf("automatic activation not paused by the rollback: %+v", v.Online.Policy)
		}
		if a := lastAuditOf(t, "online_pause"); a.Outcome != "succeeded" || a.Source != "operator_api" || a.Detail["cause"] != "rollback" {
			t.Errorf("pause audit %+v", a)
		}
		// A newer online snapshot is built but not activated while paused.
		o.restartOnlinePublisher(t, nil)
		p2 := o.publishOnline(t, variant(t, at("2026-08-02T00:00:00Z"), nil, nil), *at("2026-08-02T00:00:00Z"), nil)
		s2 := waitSubmission(t, p2.name, 90*time.Second)
		if s2.State != "ready" || s2.code() != "online_activation_paused" {
			t.Errorf("while paused: %+v", s2)
		}
		if got := status(t).activeID(); got != target {
			t.Errorf("active changed while paused: %s", got)
		}
		// Pausing again is a no-op, audited; resuming is audited.
		r = op(t, http.MethodPost, "/v1/operator/online/pause", admin, map[string]any{"reason": "keep paused"})
		expectStatus(t, r, http.StatusOK)
		if a := lastAudit(t, "online_pause", "keep paused"); a.Outcome != "noop" {
			t.Errorf("repeated pause %+v", a)
		}
		r = op(t, http.MethodPost, "/v1/operator/online/resume", monitor, map[string]any{"reason": "monitor may not"})
		expectStatus(t, r, http.StatusForbidden)
		r = op(t, http.MethodPost, "/v1/operator/online/resume", admin, map[string]any{"reason": "labels fixed upstream"})
		expectStatus(t, r, http.StatusOK)
		if a := lastAudit(t, "online_resume", "labels fixed upstream"); a.Outcome != "succeeded" {
			t.Errorf("resume audit %+v", a)
		}
		// The release kept ready can be activated explicitly; the next
		// delivery activates automatically.
		r = op(t, http.MethodPost, "/v1/operator/releases/"+s2.release()+"/activate", admin, map[string]any{"reason": "activate the paused online release"})
		expectStatus(t, r, http.StatusOK)
		p3 := o.publishOnline(t, variant(t, at("2026-08-03T00:00:00Z"), nil, nil), *at("2026-08-03T00:00:00Z"), nil)
		if s3 := waitSubmission(t, p3.name, 90*time.Second); s3.State != "published" {
			t.Errorf("after resume: %+v", s3)
		}
	})

	t.Run("the signed authorization is checked again at the switch", func(t *testing.T) {
		o.restartOnlinePublisher(t, map[string]string{"KARTA_TEST_OSM2PGSQL": "/usr/local/bin/slow-osm2pgsql"})
		t.Cleanup(func() {
			holdImports(t, false)
			o.setSource(t, map[string]any{"trusted_keys": o.defaultSource()["trusted_keys"]})
		})
		active := status(t).activeID()
		mustStay := func(t *testing.T, what, candidate string) {
			t.Helper()
			st := status(t)
			if got := st.activeID(); got != active {
				t.Errorf("%s: active %s, want %s unchanged", what, got, active)
			}
			if r := st.release(candidate); r == nil || r.State != "ready" {
				t.Errorf("%s: candidate %s not kept ready: %+v", what, candidate, r)
			}
			if m, _ := publicManifest(t); m.Release.ReleaseID != active {
				t.Errorf("%s: public manifest serves %s", what, m.Release.ReleaseID)
			}
		}

		// The manifest expires while the build is held: the switch is refused.
		holdImports(t, true)
		ts := *at("2026-08-03T06:00:00Z")
		expires := time.Now().Add(35 * time.Second).UTC().Truncate(time.Second)
		p := o.publishOnline(t, variant(t, &ts, nil, nil), ts, nil, func(m *online.Manifest) { m.ExpiresAt = expires })
		waitJobPhase(t, "building", 60*time.Second) // verified before it expired
		time.Sleep(time.Until(expires.Add(2 * time.Second)))
		holdImports(t, false)
		s := waitSubmission(t, p.name, 120*time.Second)
		if s.State != "rejected" || s.code() != online.CodeManifestExpired || s.release() == "" || !strings.Contains(strOr(s.Reason), "at activation") {
			t.Fatalf("manifest expired during the build: %+v %s", s, strOr(s.Reason))
		}
		candidate := s.release()
		mustStay(t, "expired", candidate)

		// A retry verifies the same manifest again: still expired, refused
		// before anything is built.
		r := op(t, http.MethodPost, "/v1/operator/online/retry", admin, map[string]any{"reason": "retry the expired delivery"})
		expectStatus(t, r, http.StatusOK)
		deadline := time.Now().Add(90 * time.Second)
		for s.Attempts < 2 && time.Now().Before(deadline) {
			time.Sleep(time.Second)
			s = waitSubmission(t, p.name, 10*time.Second)
		}
		if s.State != "rejected" || s.code() != online.CodeManifestExpired || s.Attempts != 2 {
			t.Fatalf("retry of an expired delivery: %+v", s)
		}
		mustStay(t, "expired, retried", candidate)

		// A fresh signed manifest for the same snapshot publishes the kept
		// candidate; the fetcher delivers it without downloading again.
		clearRequests(t)
		fresh := o.resign(t, p, "test-2026a", o.key)
		if s := waitSubmission(t, fresh.name, 120*time.Second); s.State != "published" || s.release() != candidate {
			t.Fatalf("fresh manifest for the kept candidate: %+v %s", s, strOr(s.Reason))
		}
		waitManifest(t, candidate)
		if n := countRequests(t, "/karta/"+p.manifest.Snapshot.URL); n != 0 {
			t.Errorf("the snapshot was downloaded again (%d requests)", n)
		}
		active = candidate

		// The signing key is removed from the source file while the build
		// is held: the switch is refused.
		holdImports(t, true)
		ts2 := *at("2026-08-03T12:00:00Z")
		p2 := o.publishOnline(t, variant(t, &ts2, nil, nil), ts2, nil)
		waitJobPhase(t, "building", 60*time.Second)
		o.setSource(t, map[string]any{"trusted_keys": []map[string]string{{"id": "test-2026b", "ed25519_public_key": b64(o.other.Public().(ed25519.PublicKey))}}})
		holdImports(t, false)
		s2 := waitSubmission(t, p2.name, 120*time.Second)
		if s2.State != "rejected" || s2.code() != online.CodeSignatureUntrusted || s2.release() == "" {
			t.Fatalf("key removed during the build: %+v %s", s2, strOr(s2.Reason))
		}
		mustStay(t, "key removed", s2.release())

		// A fresh manifest by the key now trusted publishes it.
		fresh2 := o.resign(t, p2, "test-2026b", o.other)
		if s := waitSubmission(t, fresh2.name, 120*time.Second); s.State != "published" || s.release() != s2.release() {
			t.Fatalf("fresh manifest by the new key: %+v %s", s, strOr(s.Reason))
		}
		waitManifest(t, s2.release())

		o.setSource(t, map[string]any{"trusted_keys": o.defaultSource()["trusted_keys"]})
		o.restartOnlinePublisher(t, nil)
		waitIdle(t)
		assertRegistryConsistent(t)
	})

	t.Run("an online delivery refused for storage is retried on request", func(t *testing.T) {
		o.restartOnlinePublisher(t, map[string]string{"KARTA_TEST_BUDGET_MB": "1"})
		p := o.publishOnline(t, variant(t, at("2026-08-04T00:00:00Z"), nil, nil), *at("2026-08-04T00:00:00Z"), nil)
		s := waitSubmission(t, p.name, 90*time.Second)
		if s.State != "rejected" || s.code() != "insufficient_storage" {
			t.Fatalf("budget: %+v", s)
		}
		o.restartOnlinePublisher(t, nil)
		time.Sleep(3 * time.Second)
		if s := waitSubmission(t, p.name, 10*time.Second); s.State != "rejected" {
			t.Fatalf("retried without a request: %+v", s)
		}
		r := op(t, http.MethodPost, "/v1/operator/online/retry", admin, map[string]any{"reason": "budget raised"})
		expectStatus(t, r, http.StatusOK)
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if s = waitSubmission(t, p.name, 10*time.Second); s.State == "published" {
				break
			}
			time.Sleep(time.Second)
		}
		if s.State != "published" || s.Attempts != 2 {
			t.Errorf("after retry: %+v", s)
		}
		r = op(t, http.MethodPost, "/v1/operator/online/retry", admin, map[string]any{"reason": "nothing left"})
		if code, _ := r.errorCode(t); r.status != http.StatusConflict || code != "nothing_to_retry" {
			t.Errorf("second retry: %d %s", r.status, r.body)
		}
	})

	t.Run("pinned clients, cleanup and the outbox stay bounded", func(t *testing.T) {
		// (Pinned clients through an online switch: the first subtest.)
		o.restartOnlinePublisher(t, map[string]string{"KARTA_TEST_RETAIN": "1", "KARTA_TEST_PIN_GRACE": "1s"})
		x := status(t).activeID()
		var rels []string
		for i := 0; i < 3; i++ {
			ts := *at(fmt.Sprintf("2026-08-%02dT00:00:00Z", 10+i))
			p := o.publishOnline(t, variant(t, &ts, nil, nil), ts, nil)
			s := waitSubmission(t, p.name, 90*time.Second)
			if s.State != "published" {
				t.Fatalf("%d: %+v", i, s)
			}
			rels = append(rels, s.release())
		}
		waitIdle(t)
		// X, P1 and P2 were retired with a 1 s grace; wait out the grace and
		// the 30 s cleanup margin. (Releases retired earlier by command-line
		// imports keep the importer's 24 h grace and stay, as pinned.)
		mine := map[string]bool{x: true, rels[0]: true, rels[1]: true}
		type decision struct {
			ReleaseID string `json:"release_id"`
			Action    string `json:"action"`
			Why       string `json:"why"`
		}
		var res struct {
			Decisions []decision `json:"decisions"`
			Removed   []string   `json:"removed"`
			Skipped   []struct {
				ReleaseID string `json:"release_id"`
				Why       string `json:"why"`
			} `json:"skipped"`
		}
		deadline := time.Now().Add(120 * time.Second)
		for {
			r := op(t, http.MethodPost, "/v1/operator/cleanup", admin, map[string]any{"reason": "dry run after online publications", "dry_run": true})
			expectStatus(t, r, http.StatusOK)
			r.json(t, &res)
			waiting := 0
			for _, d := range res.Decisions {
				if mine[d.ReleaseID] && strings.HasPrefix(d.Why, "pinned by clients") {
					waiting++
				}
			}
			if waiting == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("releases still pinned: %+v", res.Decisions)
			}
			time.Sleep(3 * time.Second)
		}
		r := op(t, http.MethodPost, "/v1/operator/cleanup", admin, map[string]any{"reason": "after online publications"})
		expectStatus(t, r, http.StatusOK)
		r.json(t, &res)
		removed := map[string]bool{}
		for _, id := range res.Removed {
			removed[id] = true
		}
		pinned := 0
		for _, d := range res.Decisions {
			switch {
			case d.Action == "remove" && !removed[d.ReleaseID]:
				t.Errorf("%s was selected for removal but not removed: %+v", d.ReleaseID, res.Skipped)
			case d.Action == "keep" && strings.HasPrefix(d.Why, "pinned by clients"):
				pinned++
			}
		}
		if !removed[x] || !removed[rels[0]] {
			t.Errorf("the releases replaced by online publications were not removed: %v", res.Removed)
		}
		st := status(t)
		if st.activeID() != rels[2] {
			t.Errorf("active %s, want %s", st.activeID(), rels[2])
		}
		if r := st.release(rels[1]); r == nil || r.State != "retired" {
			t.Errorf("the rollback target %s: %+v", rels[1], r)
		}
		kept := 0
		for _, rel := range st.Releases {
			if rel.State == "ready" || rel.State == "retired" || rel.State == "active" {
				kept++
			}
		}
		if kept != 2+pinned { // the active release, one rollback target (KARTA_RETAIN_RELEASES=1) and pinned ones
			t.Errorf("%d releases retained after cleanup, want %d; decisions %+v", kept, 2+pinned, res.Decisions)
		}
		files, _ := os.ReadDir(onlineDir)
		complete := 0
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".osm.pbf.ready") {
				complete++
			}
		}
		if complete > 2 {
			t.Errorf("%d deliveries kept in the outbox, want at most 2", complete)
		}
		out, _, _ := compose(t, "exec", "-T", "publisher", "ls", "-A", "/var/lib/karta/staging")
		if strings.TrimSpace(strings.ReplaceAll(out, ".lock", "")) != "" {
			t.Errorf("staging not empty: %q", out)
		}
		if p, _ := os.ReadDir(filepath.Join(onlineDir, online.PartialDirName)); len(p) != 0 {
			t.Errorf("partial downloads left: %v", p)
		}
		assertRegistryConsistent(t)
		o.restartOnlinePublisher(t, nil)
	})

	t.Run("a bearer token reaches only the source, never logs, URLs or APIs", func(t *testing.T) {
		token := "karta-test-token-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		writeFileAtomic(t, filepath.Join(sourceCfgDir, "token"), []byte(token+"\n"), 0o644)
		o.setSource(t, map[string]any{"auth_token_file": "/config/sources/token"})
		clearRequests(t)
		p := o.publishOnline(t, variant(t, at("2026-08-20T00:00:00Z"), nil, nil), *at("2026-08-20T00:00:00Z"), nil)
		if s := waitSubmission(t, p.name, 90*time.Second); s.State != "published" {
			t.Fatalf("%+v", s)
		}
		for _, r := range sourceRequests(t) {
			if r.Auth != "Bearer "+token || r.Query != "" {
				t.Errorf("request %+v", r)
			}
		}
		st := op(t, http.MethodGet, "/v1/operator/status", monitor, nil)
		_, man := publicManifest(t)
		for name, b := range map[string]string{"fetcher log": fetcherLogs(t), "publisher log": publisherLogs(t),
			"operator status": string(st.body), "public manifest": string(man)} {
			if strings.Contains(b, token) {
				t.Errorf("the token appears in the %s", name)
			}
		}
		o.setSource(t, map[string]any{"auth_token_file": nil})
	})

	t.Run("freshness and metrics report the data age, not the source", func(t *testing.T) {
		o.restartOnlinePublisher(t, map[string]string{"KARTA_TEST_STALE_AFTER": "1h"})
		deadline := time.Now().Add(20 * time.Second)
		var m manifestView
		for time.Now().Before(deadline) {
			if m, _ = publicManifest(t); m.Freshness.StaleAfterSeconds != nil && *m.Freshness.StaleAfterSeconds == 3600 {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		// The fixture data is months old: stale, whatever the source says.
		if m.Freshness.Stale == nil || !*m.Freshness.Stale || m.Freshness.UpdateMode != "online" {
			t.Errorf("public freshness %+v", m.Freshness)
		}
		v := onlineStatus(t)
		if v.Freshness.Stale == nil || !*v.Freshness.Stale || v.Freshness.StaleAfterSeconds == nil || *v.Freshness.StaleAfterSeconds != 3600 ||
			v.Freshness.ActiveDataAgeSeconds == nil {
			t.Errorf("operator freshness %+v", v.Freshness)
		}
		r := op(t, http.MethodGet, "/v1/operator/metrics", monitor, nil)
		expectStatus(t, r, http.StatusOK)
		for _, want := range []string{"karta_online_enabled 1", "karta_data_stale 1", "karta_data_stale_after_seconds 3600",
			"karta_online_auto_activation 1", "karta_online_verified_serial ", "karta_active_data_age_seconds ",
			"karta_online_last_success_timestamp_seconds ", "karta_online_next_attempt_timestamp_seconds ",
			"karta_online_fetcher_state_age_seconds ", `karta_submissions{source="online",state="published"} `} {
			if !bytes.Contains(r.body, []byte(want)) {
				t.Errorf("metrics lack %q", want)
			}
		}
		recordMeasurement(t, "online_metrics_sample", string(r.body))
		r = op(t, http.MethodGet, "/v1/operator/metrics", "", nil)
		expectStatus(t, r, http.StatusUnauthorized)
		o.restartOnlinePublisher(t, nil)
	})

	t.Run("a large download keeps memory bounded and a structurally invalid file leaves serving untouched", func(t *testing.T) {
		before := status(t).activeID()
		big := make([]byte, 32<<20)
		_, _ = rand.Read(big)
		clearRequests(t)
		stopMem := make(chan struct{})
		mem := sampleFetcherMemory(fetcherContainer(t), stopMem)
		start := time.Now()
		p := o.publishOnline(t, big, *at("2026-09-01T00:00:00Z"), nil)
		waitFetcher(t, "large delivery", 120*time.Second, deliveredSerial(p.serial))
		secs := time.Since(start).Seconds()
		close(stopMem)
		peak := <-mem
		s := waitSubmission(t, p.name, 120*time.Second)
		if s.State != "rejected" || s.code() != "malformed_snapshot" {
			t.Errorf("random bytes: %+v", s)
		}
		if got := status(t).activeID(); got != before {
			t.Errorf("active changed to %s", got)
		}
		recordMeasurement(t, "online_large_download", map[string]any{"bytes": len(big), "seconds_until_delivered": secs,
			"fetcher_peak_memory_bytes_sampled": peak, "fetcher_cgroup_peak_bytes_including_page_cache": cgroupPeak(fetcherContainer(t)),
			"fetcher_memory_limit_bytes": 256 << 20})
		if peak > 64<<20 {
			t.Errorf("fetcher memory peaked at %d bytes for a %d-byte download", peak, len(big))
		}
		waitReady(t, true, "ready")
	})
}

// --- small helpers ------------------------------------------------------------------

// waitIdle waits until the publisher runs no publication (including the
// retention cleanup that follows one).
func waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if status(t).Job == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the publisher did not become idle")
}

func ptr(t time.Time) *time.Time { return &t }

// resign serves a fresh manifest (the next serial, a new validity window)
// for p's snapshot, signed by key id with k.
func (o *onlineSuite) resign(t *testing.T, p published, id string, k ed25519.PrivateKey) published {
	t.Helper()
	o.serial++
	now := time.Now().UTC().Truncate(time.Second)
	m := p.manifest
	m.Serial, m.IssuedAt, m.ExpiresAt = o.serial, now.Add(-time.Minute), now.Add(12*time.Hour)
	raw, err := online.Sign(m, online.Signer{KeyID: id, Key: k})
	if err != nil {
		t.Fatal(err)
	}
	setManifest(t, raw)
	return published{serial: o.serial, data: p.data, raw: raw, name: online.DeliveryName("fixture", m.Serial, m.Snapshot.SHA256), manifest: m}
}

// holdImports makes slow-osm2pgsql wait, after its delay, until released.
func holdImports(t *testing.T, on bool) {
	t.Helper()
	args := []string{"exec", "-T", "publisher", "rm", "-f", "/tmp/karta-hold-import"}
	if on {
		args = []string{"exec", "-T", "publisher", "touch", "/tmp/karta-hold-import"}
	}
	if _, stderr, code := compose(t, args...); code != 0 {
		t.Fatalf("hold imports %v: %s", on, stderr)
	}
}

func mustSign(t *testing.T, m online.Manifest, k ed25519.PrivateKey) []byte {
	t.Helper()
	raw, err := online.Sign(m, online.Signer{KeyID: "test-2026a", Key: k})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func testOtherKey() ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	_, _ = rand.Read(seed)
	return ed25519.NewKeyFromSeed(seed)
}

// lastAuditOf returns the newest audit record of an action.
func lastAuditOf(t *testing.T, action string) auditRecord {
	t.Helper()
	r := op(t, http.MethodGet, "/v1/operator/audit?limit=200", secret(t, "operator_monitor_token"), nil)
	expectStatus(t, r, http.StatusOK)
	var a struct {
		Entries []auditRecord `json:"entries"`
	}
	r.json(t, &a)
	for _, e := range a.Entries {
		if e.Action == action {
			return e
		}
	}
	t.Fatalf("no audit record for %s", action)
	return auditRecord{}
}
