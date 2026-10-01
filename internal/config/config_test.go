package config

import (
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
