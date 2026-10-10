package config

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestServeDefaults(t *testing.T) {
	c, err := LoadServe(env(map[string]string{"KARTA_PUBLIC_BASE_URL": "http://localhost:8080/"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicBaseURL != "http://localhost:8080" || c.ListenAddr != ":8080" || c.DB.User != "karta_api" ||
		!c.DB.ReadOnly || c.DB.StatementTimeout != 3*time.Second || c.RequestTimeout != 10*time.Second || len(c.CORSAllowedOrigins) != 0 {
		t.Fatalf("defaults %+v", c)
	}
}

func TestServeErrors(t *testing.T) {
	_, err := LoadServe(env(map[string]string{
		"KARTA_DB_PORT": "99999", "KARTA_DB_USER": "Robert'); DROP", "KARTA_REQUEST_TIMEOUT": "1ms",
		"KARTA_CORS_ALLOWED_ORIGINS": "https://ok.example, not a url", "KARTA_DB_SSLMODE": "maybe",
		"KARTA_LOG_LEVEL": "loud", "KARTA_WEB_DIR": "/does/not/exist",
	}))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"KARTA_PUBLIC_BASE_URL", "KARTA_DB_PORT", "database user", "KARTA_REQUEST_TIMEOUT",
		"KARTA_CORS_ALLOWED_ORIGINS", "sslmode", "KARTA_LOG_LEVEL", "KARTA_WEB_DIR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
	for _, bad := range []string{"localhost:8080", "ftp://x", "http://u:p@x", "http://x/?a=1"} {
		if _, err := LoadServe(env(map[string]string{"KARTA_PUBLIC_BASE_URL": bad})); err == nil {
			t.Errorf("base URL %q accepted", bad)
		}
	}
	if _, err := LoadServe(env(map[string]string{"KARTA_PUBLIC_BASE_URL": "http://x", "KARTA_DB_STATEMENT_TIMEOUT": "20s"})); err == nil {
		t.Error("statement timeout longer than request timeout accepted")
	}
}

func TestImportDefaults(t *testing.T) {
	c, err := LoadImport(env(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if c.DB.User != "karta_importer" || c.TemplateDB != "karta_template" || c.MaxInputBytes != 4096<<20 || c.Slim {
		t.Fatalf("defaults %+v", c)
	}
	if _, err := LoadImport(env(map[string]string{"KARTA_TEMPLATE_DB": "x; DROP DATABASE y"})); err == nil {
		t.Error("unsafe template name accepted")
	}
}

func TestPublishTimeout(t *testing.T) {
	c, err := LoadImport(env(map[string]string{}))
	if err != nil || c.PublishTimeout != 6*time.Hour {
		t.Fatalf("default %s, %v", c.PublishTimeout, err)
	}
	c, err = LoadImport(env(map[string]string{"KARTA_PUBLISH_TIMEOUT": "90m"}))
	if err != nil || c.PublishTimeout != 90*time.Minute {
		t.Fatalf("90m: %s, %v", c.PublishTimeout, err)
	}
	// Always bounded: no zero (unbounded) value, and at most a week.
	for _, bad := range []string{"0", "0s", "500ms", "200h", "soon"} {
		if _, err := LoadImport(env(map[string]string{"KARTA_PUBLISH_TIMEOUT": bad})); err == nil || !strings.Contains(err.Error(), "KARTA_PUBLISH_TIMEOUT") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestOnlineConfig(t *testing.T) {
	dir := t.TempDir()
	region := dir + "/region.json"
	source := dir + "/source.json"
	for _, f := range []string{region, source} {
		if err := os.WriteFile(f, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := map[string]string{"KARTA_REGION_FILE": region, "KARTA_INBOX_DIR": dir}
	c, err := LoadPublisher(env(base))
	if err != nil {
		t.Fatal(err)
	}
	if c.OnlineSourceFile != "" || c.StaleAfter != 0 {
		t.Errorf("online updates or a staleness threshold on by default: %+v", c)
	}
	on := map[string]string{"KARTA_ONLINE_SOURCE_FILE": source, "KARTA_ONLINE_DIR": dir, "KARTA_DATA_STALE_AFTER": "48h"}
	for k, v := range base {
		on[k] = v
	}
	if c, err = LoadPublisher(env(on)); err != nil || c.OnlineSourceFile != source || c.OnlineDir != dir || c.StaleAfter != 48*time.Hour {
		t.Fatalf("online publisher: %v %+v", err, c)
	}
	for name, change := range map[string]map[string]string{
		"source file missing":   {"KARTA_ONLINE_SOURCE_FILE": dir + "/nope.json"},
		"no outbox":             {"KARTA_ONLINE_DIR": ""},
		"outbox not a dir":      {"KARTA_ONLINE_DIR": source},
		"bad stale threshold":   {"KARTA_DATA_STALE_AFTER": "soon"},
		"stale threshold > 1 y": {"KARTA_DATA_STALE_AFTER": "9000h"},
	} {
		m := map[string]string{}
		for k, v := range on {
			m[k] = v
		}
		for k, v := range change {
			m[k] = v
		}
		if _, err := LoadPublisher(env(m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	f, err := LoadFetcher(env(map[string]string{"KARTA_ONLINE_SOURCE_FILE": source, "KARTA_REGION_FILE": region, "KARTA_ONLINE_DIR": dir}))
	if err != nil || f.ReserveBytes != 64<<20 || f.MaxInputBytes != 4096<<20 || f.MaxFutureSkew != 10*time.Minute {
		t.Fatalf("fetcher defaults: %v %+v", err, f)
	}
	_, err = LoadFetcher(env(map[string]string{}))
	for _, want := range []string{"KARTA_ONLINE_SOURCE_FILE", "KARTA_REGION_FILE", "KARTA_ONLINE_DIR"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("fetcher without %s: %v", want, err)
		}
	}
}

func TestMetricsListener(t *testing.T) {
	base := map[string]string{"KARTA_PUBLIC_BASE_URL": "http://x"}
	c, err := LoadServe(env(base))
	if err != nil || c.MetricsListenAddr != "" {
		t.Fatalf("metrics are off by default: %q %v", c.MetricsListenAddr, err)
	}
	tokens := t.TempDir() + "/metrics_tokens"
	if err := os.WriteFile(tokens, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	with := func(kv ...string) map[string]string {
		m := map[string]string{"KARTA_PUBLIC_BASE_URL": "http://x"}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	if c, err := LoadServe(env(with("KARTA_METRICS_LISTEN_ADDR", ":9464", "KARTA_METRICS_TOKENS_FILE", tokens))); err != nil || c.MetricsTokensFile != tokens {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := LoadServe(env(with("KARTA_METRICS_LISTEN_ADDR", ":9464", "KARTA_METRICS_TOKENS_FILE", "/does/not/exist"))); err == nil {
		t.Error("a missing tokens file accepted")
	}
	if _, err := LoadServe(env(with("KARTA_METRICS_LISTEN_ADDR", ":8080", "KARTA_METRICS_TOKENS_FILE", tokens))); err == nil {
		t.Error("metrics on the public listener accepted")
	}
}

// The measured-timeout gate: twice the measurement for the server, 30 s
// more for the client, both within 10 min. The boundary is a measurement of
// exactly 285 s; the runbook states the same numbers.
func TestRevalidationTimeouts(t *testing.T) {
	for _, c := range []struct {
		d               time.Duration
		request, client time.Duration
		ok              bool
	}{
		{0, time.Second, 31 * time.Second, true},
		{400 * time.Millisecond, time.Second, 31 * time.Second, true},
		{12*time.Second + 300*time.Millisecond, 25 * time.Second, 55 * time.Second, true},
		{285 * time.Second, 570 * time.Second, 600 * time.Second, true},
		{285*time.Second + time.Nanosecond, 571 * time.Second, 601 * time.Second, false},
		{285*time.Second + 500*time.Millisecond, 571 * time.Second, 601 * time.Second, false},
		{9*time.Minute + 30*time.Second, 19 * time.Minute, 19*time.Minute + 30*time.Second, false},
	} {
		request, client, ok := RevalidationTimeouts(c.d)
		if request != c.request || client != c.client || ok != c.ok {
			t.Errorf("%v: %v %v %v, want %v %v %v", c.d, request, client, ok, c.request, c.client, c.ok)
		}
	}
	// The largest measurement that fits, to the nanosecond.
	lo, hi := time.Duration(0), MaxOperatorTimeout
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if _, _, ok := RevalidationTimeouts(mid); ok {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	request, client, _ := RevalidationTimeouts(lo)
	if lo != 285*time.Second {
		t.Fatalf("the boundary is %v", lo)
	}
	b, err := os.ReadFile("../../docs/runbook.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	start := strings.Index(doc, "#### Measured-timeout gate")
	end := strings.Index(doc[start+1:], "\n4. **Measure again**")
	if start < 0 || end < 0 {
		t.Fatal("the runbook has no measured-timeout gate")
	}
	gate := strings.Join(strings.Fields(doc[start:start+1+end]), " ")
	for _, want := range []string{
		"at least twice the measurement (rounded up to a whole second)",
		fmt.Sprintf("at least %d s longer than the request deadline", int(OperatorClientMargin.Seconds())),
		fmt.Sprintf("neither may exceed the %d min maximum", int(MaxOperatorTimeout.Minutes())),
		fmt.Sprintf("at most %d s (%d min %d s), which gives %d s and %d s", int(lo.Seconds()), int(lo.Minutes()), int(lo.Seconds())%60,
			int(request.Seconds()), int(client.Seconds())),
		"For a longer measurement, or if the measurement itself timed out",
		"do not use the operator endpoint for that release and policy",
		"resubmit the snapshot with `karta import --no-activate`",
	} {
		if !strings.Contains(gate, want) {
			t.Errorf("the runbook's measured-timeout gate does not say %q", want)
		}
	}
}
