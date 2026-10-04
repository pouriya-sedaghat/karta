package publish

import (
	"strings"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/registry"
)

// summary builds a registry summary: active release, releases (id=state),
// audit high-water mark and the verified serial of region "r".
func summary(active string, releases map[string]string, audit, serial int64) RegistrySummary {
	return RegistrySummary{TakenAt: time.Now(), SchemaVersion: registry.SchemaVersion, ActiveRelease: active, Releases: releases,
		AuditMaxID: audit, AuditCount: audit, SourceSerials: map[string]int64{"r": serial}, OnlinePaused: map[string]bool{}}
}

func failed(rep RestoreReport) []string {
	var names []string
	for _, c := range rep.Checks {
		if !c.Passed {
			names = append(names, c.Name+": "+c.Detail)
		}
	}
	return names
}

func TestCompareWithBackup(t *testing.T) {
	a, b := "ra", "rb"
	before := summary(a, map[string]string{a: "active", b: "retired", "rx": "retired"}, 100, 42)
	// While the base backup ran: a rollback to rb (audit 101) and a cleanup
	// of rx (audit 102-103); a verified serial 43.
	after := summary(b, map[string]string{a: "retired", b: "active"}, 103, 43)
	none := pointerMove{}
	toB := pointerMove{Release: b, Found: true}

	cases := []struct {
		name       string
		got        RegistrySummary
		before     RegistrySummary
		after      *RegistrySummary // nil: after
		move       pointerMove
		wantFailed []string // check names; nil = passes
		wantNote   string
	}{
		{name: "nothing changed: an exact restore", got: before, before: before, after: &before, move: none},
		{name: "nothing changed: another active release", got: summary(b, before.Releases, 100, 42), before: before, after: &before, move: none,
			wantFailed: []string{"active_release_as_backed_up"}},
		{name: "nothing changed: a release state differs", got: summary(a, map[string]string{a: "active", b: "ready", "rx": "retired"}, 100, 42),
			before: before, after: &before, move: none, wantFailed: []string{"releases_as_backed_up"}},
		{name: "nothing changed: an extra release", got: summary(a, map[string]string{a: "active", b: "retired", "rx": "retired", "rz": "ready"}, 100, 42),
			before: before, after: &before, move: none, wantFailed: []string{"releases_as_backed_up"}},

		// The switch happened before the base backup ended: the restore
		// has it, the first summary does not.
		{name: "switch during the base backup", got: summary(b, map[string]string{a: "retired", b: "active", "rx": "retired"}, 101, 42),
			before: before, move: toB, wantNote: "changed while the backup ran"},
		// Everything after the switch also happened before the end.
		{name: "switch and cleanup during the base backup", got: summary(b, map[string]string{a: "retired", b: "active"}, 103, 43),
			before: before, move: toB, wantNote: "removed while the backup ran"},
		// The switch happened after the base backup ended but before the
		// second summary: the restore is the first summary's state. A
		// comparison with the second summary alone refused this restore.
		{name: "switch after the base backup ended", got: before, before: before, move: none, wantNote: "reached 43 after the base backup ended"},

		{name: "active release contradicts the restored audit log", got: summary(a, map[string]string{a: "retired", b: "active", "rx": "retired"}, 101, 42),
			before: before, move: toB, wantFailed: []string{"active_release_as_backed_up"}},
		{name: "audit history older than the backup", got: summary(a, before.Releases, 99, 42), before: before, move: none,
			wantFailed: []string{"audit_history_complete"}},
		{name: "newer than the backup", got: summary(b, after.Releases, 104, 43), before: before, move: toB,
			wantFailed: []string{"restored_within_backup"}},
		{name: "serial newer than the backup", got: summary(b, after.Releases, 103, 44), before: before, move: toB,
			wantFailed: []string{"restored_within_backup"}},
		{name: "anti-replay floor lowered", got: summary(a, before.Releases, 100, 41), before: before, move: none,
			wantFailed: []string{"anti_replay_floor_kept"}},
		{name: "a release that existed throughout is missing", got: summary(b, map[string]string{b: "active", "rx": "retired"}, 101, 42),
			before: before, move: toB, wantFailed: []string{"releases_as_backed_up"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := RestoreReport{}
			exp := after
			if c.after != nil {
				exp = *c.after
			}
			compareWithBackup(&rep, c.got, c.before, exp, c.move)
			got := failed(rep)
			if len(got) != len(c.wantFailed) {
				t.Fatalf("failed checks %v, want %v; notes %v", got, c.wantFailed, rep.Notes)
			}
			for i, name := range c.wantFailed {
				if !strings.HasPrefix(got[i], name+":") {
					t.Errorf("failed check %q, want %s", got[i], name)
				}
			}
			if c.wantNote != "" && !strings.Contains(strings.Join(rep.Notes, "\n"), c.wantNote) {
				t.Errorf("notes %v lack %q", rep.Notes, c.wantNote)
			}
		})
	}
}

func TestSameSummaryIgnoresTime(t *testing.T) {
	x := summary("ra", map[string]string{"ra": "active"}, 5, 1)
	y := x
	y.TakenAt = x.TakenAt.Add(time.Hour)
	if !SameSummary(x, y) {
		t.Error("summaries taken at different times differ")
	}
	y.AuditMaxID++
	if SameSummary(x, y) {
		t.Error("a new audit record is not a change")
	}
}
