package publish

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pouriya-sedaghat/karta/internal/registry"
	"github.com/pouriya-sedaghat/karta/internal/releaseid"
)

// RegistrySummary is what a backup records about the registry
// (scripts/backup.sh, `karta registry-summary`) and what a restore is
// checked against (`karta restore-check`).
type RegistrySummary struct {
	TakenAt       time.Time `json:"taken_at"`
	SchemaVersion int       `json:"schema_version"`
	// ActiveRelease is "" when no release is active.
	ActiveRelease string `json:"active_release_id"`
	// Releases maps every release that has not been removed to its state.
	Releases   map[string]string `json:"releases"`
	AuditMaxID int64             `json:"audit_max_id"`
	AuditCount int64             `json:"audit_count"`
	// SourceSerials is the newest signed-manifest serial the publisher
	// verified, per region: the online anti-replay floor.
	SourceSerials map[string]int64 `json:"source_serials"`
	// OnlinePaused is whether automatic online activation is paused, per
	// region with an online policy.
	OnlinePaused map[string]bool `json:"online_paused"`
}

// Summary reads the registry summary.
func (s *Service) Summary(ctx context.Context) (RegistrySummary, error) {
	sum := RegistrySummary{TakenAt: s.now().UTC(), Releases: map[string]string{}, SourceSerials: map[string]int64{}, OnlinePaused: map[string]bool{}}
	var err error
	if sum.SchemaVersion, err = registry.ReadSchemaVersion(ctx, s.reg); err != nil {
		return sum, err
	}
	active, err := registry.Active(ctx, s.reg)
	if err != nil {
		return sum, err
	}
	if active != nil {
		sum.ActiveRelease = active.ID
	}
	rels, err := registry.List(ctx, s.reg, 100000)
	if err != nil {
		return sum, err
	}
	for _, r := range rels {
		if r.State != registry.StateRemoved {
			sum.Releases[r.ID] = r.State
		}
	}
	if err := s.reg.QueryRow(ctx, `SELECT coalesce(max(id), 0), count(*) FROM registry.audit`).Scan(&sum.AuditMaxID, &sum.AuditCount); err != nil {
		return sum, err
	}
	rows, err := s.reg.Query(ctx, `SELECT region_id, last_serial FROM registry.source_state`)
	if err != nil {
		return sum, err
	}
	for rows.Next() {
		var region string
		var serial int64
		if err := rows.Scan(&region, &serial); err != nil {
			rows.Close()
			return sum, err
		}
		sum.SourceSerials[region] = serial
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return sum, err
	}
	rows, err = s.reg.Query(ctx, `SELECT region_id, NOT auto_activate FROM registry.online_policy`)
	if err != nil {
		return sum, err
	}
	for rows.Next() {
		var region string
		var paused bool
		if err := rows.Scan(&region, &paused); err != nil {
			rows.Close()
			return sum, err
		}
		sum.OnlinePaused[region] = paused
	}
	rows.Close()
	return sum, rows.Err()
}

// RestoreCheck is one verification of a restored registry and its releases.
type RestoreCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// RestoreReport is the result of CheckRestore.
type RestoreReport struct {
	Summary RegistrySummary `json:"summary"`
	Checks  []RestoreCheck  `json:"checks"`
	// Notes are differences that need an operator's attention but do not
	// make the restore unusable.
	Notes  []string `json:"notes"`
	Passed bool     `json:"passed"`
}

func (r *RestoreReport) check(name string, ok bool, format string, args ...any) {
	r.Checks = append(r.Checks, RestoreCheck{Name: name, Passed: ok, Detail: fmt.Sprintf(format, args...)})
}

// RestoreExpect is what the backup recorded, to compare a restore with.
type RestoreExpect struct {
	Summary *RegistrySummary
	// ChangedDuringBackup: the registry changed while the backup ran, so
	// release differences are reported as notes rather than failures.
	ChangedDuringBackup bool
	// FetcherSerial is the highest serial in the restored fetcher state
	// (nil without online updates or without that state).
	FetcherSerial *int64
}

