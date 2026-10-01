//go:build !onlinefixture

package main

import "net/http"

// Production always uses online's restrictive TLS client and DNS policy.
func onlineFixtureClient(string) (*http.Client, error) { return nil, nil }
