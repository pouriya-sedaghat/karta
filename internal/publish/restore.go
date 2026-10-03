package publish

import (
	"context"
	"encoding/json"
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

// Summary reads the registry summary from one snapshot (a repeatable-read
// transaction), so its active release, releases, audit high-water mark and
// serials belong to the same moment even while the registry changes.
func (s *Service) Summary(ctx context.Context) (RegistrySummary, error) {
	tx, err := s.reg.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return RegistrySummary{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	return s.summary(ctx, tx)
}

func (s *Service) summary(ctx context.Context, q registry.Querier) (RegistrySummary, error) {
	sum := RegistrySummary{TakenAt: s.now().UTC(), Releases: map[string]string{}, SourceSerials: map[string]int64{}, OnlinePaused: map[string]bool{}}
	var err error
	if sum.SchemaVersion, err = registry.ReadSchemaVersion(ctx, q); err != nil {
		return sum, err
	}
	active, err := registry.Active(ctx, q)
	if err != nil {
		return sum, err
	}
	if active != nil {
		sum.ActiveRelease = active.ID
	}
	rels, err := registry.List(ctx, q, 100000)
	if err != nil {
		return sum, err
	}
	for _, r := range rels {
		if r.State != registry.StateRemoved {
			sum.Releases[r.ID] = r.State
		}
	}
	if err := q.QueryRow(ctx, `SELECT coalesce(max(id), 0), count(*) FROM registry.audit`).Scan(&sum.AuditMaxID, &sum.AuditCount); err != nil {
		return sum, err
	}
	rows, err := q.Query(ctx, `SELECT region_id, last_serial FROM registry.source_state`)
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
	rows, err = q.Query(ctx, `SELECT region_id, NOT auto_activate FROM registry.online_policy`)
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
	// Before and After are the summaries the backup took just before and
	// just after its base backup (registry.before.json and
	// registry.after.json). A physical base backup restores the registry
	// as it was when the base backup ended, a moment between the two: it
	// may hold a release switch, cleanup or verified serial that After has
	// and Before has not, or miss one that happened after the base backup
	// ended. Without Before, the restore must equal After.
	Before, After *RegistrySummary
	// FetcherSerial is the highest serial in the restored fetcher state
	// (nil without online updates or without that state).
	FetcherSerial *int64
}

// SameSummary reports whether two summaries describe the same registry
// state (ignoring when they were taken).
func SameSummary(a, b RegistrySummary) bool {
	a.TakenAt, b.TakenAt = time.Time{}, time.Time{}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
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
		// Whether that is what the backup holds is checked below.
		rep.check("active_release", true, "no active release")
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

	if exp.After != nil {
		before := exp.After
		if exp.Before != nil {
			before = exp.Before
		}
		move, err := s.lastPointerMove(ctx, before.AuditMaxID)
		if err != nil {
			return rep, err
		}
		compareWithBackup(&rep, sum, *before, *exp.After, move)
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

// pointerMove is the newest move of the active pointer that the restored
// audit log records after a given audit record.
type pointerMove struct {
	Release string // the release it activated
	Found   bool
}

// lastPointerMove finds the newest pointer move after audit record afterID.
// Every move of the active pointer is audited, with the release it
// activated, in the transaction that makes it (registry.Activate), so the
// restored audit log tells exactly where the pointer went after a summary.
func (s *Service) lastPointerMove(ctx context.Context, afterID int64) (pointerMove, error) {
	var m pointerMove
	err := s.reg.QueryRow(ctx, `
SELECT detail->>'activated_release_id' FROM registry.audit
WHERE id > $1 AND outcome = $2 AND detail->>'activated_release_id' IS NOT NULL
ORDER BY id DESC LIMIT 1`, afterID, registry.OutcomeSucceeded).Scan(&m.Release)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, nil
	}
	m.Found = err == nil
	return m, err
}

// compareWithBackup checks a restored registry summary against the two
// summaries a backup took around its base backup (see RestoreExpect). The
// restored registry must lie between them: the append-only audit log and
// the verified serials only grow, so it can hold nothing the second
// summary lacks and must hold everything the first one has. The active
// release must be the one the restored audit log's last pointer move after
// the first summary activated, or the first summary's if there was none.
// When nothing changed while the backup ran, this means an exact match.
func compareWithBackup(rep *RestoreReport, got, before, after RegistrySummary, move pointerMove) {
	strict := SameSummary(before, after)
	if !strict {
		rep.Notes = append(rep.Notes, fmt.Sprintf("the registry changed while the backup ran; the restore is the registry as it was when the base backup "+
			"ended: audit records up to id %d (the backup recorded %d before and %d after its base backup), active release %q (%q before, %q after)",
			got.AuditMaxID, before.AuditMaxID, after.AuditMaxID, got.ActiveRelease, before.ActiveRelease, after.ActiveRelease))
	}

	rep.check("audit_history_complete", got.AuditMaxID >= before.AuditMaxID && got.AuditCount >= before.AuditCount,
		"audit records up to id %d (%d records); the backup recorded up to %d (%d) before its base backup",
		got.AuditMaxID, got.AuditCount, before.AuditMaxID, before.AuditCount)

	var newer []string
	if got.AuditMaxID > after.AuditMaxID || got.AuditCount > after.AuditCount {
		newer = append(newer, fmt.Sprintf("audit records up to id %d (%d records), the backup recorded up to %d (%d) after its base backup",
			got.AuditMaxID, got.AuditCount, after.AuditMaxID, after.AuditCount))
	}
	for _, region := range sortedKeys(got.SourceSerials) {
		if a, ok := after.SourceSerials[region]; !ok || got.SourceSerials[region] > a {
			newer = append(newer, fmt.Sprintf("verified serial %s %d, the backup recorded %d after its base backup", region, got.SourceSerials[region], a))
		}
	}
	rep.check("restored_within_backup", len(newer) == 0, "newer than the backup: %v", newer)

	want, how := before.ActiveRelease, "no pointer move in the restored audit log after the backup's first summary"
	if move.Found {
		want, how = move.Release, "the restored audit log's last pointer move after the backup's first summary"
	}
	rep.check("active_release_as_backed_up", got.ActiveRelease == want,
		"active %q, expected %q (%s); the backup recorded %q before and %q after its base backup",
		got.ActiveRelease, want, how, before.ActiveRelease, after.ActiveRelease)

	// A release in both summaries existed for the whole backup (removal is
	// final), so the restore must have it. Without changes, also its state.
	var lost, changed []string
	for _, id := range sortedKeys(before.Releases) {
		g, ok := got.Releases[id]
		_, inAfter := after.Releases[id]
		switch {
		case !ok && inAfter:
			lost = append(lost, id)
		case !ok:
			rep.Notes = append(rep.Notes, fmt.Sprintf("release %s was removed while the backup ran and is not in the restore", id))
		case g != before.Releases[id] && strict:
			changed = append(changed, fmt.Sprintf("%s %s->%s", id, before.Releases[id], g))
		case g != before.Releases[id]:
			rep.Notes = append(rep.Notes, fmt.Sprintf("release %s is %s in the restore (%s before the base backup, %s after)", id, g, before.Releases[id], after.Releases[id]))
		}
	}
	if strict {
		for _, id := range sortedKeys(got.Releases) {
			if _, ok := before.Releases[id]; !ok {
				changed = append(changed, id+" not in the backup")
			}
		}
	}
	rep.check("releases_as_backed_up", len(lost) == 0 && len(changed) == 0, "missing %v, changed %v", lost, changed)

	var lowered []string
	for _, region := range sortedKeys(before.SourceSerials) {
		g := got.SourceSerials[region]
		if g < before.SourceSerials[region] {
			lowered = append(lowered, fmt.Sprintf("%s %d<%d", region, g, before.SourceSerials[region]))
		} else if a := after.SourceSerials[region]; g < a {
			rep.Notes = append(rep.Notes, fmt.Sprintf("the verified serial of %s reached %d after the base backup ended; the restore has %d: "+
				"confirm the producer's current serial before resuming automatic activation", region, a, g))
		}
	}
	rep.check("anti_replay_floor_kept", len(lowered) == 0, "verified manifest serials lowered: %v", lowered)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
	// The restored pointer is an operator decision like a rollback: the
	// unattended intake watcher does not override it either (Stage 5).
	if _, err := registry.SetIntakeWatcherAutoActivate(ctx, tx, regionID, false, p.Name, p.Source,
		"paused by a restore: check the restored release, then resume ("+reason+")", p.RequestID,
		map[string]any{"cause": "restore", "backup_id": backupID}); err != nil {
		return err
	}
	if err := registry.Audit(ctx, tx, registry.AuditEntry{Actor: p.Name, Source: p.Source, Action: "restore", Target: rep.Summary.ActiveRelease,
		Outcome: registry.OutcomeSucceeded, Reason: reason, RequestID: p.RequestID, Detail: map[string]any{
			"backup_id": backupID, "active_release_id": rep.Summary.ActiveRelease, "source_serials": rep.Summary.SourceSerials,
			"fetcher_serial": fetcherSerial, "releases": len(rep.Summary.Releases), "notes": rep.Notes,
			"online_activation": "paused", "intake_watcher_activation": "paused",
		}}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
