// Package config reads process configuration from environment variables.
// Every variable has a documented default (see .env.example); invalid values
// stop the process at startup with a message naming the variable.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/dbconn"
)

// Serve configures `karta serve`.
type Serve struct {
	ListenAddr          string
	PublicBaseURL       string
	DB                  dbconn.Params
	RegistryDB          string
	RequestTimeout      time.Duration
	ReleasePollInterval time.Duration
	CORSAllowedOrigins  []string
	WebDir              string
	LogLevel            string
}

// Import configures `karta import`.
type Import struct {
	DB            dbconn.Params
	RegistryDB    string
	TemplateDB    string
	MaxInputBytes int64
	Osm2pgsql     string
	CacheMB       int
	Processes     int
	Slim          bool
	LockTimeout   time.Duration
	LogLevel      string
}

type reader struct {
	getenv func(string) string
	errs   []error
}

func (r *reader) str(key, def string) string {
	if v := strings.TrimSpace(r.getenv(key)); v != "" {
		return v
	}
	return def
}

func (r *reader) int(key string, def, lo, hi int) int {
	s := r.str(key, "")
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < lo || v > hi {
		r.errs = append(r.errs, fmt.Errorf("%s=%q: want an integer %d..%d", key, s, lo, hi))
		return def
	}
	return v
}

func (r *reader) dur(key string, def, lo, hi time.Duration) time.Duration {
	s := r.str(key, "")
	if s == "" {
		return def
	}
	v, err := time.ParseDuration(s)
	if err != nil || v < lo || v > hi {
		r.errs = append(r.errs, fmt.Errorf("%s=%q: want a duration %s..%s", key, s, lo, hi))
		return def
	}
	return v
}

func (r *reader) boolean(key string, def bool) bool {
	s := r.str(key, "")
	if s == "" {
		return def
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s=%q: want true or false", key, s))
		return def
	}
	return v
}

func (r *reader) ident(key, def string) string {
	v := r.str(key, def)
	if !dbconn.ValidIdent(v) {
		r.errs = append(r.errs, fmt.Errorf("%s=%q: want a lowercase PostgreSQL identifier", key, v))
	}
	return v
}

func (r *reader) logLevel() string {
	v := r.str("KARTA_LOG_LEVEL", "info")
	switch v {
	case "debug", "info", "warn", "error":
	default:
		r.errs = append(r.errs, fmt.Errorf("KARTA_LOG_LEVEL=%q: want debug, info, warn or error", v))
	}
	return v
}

func (r *reader) db(user, passwordFile, app string) dbconn.Params {
	p := dbconn.Params{
		Host:         r.str("KARTA_DB_HOST", "db"),
		Port:         r.int("KARTA_DB_PORT", 5432, 1, 65535),
		User:         r.ident("KARTA_DB_USER", user),
		PasswordFile: r.str("KARTA_DB_PASSWORD_FILE", passwordFile),
		SSLMode:      r.str("KARTA_DB_SSLMODE", "disable"),
		AppName:      app,
	}
	if err := p.Validate(); err != nil {
		r.errs = append(r.errs, err)
	}
	return p
}

// LoadServe reads the serving configuration.
func LoadServe(getenv func(string) string) (Serve, error) {
	r := &reader{getenv: getenv}
	c := Serve{
		ListenAddr:          r.str("KARTA_LISTEN_ADDR", ":8080"),
		PublicBaseURL:       strings.TrimRight(r.str("KARTA_PUBLIC_BASE_URL", ""), "/"),
		DB:                  r.db("karta_api", "/run/secrets/db_api_password", "karta-api"),
		RegistryDB:          r.ident("KARTA_REGISTRY_DB", "karta_registry"),
		RequestTimeout:      r.dur("KARTA_REQUEST_TIMEOUT", 10*time.Second, time.Second, 2*time.Minute),
		ReleasePollInterval: r.dur("KARTA_RELEASE_POLL_INTERVAL", 5*time.Second, time.Second, 10*time.Minute),
		WebDir:              r.str("KARTA_WEB_DIR", ""),
		LogLevel:            r.logLevel(),
	}
	c.DB.StatementTimeout = r.dur("KARTA_DB_STATEMENT_TIMEOUT", 3*time.Second, 100*time.Millisecond, time.Minute)
	c.DB.MaxConns = int32(r.int("KARTA_DB_MAX_CONNS", 8, 1, 200)) // #nosec G115 -- bounded to 1..200
	c.DB.ReadOnly = true
	if c.DB.StatementTimeout >= c.RequestTimeout {
		r.errs = append(r.errs, errors.New("KARTA_DB_STATEMENT_TIMEOUT must be shorter than KARTA_REQUEST_TIMEOUT"))
	}
	if err := validBaseURL(c.PublicBaseURL); err != nil {
		r.errs = append(r.errs, fmt.Errorf("KARTA_PUBLIC_BASE_URL: %w", err))
	}
	for _, o := range strings.Split(r.str("KARTA_CORS_ALLOWED_ORIGINS", ""), ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if o != "*" {
			if u, err := url.Parse(o); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" {
				r.errs = append(r.errs, fmt.Errorf("KARTA_CORS_ALLOWED_ORIGINS: %q is not an origin like https://app.example", o))
				continue
			}
		}
		c.CORSAllowedOrigins = append(c.CORSAllowedOrigins, o)
	}
	if c.WebDir != "" {
		if st, err := os.Stat(c.WebDir); err != nil || !st.IsDir() {
			r.errs = append(r.errs, fmt.Errorf("KARTA_WEB_DIR=%q is not a directory", c.WebDir))
		}
	}
	return c, errors.Join(r.errs...)
}

// LoadImport reads the importer configuration.
func LoadImport(getenv func(string) string) (Import, error) {
	r := &reader{getenv: getenv}
	c := Import{
		DB:          r.db("karta_importer", "/run/secrets/db_importer_password", "karta-importer"),
		RegistryDB:  r.ident("KARTA_REGISTRY_DB", "karta_registry"),
		TemplateDB:  r.ident("KARTA_TEMPLATE_DB", "karta_template"),
		Osm2pgsql:   r.str("KARTA_OSM2PGSQL", "osm2pgsql"),
		CacheMB:     r.int("KARTA_OSM2PGSQL_CACHE_MB", 800, 64, 1<<20),
		Processes:   r.int("KARTA_OSM2PGSQL_PROCESSES", 2, 1, 64),
		Slim:        r.boolean("KARTA_OSM2PGSQL_SLIM", false),
		LockTimeout: r.dur("KARTA_IMPORT_LOCK_TIMEOUT", 30*time.Second, 0, time.Hour),
		LogLevel:    r.logLevel(),
	}
	c.MaxInputBytes = int64(r.int("KARTA_MAX_INPUT_MB", 4096, 1, 1<<22)) << 20
	return c, errors.Join(r.errs...)
}

func validBaseURL(s string) error {
	if s == "" {
		return errors.New("required (the absolute URL clients use to reach this service, e.g. http://localhost:8080)")
	}
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return errors.New("want http(s)://host[:port][/path] without query, fragment or credentials")
	}
	return nil
}
