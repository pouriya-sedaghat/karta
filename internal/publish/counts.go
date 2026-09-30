package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pouriya-sedaghat/karta/internal/importer"
	"github.com/pouriya-sedaghat/karta/internal/region"
	"github.com/pouriya-sedaghat/karta/internal/registry"
)

// ErrCountsUnavailable means a release's row counts could not be read or
// decoded; the relative row-count gate then refuses instead of passing.
var ErrCountsUnavailable = errors.New("release row counts unavailable")

// reportCountsReader returns the "counts" object of the import report
// stored in a release database (JSON, or nil for SQL NULL).
type reportCountsReader func(ctx context.Context, database string) ([]byte, error)

// releaseCounts returns a release's row counts: from the registry, or, for
// a release built before the registry recorded them (Stage 1), from the
// import report stored in its database. A failure to read them, and counts
// that are missing, empty or undecodable, are an error wrapping
// ErrCountsUnavailable (and the cause), never an empty result.
func releaseCounts(ctx context.Context, r *registry.Release, read reportCountsReader) (map[string]int64, error) {
	if len(r.Counts) > 0 {
		return r.Counts, nil
	}
	b, err := read(ctx, r.Database)
	if err != nil {
		return nil, fmt.Errorf("%w: release %s: reading its import report: %w", ErrCountsUnavailable, r.ID, err)
	}
	var counts map[string]int64
	if len(b) == 0 {
		return nil, fmt.Errorf("%w: release %s: neither the registry nor its import report records row counts", ErrCountsUnavailable, r.ID)
	}
	if err := json.Unmarshal(b, &counts); err != nil {
		return nil, fmt.Errorf("%w: release %s: its import report has undecodable counts: %v", ErrCountsUnavailable, r.ID, err)
	}
	if len(counts) == 0 {
		return nil, fmt.Errorf("%w: release %s: neither the registry nor its import report records row counts", ErrCountsUnavailable, r.ID)
	}
	return counts, nil
}

// readReportCounts reads the counts of the import report stored in a
// release database.
func (s *Service) readReportCounts(ctx context.Context, database string) ([]byte, error) {
	conn, err := s.cfg.Build.DB.Connect(ctx, database)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	var b []byte
	err = conn.QueryRow(ctx, `SELECT report->'counts' FROM karta.release_info`).Scan(&b)
	return b, err
}

// countGate is the relative row-count rule applied at the moment of a
// forward switch, inside the pointer transaction (registry.Activate's Allow),
// so active is the release active then: the target may not keep fewer than
// 1 - validation.max_drop_fraction of any table's rows of that release. A
// target validated against another active release (built before a
// rollback, or kept ready) is judged against the current one. The gate
// applies within one region, with the region configuration cfg (cfgErr if it
// could not be loaded); counts or a configuration that cannot be read refuse
// the switch. A transient failure (database unreachable) is returned as is.
func (s *Service) countGate(ctx context.Context, cfg region.Config, cfgErr error, active, target *registry.Release) error {
	if active == nil || active.RegionID != target.RegionID {
		return nil
	}
	if cfgErr != nil {
		return policyErr(importer.CodeRegionConfig, "cannot check row counts against the active release %s: region configuration: %v", active.ID, cfgErr)
	}
	if cfg.ID != target.RegionID {
		return policyErr(importer.CodeRegionConfig, "cannot check row counts: this publisher is configured for region %q, release %s serves %q",
			cfg.ID, target.ID, target.RegionID)
	}
	f := cfg.Validation.MaxDropFraction
	if f == nil {
		return nil
	}
	counts := make([]map[string]int64, 2)
	for i, r := range []*registry.Release{active, target} {
		c, err := releaseCounts(ctx, r, s.readCounts)
		if err != nil {
			if transient(err) {
				return err
			}
			return policyErr(CodeCountsUnavailable, "cannot check row counts (validation.max_drop_fraction): %v", err)
		}
		counts[i] = c
	}
	var lost []string
	for _, c := range importer.RelativeChecks(f, active.ID, counts[0], counts[1]) {
		if !c.Passed {
			lost = append(lost, strings.TrimPrefix(c.Name, "relative_count_")+": "+c.Detail)
		}
	}
	if len(lost) > 0 {
		return policyErr(CodeExcessiveDataLoss, "release %s would lose too much data relative to the active release %s (validation.max_drop_fraction): %s",
			target.ID, active.ID, strings.Join(lost, "; "))
	}
	return nil
}
