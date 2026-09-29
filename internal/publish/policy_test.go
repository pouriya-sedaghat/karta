package publish

import (
	"errors"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/registry"
)

func ts(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

func TestForwardRule(t *testing.T) {
	active := &registry.Release{ID: "ra", RegionID: "fixture", SourceSHA256: "aaa", DataTimestamp: ts("2026-02-01T00:00:00Z")}
	cases := []struct {
		name   string
		active *registry.Release
		c      Candidate
		allow  bool
		code   string
	}{
		{"nothing active", nil, Candidate{"fixture", "bbb", *ts("2020-01-01T00:00:00Z")}, false, ""},
		{"newer", active, Candidate{"fixture", "bbb", *ts("2026-03-01T00:00:00Z")}, false, ""},
		{"older", active, Candidate{"fixture", "bbb", *ts("2026-01-01T00:00:00Z")}, false, CodeOlderThanActive},
		{"same time, other content", active, Candidate{"fixture", "bbb", *ts("2026-02-01T00:00:00Z")}, false, CodeNotNewer},
		{"same snapshot rebuilt", active, Candidate{"fixture", "aaa", *ts("2026-02-01T00:00:00Z")}, false, ""},
		{"other region", active, Candidate{"tehran", "bbb", *ts("2026-03-01T00:00:00Z")}, false, CodeRegionChanged},
		{"other region, allowed, older", active, Candidate{"tehran", "bbb", *ts("2026-01-01T00:00:00Z")}, true, CodeOlderThanActive},
		{"other region, allowed, newer", active, Candidate{"tehran", "bbb", *ts("2026-03-01T00:00:00Z")}, true, ""},
	}
	for _, c := range cases {
		err := Forward(c.active, c.c, c.allow)
		if PolicyCode(err) != c.code {
			t.Errorf("%s: %v, want code %q", c.name, err, c.code)
		}
	}
}

func TestRetentionSelect(t *testing.T) {
	now := *ts("2026-09-29T12:00:00Z")
	rels := []registry.Release{
		{ID: "r_active", State: registry.StateActive, ActivatedAt: ts("2026-09-29T11:00:00Z")},
		{ID: "r_prev", State: registry.StateRetired, ActivatedAt: ts("2026-09-28T00:00:00Z"), DeactivatedAt: ts("2026-09-29T11:00:00Z"), PinnedUntil: ts("2026-09-30T11:00:00Z")},
		{ID: "r_old", State: registry.StateRetired, ActivatedAt: ts("2026-09-01T00:00:00Z"), DeactivatedAt: ts("2026-09-28T00:00:00Z"), PinnedUntil: ts("2026-09-29T00:00:00Z")},
		{ID: "r_older_pinned", State: registry.StateRetired, DeactivatedAt: ts("2026-08-01T00:00:00Z"), PinnedUntil: ts("2026-09-29T11:59:30Z")},
		{ID: "r_ready", State: registry.StateReady, CreatedAt: *ts("2026-07-01T00:00:00Z")},
		{ID: "r_removing", State: registry.StateRemoving},
		{ID: "r_failed", State: registry.StateFailed},
		{ID: "r_removed", State: registry.StateRemoved},
	}
	got := map[string]string{}
	for _, d := range (Retention{Keep: 1, Margin: time.Minute}).Select(rels, "r_active", now) {
		got[d.ReleaseID] = d.Action
	}
	want := map[string]string{
		"r_active": "keep", "r_prev": "keep", "r_old": "remove",
		"r_older_pinned": "keep", // grace ended 30 s ago, inside the one-minute drain margin
		"r_ready":        "remove", "r_removing": "remove",
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: %q, want %q", id, got[id], w)
		}
	}
	for _, id := range []string{"r_failed", "r_removed"} {
		if _, ok := got[id]; ok {
			t.Errorf("%s has a decision", id)
		}
	}
	// Keep 0: only the active release and live pins survive.
	for _, d := range (Retention{Keep: 0, Margin: time.Minute}).Select(rels, "r_active", now) {
		if d.ReleaseID == "r_prev" && d.Action != "keep" {
			t.Error("a pinned release was selected for removal")
		}
	}
}

func TestTransientErrors(t *testing.T) {
	for msg, want := range map[string]bool{
		"osm2pgsql failed (exit status 1): ERROR: terminating connection due to administrator command": true,
		"connect candidate database: failed to connect to `host=db`: dial tcp: connection refused":     true,
		"release validation failed: min_count_roads: 0 rows, minimum 3":                                false,
		"setup SQL: ERROR: syntax error at or near":                                                    false,
	} {
		if got := transient(errors.New(msg)); got != want {
			t.Errorf("%q: %v, want %v", msg, got, want)
		}
	}
}
