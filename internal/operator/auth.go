// Package operator is Karta's operator interface: an authenticated HTTP API,
// served by the publisher on its own listener (never by the public API
// process), for publication status, digest authorization, activation,
// rollback and cleanup. See openapi/operator.yaml and docs/security.md.
package operator

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// Scopes an operator credential can hold.
const (
	ScopeStatus   = "status"   // read status and audit
	ScopePublish  = "publish"  // authorize or revoke digests, activate a ready release
	ScopeRollback = "rollback" // roll back to a retained release
	ScopeCleanup  = "cleanup"  // remove releases beyond retention
	// ScopeIntakeWatch is the unattended local intake watcher: it may only
	// create, read and close its own bounded digest authorizations
	// (/v1/operator/intake/*); a rollback pauses what they admit.
	ScopeIntakeWatch = "intake_watch"
	// ScopeIntakeSubmit is a named person's intake command: the same narrow
	// capability, for deliberate deliveries.
	ScopeIntakeSubmit = "intake_submit"
)

var knownScopes = map[string]bool{ScopeStatus: true, ScopePublish: true, ScopeRollback: true, ScopeCleanup: true,
	ScopeIntakeWatch: true, ScopeIntakeSubmit: true}

// intakeScopes must each be a credential's only scope, so an intake
// credential is narrow by construction.
var intakeScopes = map[string]bool{ScopeIntakeWatch: true, ScopeIntakeSubmit: true}

// IntakeChannel returns the intake scope of a credential ("" if it has none).
func (c Credential) IntakeChannel() string {
	for s := range c.Scopes {
		if intakeScopes[s] {
			return s
		}
	}
	return ""
}

// Credential is one named operator credential. Only the SHA-256 of the
// token is held; tokens are 256-bit random values, so an unsalted hash is
// not open to guessing.
type Credential struct {
	Name   string
	Scopes map[string]bool
	hash   [32]byte
}

// ScopeList returns the credential's scopes, sorted.
func (c Credential) ScopeList() []string {
	var out []string
	for s := range c.Scopes {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

var (
	namePattern  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	hashPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	tokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// maxTokensFile bounds the credentials file.
const maxTokensFile = 64 << 10

// LoadCredentials reads the credentials file: one credential per line,
// "NAME SCOPE[,SCOPE...] SHA256-OF-TOKEN"; blank lines and lines starting
// with # are ignored. scripts/gen-secrets.sh writes it.
func LoadCredentials(path string) ([]Credential, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- configured secret path
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxTokensFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxTokensFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxTokensFile)
	}
	return ParseCredentials(b)
}

// ParseCredentials parses credentials file content.
func ParseCredentials(b []byte) ([]Credential, error) {
	var out []Credential
	seen := map[string]bool{}
	hashes := map[[32]byte]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("line %d: want NAME SCOPES SHA256", n)
		}
		if !namePattern.MatchString(f[0]) {
			return nil, fmt.Errorf("line %d: name %q must match %s", n, f[0], namePattern)
		}
		if seen[f[0]] {
			return nil, fmt.Errorf("line %d: name %q repeated", n, f[0])
		}
		seen[f[0]] = true
		c := Credential{Name: f[0], Scopes: map[string]bool{}}
		for _, s := range strings.Split(f[1], ",") {
			if !knownScopes[s] {
				return nil, fmt.Errorf("line %d: unknown scope %q", n, s)
			}
			c.Scopes[s] = true
		}
		if ch := c.IntakeChannel(); ch != "" && len(c.Scopes) > 1 {
			return nil, fmt.Errorf("line %d: scope %s must be the credential's only scope", n, ch)
		}
		if !hashPattern.MatchString(f[2]) {
			return nil, fmt.Errorf("line %d: the token hash must be 64 lowercase hex digits", n)
		}
		if _, err := hex.Decode(c.hash[:], []byte(f[2])); err != nil {
			return nil, fmt.Errorf("line %d: %v", n, err)
		}
		if hashes[c.hash] {
			return nil, fmt.Errorf("line %d: token hash repeated", n)
		}
		hashes[c.hash] = true
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("no operator credentials configured")
	}
	return out, nil
}

// Authentication errors.
var (
	ErrNoCredentials  = errors.New("missing bearer token")
	ErrBadCredentials = errors.New("invalid bearer token")
)

// Authenticate resolves an Authorization header value to a credential. The
// token is compared by hash against every credential in constant time, so
// timing reveals neither the match nor its position.
func Authenticate(creds []Credential, header string) (*Credential, error) {
	if header == "" {
		return nil, ErrNoCredentials
	}
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || !tokenPattern.MatchString(token) {
		return nil, ErrBadCredentials
	}
	sum := sha256.Sum256([]byte(token))
	var found *Credential
	for i := range creds {
		if subtle.ConstantTimeCompare(sum[:], creds[i].hash[:]) == 1 {
			found = &creds[i]
		}
	}
	if found == nil {
		return nil, ErrBadCredentials
	}
	return found, nil
}

// HashToken returns the credentials-file hash of a token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ReadToken reads a client token file (64 hex digits and an optional newline).
func ReadToken(path string) (string, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- operator-supplied token file
	if err != nil {
		return "", err
	}
	t := strings.TrimRight(string(b), "\r\n")
	if !tokenPattern.MatchString(t) {
		return "", fmt.Errorf("%s does not hold an operator token (64 lowercase hex digits)", path)
	}
	return t, nil
}
