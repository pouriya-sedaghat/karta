//go:build integration

package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/online"
)

type fixtureResponse struct {
	manifest           []byte
	manifestForRequest func() []byte
	snapshot           []byte
	provenance         []byte
	mode               string
	gate               <-chan struct{}
}

type onlineState struct {
	Enabled           bool       `json:"enabled"`
	Paused            bool       `json:"paused"`
	LastAttempt       *time.Time `json:"last_attempt"`
	LastCheck         *time.Time `json:"last_successful_check"`
	VerifiedDigest    *string    `json:"verified_digest"`
	LastError         *string    `json:"last_error"`
	NextAttempt       *time.Time `json:"next_attempt"`
	Failures          int        `json:"consecutive_failures"`
	ActiveAgeSeconds  *float64   `json:"active_age_seconds"`
	StaleAfterSeconds float64    `json:"stale_after_seconds"`
}

func onlineStatus(t *testing.T) (opStatus, onlineState) {
	t.Helper()
	r := op(t, http.MethodGet, "/v1/operator/status", secret(t, "operator_monitor_token"), nil)
	expectStatus(t, r, http.StatusOK)
	var v struct {
		Online onlineState `json:"online"`
	}
	r.json(t, &v)
	return status(t), v.Online
}

func waitOnlineAttempt(t *testing.T, previous *time.Time, wantError string) (opStatus, onlineState) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		st, online := onlineStatus(t)
		if online.LastAttempt != nil && (previous == nil || online.LastAttempt.After(*previous)) {
			got := ""
			if online.LastError != nil {
				got = *online.LastError
			}
			if got != wantError {
				t.Fatalf("online error %q, want %q; state %+v", got, wantError, online)
			}
			if online.NextAttempt == nil || !online.NextAttempt.After(*online.LastAttempt) {
				t.Fatalf("online retry not scheduled: %+v", online)
			}
			return st, online
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("online attempt did not finish with %q", wantError)
	return opStatus{}, onlineState{}
}

func signedOnlineManifest(t *testing.T, key ed25519.PrivateKey, rawURL string, snapshot []byte, timestamp time.Time) []byte {
	return signedOnlineManifestFor(t, key, rawURL, snapshot, timestamp, time.Hour)
}

func signedOnlineManifestFor(t *testing.T, key ed25519.PrivateKey, rawURL string, snapshot []byte, timestamp time.Time, validity time.Duration) []byte {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	m := online.Manifest{RegionID: "fixture", IssuedAt: now, ExpiresAt: now.Add(validity),
		DataTimestamp: timestamp, SHA256: digestOf(snapshot), SizeBytes: int64(len(snapshot)),
		SnapshotURL: rawURL + "/snapshot"}
	return signOnlinePayload(t, key, m)
}