// CheckRestore verifies a restored registry and its release databases: the
// schema, the active release and every retained release database (present,
// frozen read-only, servable by this server), the append-only audit log,
// and, given what the backup recorded, that no release, audit history or
// anti-replay state went missing. It changes nothing.
func (s *Service) CheckRestore(ctx context.Context, exp RestoreExpect) (RestoreReport, error) {
	rep := RestoreReport{Checks: []RestoreCheck{}, Notes: []string{}}
	sum, err := s.Summary(ctx)
	if err != nil {
		return rep, err
	}
	rep.Summary = sum
	rep.check("registry_schema", sum.SchemaVersion == registry.SchemaVersion, "schema version %d (this build: %d)", sum.SchemaVersion, registry.SchemaVersion)

	rels, err := registry.List(ctx, s.reg, 100000)
	if err != nil {
		return rep, err
	}
	present := map[string]bool{}
	rows, err := s.reg.Query(ctx, `SELECT datname FROM pg_database WHERE datname ~ '^karta_(r|c)[0-9a-f]{24}$'`)
	if err != nil {
		return rep, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return rep, err
	}
	candidates := 0
	for _, n := range names {
		present[n] = true
		if strings.HasPrefix(n, "karta_c") {
			candidates++
		}
	}
	var missing, mutable, incompatible []string
	retained := 0
	for _, r := range rels {
		switch r.State {
		case registry.StateActive, registry.StateReady, registry.StateRetired:
		case registry.StateImporting, registry.StateValidating:
			rep.Notes = append(rep.Notes, fmt.Sprintf("release %s was %s when the backup was taken; the publisher's recovery marks it failed at start", r.ID, r.State))
			continue
		default:
			continue
		}
		retained++
		if !releaseid.Valid(r.ID) || !present[r.Database] {
			missing = append(missing, r.ID)
			continue
		}
		ro, err := s.readOnly(ctx, r.Database)
		if err != nil || !ro {
			mutable = append(mutable, r.ID)
		}
		if err := s.checkCompatible(ctx, &r); err != nil {
			if r.State == registry.StateActive {
				incompatible = append(incompatible, r.ID+": "+err.Error())
			} else {
				rep.Notes = append(rep.Notes, fmt.Sprintf("retained release %s cannot be served by this server (%v); rollback to it is refused", r.ID, err))
			}
		}
	}
	rep.check("release_databases_present", len(missing) == 0, "%d retained release(s); missing databases: %v", retained, missing)
	rep.check("release_databases_read_only", len(mutable) == 0, "not frozen read-only: %v", mutable)
	if sum.ActiveRelease == "" {
		rep.check("active_release", exp.Summary == nil || exp.Summary.ActiveRelease == "", "no active release")
	} else {
		rep.check("active_release", len(incompatible) == 0, "active %s; incompatible: %v", sum.ActiveRelease, incompatible)
	}
	if candidates > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%d leftover candidate database(s); the publisher's recovery drops them at start", candidates))
	}

	var triggers int
	if err := s.reg.QueryRow(ctx, `
SELECT count(*) FROM pg_trigger WHERE tgrelid = 'registry.audit'::regclass AND tgenabled <> 'D'
  AND tgname IN ('audit_append_only', 'audit_no_truncate')`).Scan(&triggers); err != nil {
		return rep, err
	}
	rep.check("audit_append_only", triggers == 2, "%d of 2 append-only triggers enabled", triggers)

	if e := exp.Summary; e != nil {
		rep.check("active_release_as_backed_up", sum.ActiveRelease == e.ActiveRelease,
			"active %q, backup recorded %q", sum.ActiveRelease, e.ActiveRelease)
		rep.check("audit_history_complete", sum.AuditMaxID >= e.AuditMaxID && sum.AuditCount >= e.AuditCount,
			"audit records up to id %d (%d records); backup recorded up to %d (%d)", sum.AuditMaxID, sum.AuditCount, e.AuditMaxID, e.AuditCount)
		var lost, changed []string
		for id, st := range e.Releases {
			got, ok := sum.Releases[id]
			switch {
			case !ok:
				lost = append(lost, id)
			case got != st:
				changed = append(changed, fmt.Sprintf("%s %s->%s", id, st, got))
			}
		}
		sort.Strings(lost)
		sort.Strings(changed)
		if exp.ChangedDuringBackup {
			if len(lost)+len(changed) > 0 {
				rep.Notes = append(rep.Notes, fmt.Sprintf("the registry changed while the backup ran; releases missing %v, changed %v", lost, changed))
			}
		} else {
			rep.check("releases_as_backed_up", len(lost) == 0 && len(changed) == 0, "missing %v, changed %v", lost, changed)
		}
		var lowered []string
		for region, serial := range e.SourceSerials {
			if sum.SourceSerials[region] < serial {
				lowered = append(lowered, fmt.Sprintf("%s %d<%d", region, sum.SourceSerials[region], serial))
			}
		}
		sort.Strings(lowered)
		rep.check("anti_replay_floor_kept", len(lowered) == 0, "verified manifest serials lowered: %v", lowered)
	}
	if f := exp.FetcherSerial; f != nil {
		highest := int64(0)
		for _, v := range sum.SourceSerials {
			highest = max(highest, v)
		}
		if *f > highest {
			rep.Notes = append(rep.Notes, fmt.Sprintf("the restored fetcher state verified serial %d, above the registry's %d: that manifest was "+
				"never delivered; confirm the producer's current serial before resuming automatic activation", *f, highest))
		}
	}
	rep.Passed = true
	for _, c := range rep.Checks {
		rep.Passed = rep.Passed && c.Passed
	}
	return rep, nil
}

