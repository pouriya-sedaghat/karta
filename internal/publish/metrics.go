package publish

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/registry"
)

// Metrics renders the publication and freshness metrics in the Prometheus
// text exposition format (version 0.0.4). Values are computed when scraped
// from the registry and the fetcher's state; nothing upstream is contacted.
func (s *Service) Metrics(ctx context.Context) ([]byte, error) {
	st, err := s.Status(ctx)
	if err != nil {
		return nil, err
	}
	subs, err := registry.CountSubmissions(ctx, s.reg)
	if err != nil {
		return nil, err
	}
	rels, err := registry.CountReleases(ctx, s.reg)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	m := metricWriter{&b}
	ts := func(t *time.Time) *float64 {
		if t == nil || t.IsZero() {
			return nil
		}
		v := float64(t.UnixMilli()) / 1000
		return &v
	}
	f := st.Freshness
	m.gauge("karta_active_release", "1 if a release is active.", nil, boolF(st.Active != nil))
	if st.Active != nil {
		m.gauge("karta_active_release_info", "The active release (value 1).", map[string]string{"release_id": st.Active.ID, "region_id": st.Active.RegionID}, 1)
	}
	m.opt("karta_active_data_timestamp_seconds", "OSM data timestamp of the active release (Unix time).", nil, ts(f.ActiveDataTimestamp))
	m.opt("karta_active_data_age_seconds", "Age of the active release's OSM data.", nil, f.ActiveDataAgeSeconds)
	m.opt("karta_data_stale_after_seconds", "Configured staleness threshold (KARTA_DATA_STALE_AFTER); absent when not configured.", nil, f.StaleAfterSeconds)
	if f.Stale != nil {
		m.gauge("karta_data_stale", "1 if the active data is older than the staleness threshold.", nil, boolF(*f.Stale))
	}
	o := st.Online
	m.gauge("karta_online_enabled", "1 if online updates are enabled on this publisher.", nil, boolF(o.Enabled))
	if o.Enabled {
		m.gauge("karta_online_auto_activation", "1 if validated online snapshots are activated automatically (not paused).", nil, boolF(o.AutoActivate))
		if v := o.Verified; v != nil {
			m.gauge("karta_online_verified_serial", "Serial of the newest manifest the publisher verified.", nil, float64(v.Serial))
			m.opt("karta_online_verified_data_timestamp_seconds", "Data timestamp signed in the newest manifest the publisher verified.", nil, ts(&v.DataTimestamp))
			m.opt("karta_online_verified_timestamp_seconds", "When the publisher last verified a new manifest.", nil, ts(&v.VerifiedAt))
		}
		m.opt("karta_online_fetcher_state_age_seconds", "Seconds since the fetcher last wrote its state; growing means it is not running.", nil, o.Fetcher.StateAgeSeconds)
		if fs := o.Fetcher.State; fs != nil {
			m.opt("karta_online_last_check_timestamp_seconds", "Last source check started (fetcher report).", nil, ts(fs.LastCheckAt))
			m.opt("karta_online_last_success_timestamp_seconds", "Last source check that succeeded (fetcher report).", nil, ts(fs.LastSuccessAt))
			m.opt("karta_online_next_attempt_timestamp_seconds", "Next scheduled source check (fetcher report).", nil, ts(fs.NextAttemptAt))
			m.gauge("karta_online_consecutive_failures", "Consecutive failed source checks (fetcher report).", nil, float64(fs.ConsecutiveFailures))
			code := ""
			if fs.LastError != nil {
				code = fs.LastError.Code
			}
			m.gauge("karta_online_last_error", "1 with the code of the last failed check, if the last check failed (fetcher report).",
				map[string]string{"code": code}, boolF(fs.LastError != nil))
			if d := fs.Download; d != nil {
				m.gauge("karta_online_download_bytes", "Bytes of the snapshot being downloaded (fetcher report).", nil, float64(d.Bytes))
				m.gauge("karta_online_download_size_bytes", "Signed size of the snapshot being downloaded (fetcher report).", nil, float64(d.SizeBytes))
			}
		}
	}
	keys := make([][2]string, 0, len(subs))
	for k := range subs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+keys[i][1] < keys[j][0]+keys[j][1] })
	for i, k := range keys {
		help := ""
		if i == 0 {
			help = "Recorded submissions by source and state."
		}
		m.sample("karta_submissions", help, map[string]string{"source": k[0], "state": k[1]}, float64(subs[k]), i == 0)
	}
	states := make([]string, 0, len(rels))
	for k := range rels {
		states = append(states, k)
	}
	sort.Strings(states)
	for i, k := range states {
		help := ""
		if i == 0 {
			help = "Releases by state."
		}
		m.sample("karta_releases", help, map[string]string{"state": k}, float64(rels[k]), i == 0)
	}
	m.gauge("karta_release_storage_bytes", "Bytes used by release and candidate databases.", nil, float64(st.Storage.ReleaseBytes))
	m.gauge("karta_release_storage_budget_bytes", "Configured release storage budget (0 = none).", nil, float64(st.Storage.BudgetBytes))
	m.gauge("karta_publication_in_progress", "1 while a publication is running.", nil, boolF(st.Job != nil))
	return b.Bytes(), nil
}

type metricWriter struct{ b *bytes.Buffer }

func (m metricWriter) sample(name, help string, labels map[string]string, v float64, header bool) {
	if header {
		fmt.Fprintf(m.b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}
	m.b.WriteString(name)
	if len(labels) > 0 {
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+`="`+escapeLabel(labels[k])+`"`)
		}
		m.b.WriteString("{" + strings.Join(parts, ",") + "}")
	}
	m.b.WriteString(" " + strconv.FormatFloat(v, 'g', -1, 64) + "\n")
}

func (m metricWriter) gauge(name, help string, labels map[string]string, v float64) {
	m.sample(name, help, labels, v, true)
}

func (m metricWriter) opt(name, help string, labels map[string]string, v *float64) {
	if v != nil {
		m.gauge(name, help, labels, *v)
	}
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func boolF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
