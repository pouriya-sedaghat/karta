// Package openapi embeds the machine-readable API contract.
package openapi

import _ "embed"

// Spec is the OpenAPI 3.0 document served at /v1/openapi.yaml.
//
//go:embed openapi.yaml
var Spec []byte