// readOnly reports whether a release database is frozen read-only (the
// database default its build set).
func (s *Service) readOnly(ctx context.Context, database string) (bool, error) {
	conn, err := s.cfg.Build.DB.Connect(ctx, database)
	if err != nil {
		return false, err
	}
	defer conn.Close(context.Background())
	var v string
	err = conn.QueryRow(ctx, `SELECT current_setting('default_transaction_read_only')`).Scan(&v)
	return v == "on", err
}

// ErrRestoreFailed is a restore whose checks did not pass.
var ErrRestoreFailed = errors.New("the restored registry failed its checks")

// FinalizeRestore records a restore that passed its checks: it pauses
// automatic activation of online snapshots for the region (a restored
// registry may hold an older anti-replay serial than the source has
// published since the backup; an operator confirms the producer's current
// serial and resumes) and audits the restored active pointer like a
// rollback, in one transaction.
func (s *Service) FinalizeRestore(ctx context.Context, p Principal, backupID, reason string, rep RestoreReport, fetcherSerial *int64) error {
	if !rep.Passed {
		return ErrRestoreFailed
	}
	regionID := s.regionID()
	tx, err := s.reg.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	detail := map[string]any{"cause": "restore", "backup_id": backupID}
	if _, err := registry.SetOnlineAutoActivate(ctx, tx, regionID, false, p.Name, p.Source,
		"paused by a restore: confirm the producer's current manifest serial, then resume ("+reason+")", p.RequestID, detail); err != nil {
		return err
	}
	if err := registry.Audit(ctx, tx, registry.AuditEntry{Actor: p.Name, Source: p.Source, Action: "restore", Target: rep.Summary.ActiveRelease,
		Outcome: registry.OutcomeSucceeded, Reason: reason, RequestID: p.RequestID, Detail: map[string]any{
			"backup_id": backupID, "active_release_id": rep.Summary.ActiveRelease, "source_serials": rep.Summary.SourceSerials,
			"fetcher_serial": fetcherSerial, "releases": len(rep.Summary.Releases), "notes": rep.Notes,
			"online_activation": "paused",
		}}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
