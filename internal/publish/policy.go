package publish

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/registry"
)

// ErrPolicy is a publication refused by the forward-only rule, a region
// change or a duplicate; the active release is unchanged.
var ErrPolicy = errors.New("publication refused by policy")

// Policy reason codes.
const (
	CodeOlderThanActive   = "older_than_active"
	CodeNotNewer          = "not_newer"
	CodeRegionChanged     = "region_changed"
	CodeDuplicateActive   = "duplicate_active"
	CodeDuplicateRetained = "duplicate_retained"
	CodeBeingRemoved      = "release_being_removed"
	CodeActiveChanged     = "active_changed"
	CodeManualActivation  = "manual_activation"
	CodeInsufficient      = "insufficient_storage"
	CodeInterrupted       = "interrupted"
	CodeTooManyAttempts   = "too_many_attempts"
	CodeBuildFailed       = "build_failed"
	CodeValidationFailed  = "validation_failed"
	// CodeCountsUnavailable: the relative row-count gate is configured but
	// the row counts of a release it compares could not be read.
	CodeCountsUnavailable = "counts_unavailable"
	// CodeExcessiveDataLoss: a forward switch would drop more rows than
	// validation.max_drop_fraction allows relative to the active release.
	CodeExcessiveDataLoss = "excessive_data_loss"
)

// PolicyError is a refusal with a stable code.
type PolicyError struct {
	Code string
	Msg  string
}

func (e *PolicyError) Error() string { return e.Msg }
func (e *PolicyError) Unwrap() error { return ErrPolicy }

func policyErr(code, format string, args ...any) error {
	return &PolicyError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// PolicyCode returns the code of a policy error, or "".
func PolicyCode(err error) string {
	var pe *PolicyError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// Candidate is what the forward rule needs to know about a snapshot.
type Candidate struct {
	RegionID      string
	SHA256        string
	DataTimestamp time.Time
}

// Forward is the publication rule: a snapshot may replace the active
// release only if it belongs to the same region (unless a region change is
// explicitly allowed) and is strictly newer by data timestamp, or is the
// same snapshot (same SHA-256) rebuilt, for example after a toolchain or
// configuration change. An older snapshot, or a different one with the same
// timestamp, is refused; going back is a rollback, an explicit operator
// action. File times play no part.
func Forward(active *registry.Release, c Candidate, allowRegionChange bool) error {
	if active == nil {
		return nil
	}
	if active.RegionID != c.RegionID && !allowRegionChange {
		return policyErr(CodeRegionChanged, "the active release %s serves region %q, not %q; changing the served region needs an explicit region change",
			active.ID, active.RegionID, c.RegionID)
	}
	if active.SourceSHA256 == c.SHA256 {
		return nil
	}
	if active.DataTimestamp == nil {
		return nil
	}
	switch {
	case c.DataTimestamp.Before(*active.DataTimestamp):
		return policyErr(CodeOlderThanActive, "snapshot data timestamp %s is older than the active release %s (%s); use a rollback to serve older data",
			c.DataTimestamp.UTC().Format(time.RFC3339), active.ID, active.DataTimestamp.UTC().Format(time.RFC3339))
	case c.DataTimestamp.Equal(*active.DataTimestamp):
		return policyErr(CodeNotNewer, "snapshot has the same data timestamp %s as the active release %s but different content; refusing an ambiguous replacement",
			c.DataTimestamp.UTC().Format(time.RFC3339), active.ID)
	}
	return nil
}

// Retention decides which releases cleanup removes.
type Retention struct {
	// Keep is how many validated, non-active releases (ready or retired) are
	// retained as rollback targets, most recent first.
	Keep int
	// Margin is added to a release's pin grace before it may be removed; it
	// must exceed the API's drain period.
	Margin time.Duration
}

// Decision explains the fate of one release in a cleanup.
type Decision struct {
	ReleaseID string `json:"release_id"`
	State     string `json:"state"`
	Action    string `json:"action"` // keep | remove
	Why       string `json:"why"`
}

// recency orders releases for retention: last active first, then newest.
func recency(r registry.Release) time.Time {
	for _, t := range []*time.Time{r.DeactivatedAt, r.ActivatedAt} {
		if t != nil {
			return *t
		}
	}
	return r.CreatedAt
}

// Select returns the retention decision for every release: the active
// release and the Keep most recent validated releases are kept, releases
// still inside their pin grace (plus Margin) are kept, the rest of the
// validated releases are removed, removing releases are retried, and failed
// or removed ones are ignored.
func (p Retention) Select(releases []registry.Release, activeID string, now time.Time) []Decision {
	var validated []registry.Release
	var out []Decision
	for _, r := range releases {
		switch {
		case r.ID == activeID:
			out = append(out, Decision{r.ID, r.State, "keep", "active"})
		case r.State == registry.StateReady || r.State == registry.StateRetired:
			validated = append(validated, r)
		case r.State == registry.StateRemoving:
			out = append(out, Decision{r.ID, r.State, "remove", "an earlier removal did not finish"})
		}
	}
	sort.SliceStable(validated, func(i, j int) bool {
		ti, tj := recency(validated[i]), recency(validated[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return validated[i].ID < validated[j].ID
	})
	for i, r := range validated {
		switch {
		case i < p.Keep:
			out = append(out, Decision{r.ID, r.State, "keep", fmt.Sprintf("retained rollback target %d of %d", i+1, p.Keep)})
		case r.PinnedUntil != nil && r.PinnedUntil.After(now.Add(-p.Margin)):
			out = append(out, Decision{r.ID, r.State, "keep", "pinned by clients until " + r.PinnedUntil.UTC().Format(time.RFC3339) + " plus the drain margin"})
		default:
			out = append(out, Decision{r.ID, r.State, "remove", "beyond the retention count and not pinned"})
		}
	}
	return out
}
