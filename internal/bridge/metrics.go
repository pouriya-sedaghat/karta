package bridge

import (
	"bytes"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/promtext"
)

// Metrics renders the bridge's own status (acquire's and sign's reports) in
// the Prometheus text format, for a co-located bridge's operator. Karta
// itself never connects to the bridge to monitor it: its view stays the
// fetcher's report and the data age.
func Metrics(spool, publish string, now time.Time) []byte {
	a, s, err := Statuses(spool, publish)
	var b bytes.Buffer
	m := promtext.New(&b)
	ts := func(t *time.Time) *float64 {
		if t == nil || t.IsZero() {
			return nil
		}
		v := float64(t.UnixMilli()) / 1000
		return &v
	}
	m.Gauge("karta_bridge_status_readable", "1 if the acquire and sign reports could be read.", nil, promtext.Bool(err == nil))
	if a != nil {
		age := now.Sub(a.UpdatedAt).Seconds()
		m.Gauge("karta_bridge_acquire_state_age_seconds", "Seconds since acquire last wrote its state.", nil, age)
		m.Opt("karta_bridge_acquire_last_check_timestamp_seconds", "Last distributor check (hints).", nil, ts(a.LastCheckAt))
		m.Opt("karta_bridge_acquire_last_success_timestamp_seconds", "Last successful distributor check.", nil, ts(a.LastSuccessAt))
		m.Opt("karta_bridge_acquire_next_attempt_timestamp_seconds", "Next scheduled check.", nil, ts(a.NextAttemptAt))
		m.Gauge("karta_bridge_acquire_consecutive_failures", "Consecutive failed checks.", nil, float64(a.ConsecutiveFailures))
		code := ""
		if a.LastError != nil {
			code = a.LastError.Code
		}
		m.Gauge("karta_bridge_acquire_last_error", "1 with the code of the last failed check, if it failed.", map[string]string{"code": code},
			promtext.Bool(a.LastError != nil))
		if a.Last != nil {
			m.Opt("karta_bridge_acquire_verified_timestamp_seconds", "Last full download that produced the current bytes.", nil, ts(&a.Last.VerifiedAt))
		}
		if d := a.Download; d != nil {
			m.Gauge("karta_bridge_acquire_download_bytes", "Bytes of the download in progress.", nil, float64(d.Bytes))
		}
	}
	if s != nil {
		m.Gauge("karta_bridge_sign_state_age_seconds", "Seconds since sign last wrote its state.", nil, now.Sub(s.UpdatedAt).Seconds())
		m.Gauge("karta_bridge_sign_high_water_serial", "Highest manifest serial allocated.", nil, float64(s.HighWater))
		if c := s.Current; c != nil {
			m.Gauge("karta_bridge_manifest_pending", "1 if the current manifest is signed but its publication has not succeeded yet.", nil,
				promtext.Bool(!c.Promoted))
		}
		if c := s.Current; c != nil && c.Promoted {
			m.Gauge("karta_bridge_manifest_serial", "Serial of the published manifest.", nil, float64(c.Serial))
			m.Opt("karta_bridge_manifest_expires_timestamp_seconds", "When the published manifest expires.", nil, ts(&c.ExpiresAt))
			m.Opt("karta_bridge_manifest_data_timestamp_seconds", "Data timestamp the published manifest signs.", nil, ts(&c.DataTimestamp))
		}
		m.Gauge("karta_bridge_held", "1 if a newer download is held (not signed) for inspection.", nil, promtext.Bool(s.Held != nil))
		code := ""
		if s.LastError != nil {
			code = s.LastError.Code
		}
		m.Gauge("karta_bridge_sign_last_error", "1 with the code of the signer's last failure, if its last run failed.", map[string]string{"code": code},
			promtext.Bool(s.LastError != nil))
	}
	return b.Bytes()
}