func signOnlinePayload(t *testing.T, key ed25519.PrivateKey, m online.Manifest) []byte {
	t.Helper()
	payload, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]string{
		"payload":   base64.StdEncoding.EncodeToString(payload),
		"signature": base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestOnlinePublisher exercises the real Compose publisher and public API with
// a local signed TLS source. Only the disposable onlinefixture image may dial
// that source; the production client's private-address rejection stays intact.
func TestOnlinePublisher(t *testing.T) {
	if inboxDir == "" {
		t.Fatal("KARTA_INBOX_HOST_DIR is not set")
	}
	resetAll(t)
	t.Cleanup(func() { resetAll(t) })
	a, b := readRepo(t, snapA), readRepo(t, snapB)
	submit(t, "online-baseline", a, nil, "")
	first := waitSubmission(t, "online-baseline", 90*time.Second)
	if first.State != "published" {
		t.Fatalf("baseline: %+v", first)
	}
	waitManifest(t, first.release())

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(inboxDir, ".online-key")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(pub)), 0o644); err != nil {
		t.Fatal(err)
	}
	var reply atomic.Pointer[fixtureResponse]
	var snapshotRequests atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := reply.Load()
		if v == nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/manifest":
			manifest := v.manifest
			if v.manifestForRequest != nil {
				manifest = v.manifestForRequest()
			}
			_, _ = w.Write(manifest)
		case "/snapshot":
			snapshotRequests.Add(1)
			switch v.mode {
			case "redirect":
				http.Redirect(w, r, "https://example.com/private", http.StatusFound)
			case "broken":
				_, _ = w.Write(v.snapshot[:len(v.snapshot)/2])
			case "slow":
				select {
				case <-v.gate:
					_, _ = w.Write(v.snapshot)
				case <-r.Context().Done():
				}
			default:
				_, _ = w.Write(v.snapshot)
			}
		case "/provenance":
			_, _ = w.Write(v.provenance)
		default:
			http.NotFound(w, r)
		}
	}))
	_ = server.Listener.Close()
	server.Listener, err = net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server.StartTLS()
	defer server.Close()
	cert, err := x509.ParseCertificate(server.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(inboxDir, ".online-cert")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	port := server.Listener.Addr().(*net.TCPAddr).Port
	sourceURL := fmt.Sprintf("https://example.com:%d", port)
	settings := map[string]string{
		"KARTA_TEST_ONLINE_MANIFEST_URL": sourceURL + "/manifest",
		"KARTA_TEST_ONLINE_KEY_FILE":     "/data/inbox/.online-key",
		"KARTA_TEST_ONLINE_CERT_FILE":    "/data/inbox/.online-cert",
		"KARTA_TEST_ONLINE_DIAL_ADDR":    fmt.Sprintf("host.docker.internal:%d", port),
	}
	trigger := func() {
		t.Helper()
		reg := superuser(t, "karta_registry")
		if _, err := reg.Exec(context.Background(), `UPDATE registry.online_state SET next_attempt = NULL WHERE singleton`); err != nil {
			t.Fatal(err)
		}
		reg.Close(context.Background())
		restartPublisher(t, settings)
	}
	triggerWith := func(extra map[string]string) {
		t.Helper()
		reg := superuser(t, "karta_registry")
		if _, err := reg.Exec(context.Background(), `UPDATE registry.online_state SET next_attempt = NULL WHERE singleton`); err != nil {
			t.Fatal(err)
		}
		reg.Close(context.Background())
		configured := make(map[string]string, len(settings)+len(extra))
		for k, v := range settings {
			configured[k] = v
		}
		for k, v := range extra {
			configured[k] = v
		}
		restartPublisher(t, configured)
	}
	timestamp := *at("2026-02-01T00:00:00Z")
	valid := signedOnlineManifest(t, priv, sourceURL, b, timestamp)
	_, wrong, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	reply.Store(&fixtureResponse{manifest: signedOnlineManifest(t, wrong, sourceURL, b, timestamp), snapshot: b})
	trigger()
	st, failed := waitOnlineAttempt(t, nil, "manifest_invalid")
	if st.activeID() != first.release() || failed.LastCheck != nil || failed.Failures != 1 {
		t.Fatalf("invalid signature changed publication: %+v %+v", st.Active, failed)
	}

	for _, mode := range []string{"redirect", "broken"} {
		reply.Store(&fixtureResponse{manifest: valid, snapshot: b, mode: mode})
		previous := failed.LastAttempt
		trigger()
		st, failed = waitOnlineAttempt(t, previous, "download_failed")
		if st.activeID() != first.release() || failed.LastCheck == nil || failed.VerifiedDigest == nil || *failed.VerifiedDigest != digestOf(b) {
			t.Fatalf("%s changed publication: %+v %+v", mode, st.Active, failed)
		}
	}
	gate := make(chan struct{})
	reply.Store(&fixtureResponse{manifest: valid, snapshot: b, mode: "slow", gate: gate})
	previous := failed.LastAttempt
	trigger()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := onlineStatus(t); st.activeID() != first.release() {
			t.Fatalf("active release changed during download: %s", st.activeID())
		}
		if s := status(t); s.OnlineJob != nil && s.OnlineJob.Phase == "downloading" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	load := startLoad(2)
	expectStatus(t, get(t, "/v1/releases/"+first.release()+lakeTile), http.StatusOK)
	time.Sleep(time.Second) // ensure the load sampler sees the old release
	close(gate)
	st, checked := waitOnlineAttempt(t, previous, "")
	if checked.VerifiedDigest == nil || *checked.VerifiedDigest != digestOf(b) || checked.LastCheck == nil ||
		st.activeID() == first.release() || st.activeID() == "" {
		t.Fatalf("valid signed snapshot not published: %+v %+v", st.Active, checked)
	}
	waitManifest(t, st.activeID())
	if got := load.finish(t, "online download and activation"); len(got.Releases) != 2 {
		t.Errorf("load observed %v, want old then new release", got.Releases)
	} else {
		recordMeasurement(t, "load_during_online_download_and_switch", got)
	}
	if _, next := onlineStatus(t); !next.Enabled || next.NextAttempt == nil || next.Failures != 0 {
		t.Fatalf("successful source check not visible: %+v", next)
	}

	// A signed repeat is a check, not a second download or publication.
	previous = checked.LastAttempt
	beforeRequests := snapshotRequests.Load()
	trigger()
	duplicateStatus, duplicate := waitOnlineAttempt(t, previous, "")
	if duplicateStatus.activeID() != st.activeID() || snapshotRequests.Load() != beforeRequests ||
		duplicate.VerifiedDigest == nil || *duplicate.VerifiedDigest != digestOf(b) {
		t.Fatalf("duplicate source was re-imported: %+v %+v", duplicateStatus.Active, duplicate)
	}

	// The signed sidecar claim must match the complete downloaded bytes.
	// Give this failed candidate a newer timestamp and separate fingerprint;
	// the earlier b candidate must remain within its retry budget.
	prov := []byte(`{"source":"fixture"}`)
	provTimestamp := *at("2026-02-15T00:00:00Z")
	provSnapshot := variant(t, &provTimestamp, nil, nil)
	provManifest := online.Manifest{RegionID: "fixture", IssuedAt: time.Now().UTC().Truncate(time.Second),
		ExpiresAt: time.Now().UTC().Add(time.Hour), DataTimestamp: provTimestamp,
		SHA256: digestOf(provSnapshot), SizeBytes: int64(len(provSnapshot)), SnapshotURL: sourceURL + "/snapshot",
		ProvenanceURL: sourceURL + "/provenance", ProvenanceSHA256: digestOf(prov), ProvenanceSizeBytes: int64(len(prov))}
	reply.Store(&fixtureResponse{manifest: signOnlinePayload(t, priv, provManifest), snapshot: provSnapshot, provenance: []byte(`{"source":"corrupt"}`)})
	previous = duplicate.LastAttempt
	trigger()
	provenanceStatus, provenanceState := waitOnlineAttempt(t, previous, "download_failed")
	if provenanceStatus.activeID() != st.activeID() || provenanceState.VerifiedDigest == nil || *provenanceState.VerifiedDigest != digestOf(provSnapshot) {
		t.Fatalf("invalid signed provenance affected active release: %+v %+v", provenanceStatus.Active, provenanceState)
	}

	// Signed but older data must be refused without fetching its snapshot.
	previous = provenanceState.LastAttempt
	beforeRequests = snapshotRequests.Load()
	reply.Store(&fixtureResponse{manifest: signedOnlineManifest(t, priv, sourceURL, a, *at("2026-01-01T00:00:00Z")), snapshot: a})
	trigger()
	olderStatus, older := waitOnlineAttempt(t, previous, "source_conflict")
	if olderStatus.activeID() != st.activeID() || snapshotRequests.Load() != beforeRequests {
		t.Fatalf("older source affected active release: %+v %+v", olderStatus.Active, older)
	}

	// An explicit rollback pauses automatic activation in the pointer
	// transaction, including a newer online candidate built after rollback.
	r := op(t, http.MethodPost, "/v1/operator/rollback", secret(t, "operator_token"),
		map[string]any{"release_id": first.release(), "reason": "online acceptance rollback"})
	expectStatus(t, r, http.StatusOK)
	waitManifest(t, first.release())
	if _, policy := onlineStatus(t); !policy.Paused {
		t.Fatal("operator rollback did not pause online activation")
	}
	c := variant(t, at("2026-03-01T00:00:00Z"), nil, nil)
	reply.Store(&fixtureResponse{manifest: signedOnlineManifest(t, priv, sourceURL, c, *at("2026-03-01T00:00:00Z")), snapshot: c})
	previous = older.LastAttempt
	trigger()
	pausedStatus, paused := waitOnlineAttempt(t, previous, "")
	if pausedStatus.activeID() != first.release() || !paused.Paused {
		t.Fatalf("online candidate overwrote rollback: %+v %+v", pausedStatus.Active, paused)
	}
	var held bool
	for _, sub := range pausedStatus.Submissions {
		if sub.Name == digestOf(c)[:16] && sub.State == "ready" && sub.code() == "online_paused" {
			held = true
		}
	}
	if !held {
		t.Fatalf("paused online candidate was not retained ready: %+v", pausedStatus.Submissions)
	}
	r = op(t, http.MethodPost, "/v1/operator/online/resume", secret(t, "operator_token"),
		map[string]any{"reason": "resume signed fixture after rollback check"})
	expectStatus(t, r, http.StatusOK)
	previous = paused.LastAttempt
	trigger()
	resumedStatus, resumed := waitOnlineAttempt(t, previous, "")
	if resumed.Paused || resumedStatus.activeID() == first.release() {
		t.Fatalf("ready candidate did not activate on resume: %+v %+v", resumedStatus.Active, resumed)
	}
	waitManifest(t, resumedStatus.activeID())

	// A failing source does not block a manually authorized inbox snapshot.
	previous = resumed.LastAttempt
	reply.Store(&fixtureResponse{manifest: []byte(`{"payload":"invalid","signature":"invalid"}`)})
	trigger()
	failedStatus, offline := waitOnlineAttempt(t, previous, "manifest_invalid")
	if failedStatus.activeID() != resumedStatus.activeID() || offline.Failures != 1 {
		t.Fatalf("source failure changed active release: %+v %+v", failedStatus.Active, offline)
	}
	d := variant(t, at("2026-04-01T00:00:00Z"), nil, nil)
	authorize(t, d, "offline manual import while the signed source fails")
	submit(t, "manual-while-offline", d, nil, "")
	manual := waitSubmission(t, "manual-while-offline", 90*time.Second)
	if manual.State != "published" || manual.release() == resumedStatus.activeID() {
		t.Fatalf("offline manual publication failed: %+v", manual)
	}
	waitManifest(t, manual.release())

	// A crash after a complete download, before Verify, leaves no partial
	// release. Startup recovery reclaims the same digest and publishes it.
	e := variant(t, at("2026-05-01T00:00:00Z"), nil, nil)
	reply.Store(&fixtureResponse{manifest: signedOnlineManifest(t, priv, sourceURL, e, *at("2026-05-01T00:00:00Z")), snapshot: e})
	reg := superuser(t, "karta_registry")
	if _, err := reg.Exec(context.Background(), `UPDATE registry.online_state SET next_attempt = NULL WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	reg.Close(context.Background())
	crashSettings := make(map[string]string, len(settings)+1)
	for k, v := range settings {
		crashSettings[k] = v
	}
	crashSettings["KARTA_TEST_FAILPOINTS"] = "online.after_download"
	// The failpoint can exit before the operator API is ready to answer;
	// recreate directly rather than calling restartPublisher.
	if _, stderr, code := composeEnv(t, settingsEnv(crashSettings), "up", "-d", "--no-deps", "--force-recreate", "publisher"); code != 0 {
		t.Fatalf("start crash publisher: %s", stderr)
	}
	if code := waitPublisherExit(t, 60*time.Second); code != 99 {
		t.Fatalf("online download failpoint exit %d, want 99", code)
	}
	if got := activeFromRegistry(t); got != manual.release() {
		t.Fatalf("online crash moved active pointer to %s", got)
	}
	previous = offline.LastAttempt
	restartPublisher(t, settings)
	recoveredStatus, recovered := waitOnlineAttempt(t, previous, "")
	if recoveredStatus.activeID() == manual.release() || recovered.VerifiedDigest == nil || *recovered.VerifiedDigest != digestOf(e) {
		t.Fatalf("online crash did not recover: %+v %+v", recoveredStatus.Active, recovered)
	}
	waitManifest(t, recoveredStatus.activeID())

	// Rollback during the online build must win over the candidate that began
	// earlier; the candidate stays ready and auto activation remains paused.
	f := variant(t, at("2026-06-01T00:00:00Z"), nil, nil)
	reply.Store(&fixtureResponse{manifest: signedOnlineManifest(t, priv, sourceURL, f, *at("2026-06-01T00:00:00Z")), snapshot: f})
	previous = recovered.LastAttempt
	triggerWith(map[string]string{"KARTA_TEST_OSM2PGSQL": "/usr/local/bin/slow-osm2pgsql"})
	deadline = time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if s := status(t); s.OnlineJob != nil && s.OnlineJob.Phase == "building" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if s := status(t); s.OnlineJob == nil || s.OnlineJob.Phase != "building" {
		t.Fatal("online publisher never reached the slow build")
	}
	r = op(t, http.MethodPost, "/v1/operator/rollback", secret(t, "operator_token"),
		map[string]any{"release_id": manual.release(), "reason": "rollback during online build"})
	expectStatus(t, r, http.StatusOK)
	finalStatus, finalCheck := waitOnlineAttempt(t, previous, "")
	if finalStatus.activeID() != manual.release() || !finalCheck.Paused {
		t.Fatalf("online build overwrote an operator rollback: %+v %+v", finalStatus.Active, finalCheck)
	}
	waitManifest(t, manual.release())

	// A manifest can expire during a long build. Its ready release must not
	// switch the pointer until a fresh signature authorizes the same digest.
	r = op(t, http.MethodPost, "/v1/operator/online/resume", secret(t, "operator_token"),
		map[string]any{"reason": "test authorization expiry during build"})
	expectStatus(t, r, http.StatusOK)
	g := variant(t, at("2026-07-01T00:00:00Z"), nil, nil)
	reply.Store(&fixtureResponse{manifestForRequest: func() []byte {
		return signedOnlineManifestFor(t, priv, sourceURL, g, *at("2026-07-01T00:00:00Z"), 5*time.Second)
	}, snapshot: g})
	previous = finalCheck.LastAttempt
	triggerWith(map[string]string{"KARTA_TEST_OSM2PGSQL": "/usr/local/bin/slow-osm2pgsql"})
	expiredStatus, expired := waitOnlineAttempt(t, previous, "manifest_expired")
	if expiredStatus.activeID() != manual.release() || expired.VerifiedDigest == nil || *expired.VerifiedDigest != digestOf(g) {
		t.Fatalf("expired authorization moved the pointer: %+v %+v", expiredStatus.Active, expired)
	}
	reply.Store(&fixtureResponse{manifest: signedOnlineManifest(t, priv, sourceURL, g, *at("2026-07-01T00:00:00Z")), snapshot: g})
	previous = expired.LastAttempt
	trigger()
	renewedStatus, renewed := waitOnlineAttempt(t, previous, "")
	if renewedStatus.activeID() == manual.release() || renewed.VerifiedDigest == nil || *renewed.VerifiedDigest != digestOf(g) {
		t.Fatalf("renewed authorization did not publish ready release: %+v %+v", renewedStatus.Active, renewed)
	}
	waitManifest(t, renewedStatus.activeID())
}

func settingsEnv(settings map[string]string) []string {
	env := make([]string, 0, len(settings))
	for key, value := range settings {
		env = append(env, key+"="+value)
	}
	return env
}
