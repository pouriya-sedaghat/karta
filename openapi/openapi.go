// Package openapi embeds the machine-readable API contracts.
package openapi

import _ "embed"

// Spec is the public OpenAPI 3.0 document served at /v1/openapi.yaml.
//
//go:embed openapi.yaml
var Spec []byte

// OperatorSpec is the operator API document served by the publisher at
// /v1/operator/openapi.yaml.
//
//go:embed operator.yaml
var OperatorSpec []byte
