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
	"github.com/pouriya-sedaghat/karta/internal/online"
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

// Import configures `karta import` (and is the build part of Publisher).
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
	// Tablespace holds new release databases ("" = the database default).
	Tablespace string
	// StagingDir receives the private copy of every snapshot imported.
	StagingDir string
	Publication
}

// Publication is the release lifecycle policy shared by the publisher and
// the command-line import.
type Publication struct {
	PinGrace            time.Duration
	RetainReleases      int
	CleanupMargin       time.Duration
	CleanupInterval     time.Duration
	MaxAttempts         int
	PointerLockTimeout  time.Duration
	MaxFutureSkew       time.Duration
	StagingReserveBytes int64
	StorageBudgetBytes  int64
	DBVolumePath        string
	MinFreeBytes        int64
	CandidateSizeFactor float64
}

// Publisher configures `karta publisher`: the inbox watcher and the
// operator API.
type Publisher struct {
	Import
	RegionFile             string
	InboxDir               string
	InboxPoll              time.Duration
	InboxSettle            time.Duration
	InboxMaxEntries        int
	AutoActivate           bool
	OperatorListenAddr     string
	OperatorTokensFile     string
	OperatorRequestTimeout time.Duration
	OnlineManifestURL      string
	OnlinePublicKeyFile    string
	OnlineInterval         time.Duration
	OnlineTimeout          time.Duration
	OnlineMaxBytes         int64
	OnlineStaleAfter       time.Duration
}

// OperatorClient configures `karta operator`.
type OperatorClient struct {
	URL       string
	TokenFile string
	Timeout   time.Duration
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
	c := loadImport(r)
	return c, errors.Join(r.errs...)
}

func loadImport(r *reader) Import {
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
	if ts := r.str("KARTA_RELEASE_TABLESPACE", ""); ts != "" {
		c.Tablespace = r.ident("KARTA_RELEASE_TABLESPACE", "")
	}
	c.StagingDir = r.dir("KARTA_STAGING_DIR", os.TempDir())
	c.Publication = Publication{
		PinGrace:            r.dur("KARTA_RELEASE_PIN_GRACE", 24*time.Hour, 0, 30*24*time.Hour),
		RetainReleases:      r.int("KARTA_RETAIN_RELEASES", 2, 0, 50),
		CleanupMargin:       r.dur("KARTA_CLEANUP_MARGIN", 5*time.Minute, 30*time.Second, 24*time.Hour),
		CleanupInterval:     r.dur("KARTA_CLEANUP_INTERVAL", 15*time.Minute, 10*time.Second, 24*time.Hour),
		MaxAttempts:         r.int("KARTA_PUBLISH_MAX_ATTEMPTS", 3, 1, 20),
		PointerLockTimeout:  r.dur("KARTA_POINTER_LOCK_TIMEOUT", 5*time.Second, 100*time.Millisecond, time.Minute),
		MaxFutureSkew:       r.dur("KARTA_MAX_FUTURE_SKEW", 10*time.Minute, 0, 24*time.Hour),
		StagingReserveBytes: int64(r.int("KARTA_STAGING_RESERVE_MB", 64, 0, 1<<22)) << 20,
		StorageBudgetBytes:  int64(r.int("KARTA_RELEASE_STORAGE_BUDGET_MB", 10240, 0, 1<<30)) << 20,
		DBVolumePath:        r.str("KARTA_DB_VOLUME_PATH", ""),
		MinFreeBytes:        int64(r.int("KARTA_MIN_FREE_MB", 512, 0, 1<<22)) << 20,
		CandidateSizeFactor: float64(r.int("KARTA_CANDIDATE_SIZE_FACTOR", 40, 1, 1000)),
	}
	if p := c.DBVolumePath; p != "" {
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			r.errs = append(r.errs, fmt.Errorf("KARTA_DB_VOLUME_PATH=%q is not a directory", p))
		}
	}
	return c
}

