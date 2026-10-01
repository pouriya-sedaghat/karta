package online

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"syscall"
	"time"
)

// nonPublic lists the address ranges the fetcher never connects to unless
// the source file allows them explicitly (allowed_networks): this host,
// private networks (including Docker's), shared, link-local and cloud
// metadata addresses, documentation, benchmarking, multicast and reserved
// ranges, and IPv6 forms that can embed such IPv4 addresses.
var nonPublic = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23",
		"2001:db8::/32", "2002::/16", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// Policy decides which addresses may be connected to.
type Policy struct {
	Allowed []netip.Prefix
}

// Check refuses a non-public address that no allowed prefix contains.
func (p Policy) Check(ip netip.Addr) error {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return errorf(CodeDestinationRefused, "not an IP address")
	}
	for _, a := range p.Allowed {
		if a.Contains(ip) {
			return nil
		}
	}
	for _, n := range nonPublic {
		if n.Contains(ip) {
			return errorf(CodeDestinationRefused, "address %s is not a public address and not in allowed_networks", ip)
		}
	}
	return nil
}

// control runs for every connection the fetcher makes, with the address
// actually being connected to (after name resolution), so a name that
// resolves (or later re-resolves) to a local or private address is refused
// at the moment of connecting.
func (p Policy) control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errorf(CodeDestinationRefused, "address %q", address)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return errorf(CodeDestinationRefused, "address %q is not an IP address", host)
	}
	return p.Check(ip)
}

// ClientOptions configure the HTTP client.
type ClientOptions struct {
	Policy Policy
	// Roots, when not nil, are the only trusted root certificates.
	Roots          *x509.CertPool
	DialTimeout    time.Duration
	HeaderTimeout  time.Duration
	HandshakeLimit time.Duration
}

// errRedirect is returned for any redirect: the source must publish direct
// URLs in its signed manifest.
var errRedirect = errors.New("redirect refused")

// NewHTTPClient returns the fetcher's client: TLS 1.2+ with certificate
// verification (system roots or the source's CA file), the destination
// policy checked on every connection, no proxy from the environment, no
// redirects, no transparent decompression, bounded header sizes and
// timeouts, and at most two connections.
func NewHTTPClient(o ClientOptions) *http.Client {
	d := &net.Dialer{Timeout: o.DialTimeout, KeepAlive: 30 * time.Second, Control: o.Policy.control}
	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            d.DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: o.Roots},
		TLSHandshakeTimeout:    o.HandshakeLimit,
		ResponseHeaderTimeout:  o.HeaderTimeout,
		DisableCompression:     true,
		MaxIdleConns:           2,
		MaxConnsPerHost:        2,
		IdleConnTimeout:        30 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
	}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect }}
}

// LoadRoots reads a PEM bundle of root certificates.
func LoadRoots(path string) (*x509.CertPool, error) {
	b, err := readRegular(path, 1<<20)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("%s holds no PEM certificate", path)
	}
	return pool, nil
}

// classify turns a transport error into a coded error without the URL.
func classify(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var coded *Error
	if errors.As(err, &coded) {
		return coded
	}
	if errors.Is(err, errRedirect) {
		return errorf(CodeRedirectRefused, "the source answered with a redirect")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return errorf(CodeTruncated, "the connection ended before the whole response arrived")
	}
	if errors.Is(err, errStalled) {
		return errorf(CodeStalled, "no data received for the stall timeout")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || os.IsTimeout(err) {
		return errorf(CodeTimeout, "the request did not finish in time")
	}
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var recErr tls.RecordHeaderError
	if errors.As(err, &certErr) || errors.As(err, &unknownAuth) || errors.As(err, &hostErr) || errors.As(err, &recErr) {
		return errorf(CodeTLS, "TLS verification failed: %v", unwrapMsg(err))
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return errorf(CodeNetwork, "name resolution failed for %s", dnsErr.Name)
	}
	return errorf(CodeNetwork, "%v", unwrapMsg(err))
}

// unwrapMsg drops the "Get \"URL\": " prefix of url.Error (URLs are
// reported redacted by the caller).
func unwrapMsg(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	return truncate(err.Error(), 300)
}
