//go:build onlinefixture

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// This file is compiled only into the disposable CI importer image. The
// production binary cannot dial a private fixture address through this seam.
func onlineFixtureClient(manifestURL string) (*http.Client, error) {
	addr, certFile := os.Getenv("KARTA_TEST_ONLINE_DIAL_ADDR"), os.Getenv("KARTA_TEST_ONLINE_CERT_FILE")
	if addr == "" && certFile == "" {
		return nil, nil
	}
	u, err := url.Parse(manifestURL)
	if err != nil || u.Scheme != "https" || u.Hostname() != "example.com" || addr == "" || certFile == "" {
		return nil, errors.New("fixture client requires the test certificate's HTTPS name, dial address and certificate")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != "host.docker.internal" || port != u.Port() {
		return nil, errors.New("fixture dial address does not match source port")
	}
	pem, err := os.ReadFile(certFile) // #nosec G304 -- disposable CI certificate path
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("invalid fixture certificate")
	}
	transport := &http.Transport{Proxy: nil, DisableCompression: true, MaxIdleConnsPerHost: 1,
		TLSClientConfig:     &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second}
	transport.DialContext = func(ctx context.Context, network, destination string) (net.Conn, error) {
		if !strings.EqualFold(destination, u.Host) {
			return nil, errors.New("unexpected fixture destination")
		}
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}, nil
}
