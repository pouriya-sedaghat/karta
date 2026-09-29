package openapi

import (
	"context"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestSpecIsValid(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/v1/manifest", "/v1/search", "/v1/releases/{release_id}/style.json", "/v1/releases/{release_id}/styles/{style_id}.json",
		"/v1/releases/{release_id}/tiles/{z}/{x}/{y}.pbf", "/health/live", "/health/ready"} {
		if doc.Paths.Find(p) == nil {
			t.Errorf("missing path %s", p)
		}
	}
}

func TestOperatorSpecIsValid(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(OperatorSpec)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/v1/operator/status", "/v1/operator/audit", "/v1/operator/authorizations",
		"/v1/operator/authorizations/{sha256}/revoke", "/v1/operator/releases/{release_id}/activate",
		"/v1/operator/rollback", "/v1/operator/cleanup"} {
		if doc.Paths.Find(p) == nil {
			t.Errorf("missing path %s", p)
		}
	}
}
