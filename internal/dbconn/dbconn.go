// Package dbconn builds PostgreSQL connection configurations from explicit
// parameters. Passwords are read from files (Docker/Compose secrets) and are
// never part of a DSN string, log line or process argument.
package dbconn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var identPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Params identify a server and role.
type Params struct {
	Host         string
	Port         int
	User         string
	PasswordFile string
	// SSLMode is a libpq sslmode (disable, require, verify-full, ...).
	SSLMode string
	AppName string
	// StatementTimeout bounds every statement on the connection (0 = none).
	StatementTimeout time.Duration
	// ReadOnly makes every transaction read-only.
	ReadOnly bool
	// MaxConns bounds a pool (ignored for single connections).
	MaxConns int32
}

// ValidIdent reports whether s is a safe, unquoted PostgreSQL identifier.
func ValidIdent(s string) bool { return identPattern.MatchString(s) }

// Validate checks the parameters without reading the password.
func (p Params) Validate() error {
	var errs []error
	if p.Host == "" {
		errs = append(errs, errors.New("database host is required"))
	}
	if p.Port <= 0 || p.Port > 65535 {
		errs = append(errs, fmt.Errorf("database port %d out of range", p.Port))
	}
	if !ValidIdent(p.User) {
		errs = append(errs, fmt.Errorf("database user %q is not a plain identifier", p.User))
	}
	if p.PasswordFile == "" {
		errs = append(errs, errors.New("database password file is required"))
	}
	switch p.SSLMode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
	default:
		errs = append(errs, fmt.Errorf("unsupported sslmode %q", p.SSLMode))
	}
	return errors.Join(errs...)
}

// Password reads the password file, trimming one trailing newline.
func (p Params) Password() (string, error) {
	b, err := os.ReadFile(p.PasswordFile)
	if err != nil {
		return "", fmt.Errorf("read database password file: %w", err)
	}
	pw := strings.TrimRight(string(b), "\r\n")
	if pw == "" {
		return "", fmt.Errorf("database password file %s is empty", p.PasswordFile)
	}
	return pw, nil
}

func (p Params) connString(database string) string {
	q := func(s string) string { return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'" }
	return fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=%s connect_timeout=10",
		q(p.Host), p.Port, q(p.User), q(database), q(p.SSLMode))
}

func (p Params) runtimeParams() map[string]string {
	rp := map[string]string{}
	if p.AppName != "" {
		rp["application_name"] = p.AppName
	}
	if p.StatementTimeout > 0 {
		rp["statement_timeout"] = strconv.FormatInt(p.StatementTimeout.Milliseconds(), 10)
	}
	if p.ReadOnly {
		rp["default_transaction_read_only"] = "on"
	}
	return rp
}

// ConnConfig returns a single-connection configuration for a database.
func (p Params) ConnConfig(database string) (*pgx.ConnConfig, error) {
	if !ValidIdent(database) {
		return nil, fmt.Errorf("database name %q is not a plain identifier", database)
	}
	pw, err := p.Password()
	if err != nil {
		return nil, err
	}
	cfg, err := pgx.ParseConfig(p.connString(database))
	if err != nil {
		return nil, err
	}
	cfg.Password = pw
	for k, v := range p.runtimeParams() {
		cfg.RuntimeParams[k] = v
	}
	return cfg, nil
}

// Connect opens a single connection.
func (p Params) Connect(ctx context.Context, database string) (*pgx.Conn, error) {
	cfg, err := p.ConnConfig(database)
	if err != nil {
		return nil, err
	}
	return pgx.ConnectConfig(ctx, cfg)
}

// Pool opens a connection pool; connections are established lazily.
func (p Params) Pool(ctx context.Context, database string) (*pgxpool.Pool, error) {
	if !ValidIdent(database) {
		return nil, fmt.Errorf("database name %q is not a plain identifier", database)
	}
	pw, err := p.Password()
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(p.connString(database))
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Password = pw
	for k, v := range p.runtimeParams() {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	if p.MaxConns > 0 {
		cfg.MaxConns = p.MaxConns
	}
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	return pgxpool.NewWithConfig(ctx, cfg)
}
