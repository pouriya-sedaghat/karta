package config

import (
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
