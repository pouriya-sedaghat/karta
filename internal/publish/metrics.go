package publish

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/promtext"
	"github.com/pouriya-sedaghat/karta/internal/registry"
)

// publicationBuckets are the duration histogram's upper bounds in seconds,
// from a small fixture build to a day.
var publicationBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400, 28800, 86400}

// pubStats counts the publications this process finished since it started
// (counters reset on restart; the registry's submission counts persist).
type pubStats struct {
	mu        sync.Mutex
	outcomes  map[[2]string]uint64           // source, state
	durations map[string]*promtext.Histogram // per source
	timeouts  uint64
}

func (p *pubStats) record(source, state, code string, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.outcomes == nil {
		p.outcomes, p.durations = map[[2]string]uint64{}, map[string]*promtext.Histogram{}
	}
	p.outcomes[[2]string{source, state}]++
	h := p.durations[source]
	if h == nil {
		h = promtext.NewHistogram(publicationBuckets...)
		p.durations[source] = h
	}
	h.Observe(d.Seconds())
	if code == CodePublicationTimeout {
		p.timeouts++
	}
}

func (p *pubStats) snapshot() (map[[2]string]uint64, map[string]promtext.Snapshot, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[[2]string]uint64, len(p.outcomes))
	for k, v := range p.outcomes {
		out[k] = v
	}
	hs := make(map[string]promtext.Snapshot, len(p.durations))
	for k, h := range p.durations {
		hs[k] = h.Snapshot()
	}
	return out, hs, p.timeouts
}

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
	m := promtext.New(&b)
	ts := func(t *time.Time) *float64 {
		if t == nil || t.IsZero() {
			return nil
		}
		v := float64(t.UnixMilli()) / 1000
		return &v
	}
	f := st.Freshness
	m.Gauge("karta_active_release", "1 if a release is active.", nil, promtext.Bool(st.Active != nil))
	if st.Active != nil {
		m.Gauge("karta_active_release_info", "The active release (value 1).", map[string]string{"release_id": st.Active.ID, "region_id": st.Active.RegionID}, 1)
	}
	m.Opt("karta_active_data_timestamp_seconds", "OSM data timestamp of the active release (Unix time).", nil, ts(f.ActiveDataTimestamp))
	m.Opt("karta_active_data_age_seconds", "Age of the active release's OSM data.", nil, f.ActiveDataAgeSeconds)
	m.Opt("karta_data_stale_after_seconds", "Configured staleness threshold (KARTA_DATA_STALE_AFTER); absent when not configured.", nil, f.StaleAfterSeconds)
	if f.Stale != nil {
		m.Gauge("karta_data_stale", "1 if the active data is older than the staleness threshold.", nil, promtext.Bool(*f.Stale))
	}
	o := st.Online
	m.Gauge("karta_online_enabled", "1 if online updates are enabled on this publisher.", nil, promtext.Bool(o.Enabled))
	if o.Enabled {
		m.Gauge("karta_online_auto_activation", "1 if validated online snapshots are activated automatically (not paused).", nil, promtext.Bool(o.AutoActivate))
		if v := o.Verified; v != nil {
			m.Gauge("karta_online_verified_serial", "Serial of the newest manifest the publisher verified.", nil, float64(v.Serial))
			m.Opt("karta_online_verified_data_timestamp_seconds", "Data timestamp signed in the newest manifest the publisher verified.", nil, ts(&v.DataTimestamp))
			m.Opt("karta_online_verified_timestamp_seconds", "When the publisher last verified a new manifest.", nil, ts(&v.VerifiedAt))
		}
		m.Opt("karta_online_fetcher_state_age_seconds", "Seconds since the fetcher last wrote its state; growing means it is not running.", nil, o.Fetcher.StateAgeSeconds)
		if fs := o.Fetcher.State; fs != nil {
			m.Opt("karta_online_last_check_timestamp_seconds", "Last source check started (fetcher report).", nil, ts(fs.LastCheckAt))
			m.Opt("karta_online_last_success_timestamp_seconds", "Last source check that succeeded (fetcher report).", nil, ts(fs.LastSuccessAt))
			m.Opt("karta_online_next_attempt_timestamp_seconds", "Next scheduled source check (fetcher report).", nil, ts(fs.NextAttemptAt))
			m.Gauge("karta_online_consecutive_failures", "Consecutive failed source checks (fetcher report).", nil, float64(fs.ConsecutiveFailures))
			code := ""
			if fs.LastError != nil {
				code = fs.LastError.Code
			}
			m.Gauge("karta_online_last_error", "1 with the code of the last failed check, if the last check failed (fetcher report).",
				map[string]string{"code": code}, promtext.Bool(fs.LastError != nil))
			if d := fs.Download; d != nil {
				m.Gauge("karta_online_download_bytes", "Bytes of the snapshot being downloaded (fetcher report).", nil, float64(d.Bytes))
				m.Gauge("karta_online_download_size_bytes", "Signed size of the snapshot being downloaded (fetcher report).", nil, float64(d.SizeBytes))
			}
		}
	}
	in := st.Intake
	m.Gauge("karta_intake_enabled", "1 if the local intake (KARTA_INTAKE_DIR) is enabled on this publisher.", nil, promtext.Bool(in.Enabled))
	if in.Enabled {
		m.Gauge("karta_intake_watcher_auto_activation", "1 if deliveries admitted only by the intake watcher are activated automatically (not paused).",
			nil, promtext.Bool(in.AutoActivate))
		chans := make([]string, 0, len(in.OpenAuthorizations))
		for c := range in.OpenAuthorizations {
			chans = append(chans, c)
		}
		sort.Strings(chans)
		m.Header("karta_intake_open_authorizations", "Open, unexpired intake authorizations by channel (intake_watch, intake_submit).", "gauge")
		for _, c := range chans {
			m.Sample("karta_intake_open_authorizations", map[string]string{"channel": c}, float64(in.OpenAuthorizations[c]))
		}
		m.Opt("karta_intake_watcher_state_age_seconds", "Seconds since the intake watcher last wrote its state; growing means it is not running.",
			nil, in.Watcher.StateAgeSeconds)
		if ws := in.Watcher.State; ws != nil {
			m.Gauge("karta_intake_preflight_ok", "1 if the watcher's last landing preflight passed (watcher report).", nil, promtext.Bool(ws.Preflight.OK))
			m.Opt("karta_intake_last_scan_timestamp_seconds", "Last landing scan of the watcher (watcher report).", nil, ts(ws.LastScanAt))
			waiting, refused := 0, 0
			for _, e := range ws.Entries {
				if e.State == "refused" {
					refused++
				} else if e.State != "delivered" {
					waiting++
				}
			}
			m.Gauge("karta_intake_landing_waiting", "Landing deliveries waiting for completion, settling or a free slot (watcher report).", nil, float64(waiting))
			m.Gauge("karta_intake_landing_refused", "Landing deliveries the watcher refused (watcher report).", nil, float64(refused))
		}
	}
	keys := make([][2]string, 0, len(subs))
	for k := range subs {
		keys = append(keys, k)
	}
	sortPairs(keys)
	if len(keys) > 0 {
		m.Header("karta_submissions", "Recorded submissions by source and state (registry; survives restarts).", "gauge")
	}
	for _, k := range keys {
		m.Sample("karta_submissions", map[string]string{"source": k[0], "state": k[1]}, float64(subs[k].N))
	}
	// A new failure shows here even when the count above stays the same (an
	// older failure retried as it happens) and across publisher restarts
	// (it is read from the registry, unlike karta_publications_total).
	header := false
	for _, k := range keys {
		v := ts(subs[k].LastFinished)
		if v == nil {
			continue
		}
		if !header {
			m.Header("karta_submission_last_finished_timestamp_seconds",
				"When the latest submission now in this source and state finished (Unix time; registry; survives restarts).", "gauge")
			header = true
		}
		m.Sample("karta_submission_last_finished_timestamp_seconds", map[string]string{"source": k[0], "state": k[1]}, *v)
	}
	states := make([]string, 0, len(rels))
	for k := range rels {
		states = append(states, k)
	}
	sort.Strings(states)
	if len(states) > 0 {
		m.Header("karta_releases", "Releases by state.", "gauge")
	}
	for _, k := range states {
		m.Sample("karta_releases", map[string]string{"state": k}, float64(rels[k]))
	}
	m.Gauge("karta_release_storage_bytes", "Bytes used by release and candidate databases.", nil, float64(st.Storage.ReleaseBytes))
	m.Gauge("karta_release_storage_budget_bytes", "Configured release storage budget (0 = none).", nil, float64(st.Storage.BudgetBytes))
	free := func(v *int64) *float64 {
		if v == nil {
			return nil
		}
		f := float64(*v)
		return &f
	}
	m.Opt("karta_staging_free_bytes", "Free bytes on the staging volume.", nil, free(st.Storage.StagingFreeBytes))
	m.Opt("karta_db_volume_free_bytes", "Free bytes on the database volume (only with KARTA_DB_VOLUME_PATH).", nil, free(st.Storage.DBVolumeFreeBytes))

	// Publications this process ran.
	m.Gauge("karta_publication_in_progress", "1 while a publication is running.", nil, promtext.Bool(st.Job != nil))
	if st.Job != nil {
		m.Gauge("karta_publication_running_seconds", "How long the publication in progress has been running.", nil, s.now().Sub(st.Job.StartedAt).Seconds())
	}
	if s.cfg.PublishTimeout > 0 {
		m.Gauge("karta_publication_timeout_seconds", "Whole-publication deadline (KARTA_PUBLISH_TIMEOUT).", nil, s.cfg.PublishTimeout.Seconds())
	}
	outcomes, durations, timeouts := s.stats.snapshot()
	okeys := make([][2]string, 0, len(outcomes))
	for k := range outcomes {
		okeys = append(okeys, k)
	}
	sortPairs(okeys)
	if len(okeys) > 0 {
		m.Header("karta_publications_total", "Publications this process finished, by source and final state.", "counter")
	}
	for _, k := range okeys {
		m.Sample("karta_publications_total", map[string]string{"source": k[0], "state": k[1]}, float64(outcomes[k]))
	}
	m.Counter("karta_publication_timeouts_total", "Publications this process stopped at their deadline.", nil, float64(timeouts))
	srcs := make([]string, 0, len(durations))
	for k := range durations {
		srcs = append(srcs, k)
	}
	sort.Strings(srcs)
	if len(srcs) > 0 {
		m.Header("karta_publication_duration_seconds", "Duration of the publications this process finished, from staging to the outcome.", "histogram")
	}
	for _, k := range srcs {
		m.HistogramSamples("karta_publication_duration_seconds", map[string]string{"source": k}, durations[k])
	}
	m.Gauge("karta_publisher_start_time_seconds", "When this publisher process started (Unix time).", nil, float64(s.started.UnixMilli())/1000)
	return b.Bytes(), nil
}

func sortPairs(keys [][2]string) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
}
