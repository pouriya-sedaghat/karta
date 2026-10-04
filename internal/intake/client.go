package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/safefile"
)

// Client calls the operator API's intake endpoints with an intake
// credential (scope intake_watch or intake_submit). It can do nothing else:
// every other route refuses that credential.
type Client struct {
	base  string
	token string
	http  *http.Client
}

var tokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// NewClient returns a client for the operator API at base (http(s), no
// credentials or query; use loopback or the operator network) with the token
// in tokenFile.
func NewClient(base, tokenFile string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return nil, fmt.Errorf("operator URL %q must be http(s)://host[:port] without credentials or query", base)
	}
	b, err := safefile.ReadRegular(tokenFile, 4096)
	if err != nil {
		return nil, fmt.Errorf("intake token: %w", err)
	}
	tok := strings.TrimRight(string(b), "\r\n")
	if !tokenPattern.MatchString(tok) {
		return nil, fmt.Errorf("%s does not hold an operator token (64 lowercase hex digits)", tokenFile)
	}
	return &Client{base: strings.TrimRight(base, "/"), token: tok, http: &http.Client{Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// APIError is a refusal by the operator API.
type APIError struct {
	Status     int
	Code       string
	ReasonCode string
	Message    string
}

func (e *APIError) Error() string {
	if e.ReasonCode != "" {
		return fmt.Sprintf("operator API %d %s/%s: %s", e.Status, e.Code, e.ReasonCode, e.Message)
	}
	return fmt.Sprintf("operator API %d %s: %s", e.Status, e.Code, e.Message)
}

// Authorization is the intake's view of one of its authorizations.
type Authorization struct {
	ID         int64      `json:"id"`
	RegionID   string     `json:"region_id"`
	SHA256     string     `json:"sha256"`
	SizeBytes  *int64     `json:"size_bytes"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	Channel    string     `json:"channel"`
	IntakeName *string    `json:"intake_name"`
}

// Open reports an authorization that is neither closed nor expired.
func (a Authorization) Open(now time.Time) bool {
	return a.RevokedAt == nil && (a.ExpiresAt == nil || a.ExpiresAt.After(now))
}

// Submission is the publisher's record of a handed-off delivery.
type Submission struct {
	ID         int64      `json:"id"`
	Source     string     `json:"source"`
	Name       string     `json:"name"`
	State      string     `json:"state"`
	ReasonCode *string    `json:"reason_code"`
	Reason     *string    `json:"reason"`
	ReleaseID  *string    `json:"release_id"`
	FinishedAt *time.Time `json:"finished_at"`
}

// Final reports a submission whose outcome will not change by itself.
func (s *Submission) Final() bool {
	if s == nil {
		return false
	}
	switch s.State {
	case "published", "ready", "duplicate", "rejected", "failed":
		return true
	}
	return false
}

// Record is an authorization with its submission (nil until the publisher
// has one).
type Record struct {
	Authorization Authorization `json:"authorization"`
	Submission    *Submission   `json:"submission"`
}

// Limits are the publisher's bounds for intake authorizations.
type Limits struct {
	RegionID         string  `json:"region_id"`
	MaxTTLSeconds    float64 `json:"max_ttl_seconds"`
	MaxOpen          int     `json:"max_open"`
	MaxSnapshotBytes int64   `json:"max_snapshot_bytes"`
}

// List is the caller's own records and the limits.
type List struct {
	Limits  Limits   `json:"limits"`
	Records []Record `json:"records"`
}

// AuthorizeRequest asks for one authorization.
type AuthorizeRequest struct {
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"size_bytes"`
	RegionID   string `json:"region_id"`
	TTLSeconds int64  `json:"ttl_seconds"`
	Name       string `json:"name"`
	Reason     string `json:"reason"`
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Code       string `json:"code"`
				ReasonCode string `json:"reason_code"`
				Message    string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		return &APIError{Status: resp.StatusCode, Code: e.Error.Code, ReasonCode: e.Error.ReasonCode, Message: e.Error.Message}
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("operator API %s %s: %w", method, path, err)
		}
	}
	return nil
}

// Authorize creates (or returns the caller's equal open) authorization.
func (c *Client) Authorize(ctx context.Context, r AuthorizeRequest) (Authorization, bool, error) {
	var out struct {
		Authorization Authorization `json:"authorization"`
		Created       bool          `json:"created"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/operator/intake/authorizations", r, &out)
	return out.Authorization, out.Created, err
}

// List returns the caller's own records.
func (c *Client) List(ctx context.Context) (List, error) {
	var out List
	err := c.do(ctx, http.MethodGet, "/v1/operator/intake/authorizations", nil, &out)
	return out, err
}

// Close closes one of the caller's authorizations.
func (c *Client) Close(ctx context.Context, id int64, reason string) (bool, error) {
	var out struct {
		Closed bool `json:"closed"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/operator/intake/authorizations/"+strconv.FormatInt(id, 10)+"/close",
		map[string]string{"reason": reason}, &out)
	return out.Closed, err
}
