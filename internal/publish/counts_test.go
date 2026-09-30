package publish

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pouriya-sedaghat/karta/internal/importer"
	"github.com/pouriya-sedaghat/karta/internal/region"
	"github.com/pouriya-sedaghat/karta/internal/registry"
)

// reader returns a stored-report reader answering b, err, and counts calls.
func reader(b []byte, err error, calls *[]string) reportCountsReader {
	return func(_ context.Context, database string) ([]byte, error) {
		*calls = append(*calls, database)
		return b, err
	}
}

func TestReleaseCounts(t *testing.T) {
	ctx := context.Background()
	v2 := &registry.Release{ID: "rv2", Database: "karta_rv2", Counts: map[string]int64{"roads": 4}}
	var calls []string
	got, err := releaseCounts(ctx, v2, reader(nil, errors.New("must not be read"), &calls))
	if err != nil || got["roads"] != 4 || len(calls) != 0 {
		t.Fatalf("registry counts: %v %v (read %v)", got, err, calls)
	}

	// A Stage 1 release: the registry has no counts; its stored report does.
	stage1 := &registry.Release{ID: "rs1", Database: "karta_rs1"}
	got, err = releaseCounts(ctx, stage1, reader([]byte(`{"roads": 7, "pois": 16}`), nil, &calls))
	if err != nil || got["roads"] != 7 || got["pois"] != 16 || len(calls) != 1 || calls[0] != "karta_rs1" {
		t.Fatalf("Stage 1 fallback: %v %v (read %v)", got, err, calls)
	}

	// Every way the fallback can fail is an error, never empty counts that
	// would skip the relative checks.
	connErr := errors.New(`failed to connect to database "karta_rs1"`)
	for name, tc := range map[string]struct {
		b   []byte
		err error
	}{
		"connection fails":      {nil, connErr},
		"no report row":         {nil, pgx.ErrNoRows},
		"counts are SQL NULL":   {nil, nil},
		"counts are JSON null":  {[]byte(`null`), nil},
		"counts are malformed":  {[]byte(`{"roads": `), nil},
		"counts are not a map":  {[]byte(`"unreadable"`), nil},
		"counts are not counts": {[]byte(`{"roads": "four"}`), nil},
		"counts are empty":      {[]byte(`{}`), nil},
	} {
		got, err := releaseCounts(ctx, stage1, reader(tc.b, tc.err, &calls))
		if !errors.Is(err, ErrCountsUnavailable) || got != nil || !strings.Contains(err.Error(), "rs1") {
			t.Errorf("%s: %v %v", name, got, err)
		}
		if tc.err != nil && !errors.Is(err, tc.err) {
			t.Errorf("%s: the cause is not kept: %v", name, err)
		}
	}
}

func TestCountGate(t *testing.T) {
	ctx := context.Background()
	half := 0.5
	cfg := region.Config{ID: "fixture"}
	cfg.Validation.MaxDropFraction = &half
	counts := func(roads, pois int64) map[string]int64 { return map[string]int64{"roads": roads, "pois": pois} }
	rel := func(id string, c map[string]int64) *registry.Release {
		return &registry.Release{ID: id, Database: "karta_" + id, RegionID: "fixture", Counts: c}
	}
	var calls []string
	s := &Service{readCounts: reader(nil, errors.New("unexpected read"), &calls)}
	candidate := rel("rX", counts(4, 16))

	// Passes against the release it was validated with (16 POIs) ...
	if err := s.countGate(ctx, cfg, nil, rel("rA", counts(4, 16)), candidate); err != nil {
		t.Errorf("same counts: %v", err)
	}
	// ... and is refused against a release with far more data.
	err := s.countGate(ctx, cfg, nil, rel("rH", counts(4, 56)), candidate)
	if PolicyCode(err) != CodeExcessiveDataLoss || !strings.Contains(err.Error(), "pois: 16 rows; active release rH has 56, minimum 28") ||
		strings.Contains(err.Error(), "roads:") {
		t.Errorf("excessive drop: %v", err)
	}

	// Not applicable: no active release, another region (an explicit
	// region change), or the gate is not configured.
	if err := s.countGate(ctx, cfg, nil, nil, candidate); err != nil {
		t.Errorf("no active release: %v", err)
	}
	other := rel("rO", counts(400, 1600))
	other.RegionID = "elsewhere"
	if err := s.countGate(ctx, cfg, nil, other, candidate); err != nil {
		t.Errorf("region change: %v", err)
	}
	if err := s.countGate(ctx, region.Config{ID: "fixture"}, nil, rel("rH", counts(4, 56)), candidate); err != nil {
		t.Errorf("gate off: %v", err)
	}

	// The policy cannot be evaluated: refused.
	if err := s.countGate(ctx, cfg, errors.New("unreadable"), rel("rH", counts(4, 56)), candidate); PolicyCode(err) != importer.CodeRegionConfig {
		t.Errorf("region configuration error: %v", err)
	}
	if err := s.countGate(ctx, region.Config{ID: "tehran-chitgar", Validation: cfg.Validation}, nil, rel("rH", counts(4, 56)), candidate); PolicyCode(err) != importer.CodeRegionConfig {
		t.Errorf("configured for another region: %v", err)
	}

	// A Stage 1 active release is compared through its stored report; if
	// that cannot be read the switch is refused, and a database outage is
	// reported as such (retried), not as a policy decision.
	stage1 := rel("rS1", nil)
	calls = nil
	s.readCounts = reader([]byte(`{"roads": 4, "pois": 100}`), nil, &calls)
	if err := s.countGate(ctx, cfg, nil, stage1, candidate); PolicyCode(err) != CodeExcessiveDataLoss || len(calls) != 1 || calls[0] != "karta_rS1" {
		t.Errorf("Stage 1 active release: %v (read %v)", err, calls)
	}
	s.readCounts = reader([]byte(`"unreadable"`), nil, &calls)
	if err := s.countGate(ctx, cfg, nil, stage1, candidate); PolicyCode(err) != CodeCountsUnavailable {
		t.Errorf("unreadable Stage 1 counts: %v", err)
	}
	s.readCounts = reader(nil, &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command"}, &calls)
	err = s.countGate(ctx, cfg, nil, stage1, candidate)
	if err == nil || PolicyCode(err) != "" || !errors.Is(err, ErrCountsUnavailable) || !transient(err) {
		t.Errorf("database outage: %v", err)
	}
	// The target's counts are needed as much as the active release's.
	s.readCounts = reader(nil, nil, &calls)
	if err := s.countGate(ctx, cfg, nil, rel("rA", counts(4, 16)), rel("rT", nil)); PolicyCode(err) != CodeCountsUnavailable {
		t.Errorf("target without counts: %v", err)
	}
}

func TestCleanupOutcome(t *testing.T) {
	boom := errors.New("lock timeout")
	for _, tc := range []struct {
		dryRun  bool
		removed int
		err     error
		want    string
	}{
		{true, 0, nil, registry.OutcomeNoop},
		{false, 0, nil, registry.OutcomeNoop},
		{false, 2, nil, registry.OutcomeSucceeded},
		// An error is a failure, even after some releases were removed.
		{false, 0, boom, registry.OutcomeFailed},
		{false, 1, boom, registry.OutcomeFailed},
		{true, 0, boom, registry.OutcomeFailed},
	} {
		if got := cleanupOutcome(tc.dryRun, tc.removed, tc.err); got != tc.want {
			t.Errorf("dry run %v, %d removed, error %v: %s, want %s", tc.dryRun, tc.removed, tc.err, got, tc.want)
		}
	}
}
