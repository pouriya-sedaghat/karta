package publish

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/importer"
	"github.com/pouriya-sedaghat/karta/internal/region"
	"github.com/pouriya-sedaghat/karta/internal/registry"
	"github.com/pouriya-sedaghat/karta/internal/release"
	"github.com/pouriya-sedaghat/karta/internal/schema"
	"github.com/pouriya-sedaghat/karta/internal/style"
)

// A forward activation requires the target to have passed the content
// policy of the region file read for the switch, and nothing else.
func TestPolicyGate(t *testing.T) {
	cfg, err := region.Load("../../config/regions/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	current := importer.PolicyOf(cfg).SHA256
	empty := importer.PolicyOf(region.Config{ID: cfg.ID}).SHA256
	str := func(s string) *string { return &s }
	release := func(policy *string) *registry.Release {
		return &registry.Release{ID: "r000000000000000000000001", RegionID: cfg.ID, State: registry.StateReady, ValidationPolicySHA256: policy}
	}

	if err := policyGate(cfg, nil, release(str(current))); err != nil {
		t.Fatalf("a release that passed the policy in force: %v", err)
	}
	// The pass is bound to the exact region identity it was evaluated for:
	// an edit of only the name, box or view (checks unchanged) makes it not
	// current, so activation is refused and the status says so.
	for name, edit := range map[string]func(*region.Config){
		"name":        func(c *region.Config) { c.Name += " (renamed)" },
		"box":         func(c *region.Config) { c.BBox[2] += 1e-7 },
		"view centre": func(c *region.Config) { c.View.Center[0] += 1e-7 },
		"view zoom":   func(c *region.Config) { c.View.Zoom-- },
	} {
		edited := cfg
		edit(&edited)
		if !samePolicyChecks(cfg, edited) {
			t.Fatalf("%s: the edit changed the checks", name)
		}
		err := policyGate(edited, nil, release(str(current)))
		if PolicyCode(err) != CodeValidationRequired || !strings.Contains(err.Error(), "publish the snapshot again") {
			t.Errorf("a pass for the old %s: %v", name, err)
		}
		if policyCurrent(*release(str(current)), importer.PolicyOf(edited).SHA256) {
			t.Errorf("a pass for the old %s is reported current", name)
		}
	}
	for name, c := range map[string]struct {
		cfg    region.Config
		cfgErr error
		target *registry.Release
		code   string
		says   string
	}{
		"built before policies were recorded": {cfg, nil, release(nil), CodeValidationRequired, "revalidate"},
		"validated under the empty draft policy, the region file now has checks": {cfg, nil, release(str(empty)), CodeValidationRequired,
			"now requires " + current[:12]},
		"another region's release":  {cfg, nil, &registry.Release{ID: "r000000000000000000000002", RegionID: "elsewhere", ValidationPolicySHA256: str(current)}, importer.CodeRegionConfig, "elsewhere"},
		"an unreadable region file": {region.Config{}, errors.New("no such file"), release(str(current)), importer.CodeRegionConfig, "no such file"},
	} {
		err := policyGate(c.cfg, c.cfgErr, c.target)
		if PolicyCode(err) != c.code || !errors.Is(err, ErrPolicy) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v (code %q), want %s naming %q", name, err, PolicyCode(err), c.code, c.says)
		}
	}
}

func TestPolicyCurrent(t *testing.T) {
	p := "aa"
	if policyCurrent(registry.Release{}, "aa") || policyCurrent(registry.Release{ValidationPolicySHA256: &p}, "") ||
		policyCurrent(registry.Release{ValidationPolicySHA256: &p}, "bb") || !policyCurrent(registry.Release{ValidationPolicySHA256: &p}, "aa") {
		t.Fatal("policyCurrent")
	}
}

// A release is revalidated only against a region file that describes the
// region it was built for (identity and revisions, as its database records
// them), so a policy-only edit is the only difference.
func TestSameBuild(t *testing.T) {
	cfg, err := region.Load("../../config/regions/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	id := cfg.Identity()
	built := release.Info{RegionID: id.ID, RegionName: id.Name, BBox: id.BBox, Center: id.Center, Zoom: id.Zoom,
		SchemaRevision: schema.Revision(), StyleRevision: style.Revision()}
	if err := sameBuild(built, cfg); err != nil {
		t.Fatalf("the release built from this region file: %v", err)
	}
	policyOnly := cfg
	policyOnly.Validation = region.Validation{}
	if err := sameBuild(built, policyOnly); err != nil {
		t.Fatalf("a policy-only edit: %v", err)
	}
	for name, change := range map[string]func(*release.Info){
		"region id":   func(i *release.Info) { i.RegionID = "elsewhere" },
		"region name": func(i *release.Info) { i.RegionName += " (old)" },
		"bbox":        func(i *release.Info) { i.BBox[0] -= 1e-9 },
		"view center": func(i *release.Info) { i.Center[1] += 1e-9 },
		"view zoom":   func(i *release.Info) { i.Zoom++ },
		"schema":      func(i *release.Info) { i.SchemaRevision = "s0-old" },
		"style":       func(i *release.Info) { i.StyleRevision = "st0-old" },
	} {
		info := built
		change(&info)
		if err := sameBuild(info, cfg); err == nil {
			t.Errorf("a release built with another %s was accepted", name)
		}
	}
}

// An evaluation cut short reports the deadline or cancellation, so it is
// not taken for a result.
func TestInterrupted(t *testing.T) {
	cause := errors.New("conn closed")
	if err := interrupted(context.Background(), cause); err != cause {
		t.Fatalf("live context: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done()
	if err := interrupted(ctx, cause); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "conn closed") {
		t.Fatalf("expired context: %v", err)
	}
}

// An evaluation reads the release database on read-only sessions bounded,
// on the server too, by the time left until its deadline.
func TestRevalidationParams(t *testing.T) {
	s := &Service{cfg: Config{}}
	s.cfg.Build.DB.StatementTimeout = time.Hour
	if p := s.revalidationParams(context.Background()); !p.ReadOnly || p.StatementTimeout != time.Hour {
		t.Fatalf("no deadline: %+v", p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if p := s.revalidationParams(ctx); !p.ReadOnly || p.StatementTimeout <= 4*time.Second || p.StatementTimeout > 5*time.Second {
		t.Fatalf("a 5 s deadline: %+v", p)
	}
	expired, cancel2 := context.WithTimeout(context.Background(), 0)
	defer cancel2()
	if p := s.revalidationParams(expired); p.StatementTimeout != time.Millisecond {
		t.Fatalf("an expired deadline: %+v", p)
	}
	if s.cfg.Build.DB.ReadOnly {
		t.Fatal("the build's own parameters were changed")
	}
}

// samePolicyChecks reports whether two configurations have the same checks.
func samePolicyChecks(a, b region.Config) bool {
	x, _ := json.Marshal(a.Validation)
	y, _ := json.Marshal(b.Validation)
	return string(x) == string(y)
}