// LoadPublisher reads the publisher configuration.
func LoadPublisher(getenv func(string) string) (Publisher, error) {
	r := &reader{getenv: getenv}
	c := Publisher{
		Import:                 loadImport(r),
		RegionFile:             r.str("KARTA_REGION_FILE", ""),
		InboxDir:               r.dir("KARTA_INBOX_DIR", ""),
		InboxPoll:              r.dur("KARTA_INBOX_POLL_INTERVAL", 10*time.Second, time.Second, 10*time.Minute),
		InboxSettle:            r.dur("KARTA_INBOX_SETTLE", 5*time.Second, 0, 10*time.Minute),
		InboxMaxEntries:        r.int("KARTA_INBOX_MAX_ENTRIES", 1000, 10, 100000),
		AutoActivate:           r.boolean("KARTA_PUBLISH_AUTO_ACTIVATE", true),
		OperatorListenAddr:     r.str("KARTA_OPERATOR_LISTEN_ADDR", ":8081"),
		OperatorTokensFile:     r.str("KARTA_OPERATOR_TOKENS_FILE", "/run/secrets/operator_tokens"),
		OperatorRequestTimeout: r.dur("KARTA_OPERATOR_REQUEST_TIMEOUT", time.Minute, time.Second, 10*time.Minute),
		OnlineManifestURL:      r.str("KARTA_ONLINE_MANIFEST_URL", ""),
		OnlinePublicKeyFile:    r.str("KARTA_ONLINE_PUBLIC_KEY_FILE", ""),
		OnlineInterval:         r.dur("KARTA_ONLINE_INTERVAL", time.Hour, time.Minute, 7*24*time.Hour),
		OnlineTimeout:          r.dur("KARTA_ONLINE_TIMEOUT", 10*time.Minute, time.Second, 30*time.Minute),
		OnlineStaleAfter:       r.dur("KARTA_ONLINE_STALE_AFTER", 0, 0, 30*24*time.Hour),
	}
	c.OnlineMaxBytes = int64(r.int("KARTA_ONLINE_MAX_MB", 1024, 1, 1<<20)) << 20
	if c.OnlineManifestURL != "" {
		if c.OnlineMaxBytes > c.MaxInputBytes {
			r.errs = append(r.errs, errors.New("KARTA_ONLINE_MAX_MB must not exceed KARTA_MAX_INPUT_MB"))
		}
		if _, err := online.ValidateURL(c.OnlineManifestURL); err != nil {
			r.errs = append(r.errs, fmt.Errorf("KARTA_ONLINE_MANIFEST_URL: %w", err))
		}
		if c.OnlinePublicKeyFile == "" {
			r.errs = append(r.errs, errors.New("KARTA_ONLINE_PUBLIC_KEY_FILE is required for online updates"))
		}
		if st, err := os.Stat(c.OnlinePublicKeyFile); err != nil || !st.Mode().IsRegular() {
			r.errs = append(r.errs, errors.New("KARTA_ONLINE_PUBLIC_KEY_FILE must be a regular file"))
		}
	} else if c.OnlinePublicKeyFile != "" {
		r.errs = append(r.errs, errors.New("KARTA_ONLINE_PUBLIC_KEY_FILE requires KARTA_ONLINE_MANIFEST_URL"))
	}
	if c.RegionFile == "" {
		r.errs = append(r.errs, errors.New("KARTA_REGION_FILE is required (the region configuration this deployment publishes)"))
	} else if st, err := os.Stat(c.RegionFile); err != nil || !st.Mode().IsRegular() {
		r.errs = append(r.errs, fmt.Errorf("KARTA_REGION_FILE=%q is not a readable file", c.RegionFile))
	}
	if c.InboxDir == "" {
		r.errs = append(r.errs, errors.New("KARTA_INBOX_DIR is required"))
	}
	return c, errors.Join(r.errs...)
}

// LoadOperatorClient reads the operator client configuration.
func LoadOperatorClient(getenv func(string) string) (OperatorClient, error) {
	r := &reader{getenv: getenv}
	c := OperatorClient{
		URL:       r.str("KARTA_OPERATOR_URL", "http://127.0.0.1:8081"),
		TokenFile: r.str("KARTA_OPERATOR_TOKEN_FILE", ""),
		Timeout:   r.dur("KARTA_OPERATOR_CLIENT_TIMEOUT", 2*time.Minute, time.Second, 10*time.Minute),
	}
	return c, errors.Join(r.errs...)
}

// dir reads a directory path that must exist ("" allowed when def is "").
func (r *reader) dir(key, def string) string {
	v := r.str(key, def)
	if v == "" {
		return v
	}
	if st, err := os.Stat(v); err != nil || !st.IsDir() {
		r.errs = append(r.errs, fmt.Errorf("%s=%q is not a directory", key, v))
	}
	return v
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
