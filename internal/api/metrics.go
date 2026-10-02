package api

import (
	"bytes"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/promtext"
)

// requestBuckets are the latency histogram's upper bounds in seconds.
var requestBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Metrics counts the public API's requests for monitoring: by route (the
// matched pattern, never the path or query, so labels stay few and carry no
// search terms or release ids) and status class. It is served on a separate
// listener (see cmd/karta), never on the public one.
type Metrics struct {
	mu       sync.Mutex
	requests map[[2]string]uint64 // route, status class
	duration map[string]*promtext.Histogram
	inFlight atomic.Int64
	started  time.Time
}

// NewMetrics returns empty request metrics.
func NewMetrics() *Metrics {
	return &Metrics{requests: map[[2]string]uint64{}, duration: map[string]*promtext.Histogram{}, started: time.Now()}
}

// routes names the handler patterns (internal/api/server.go).
var routes = map[string]string{
	"GET /health/live":                                "health_live",
	"GET /health/ready":                               "health_ready",
	"GET /v1/manifest":                                "manifest",
	"GET /v1/search":                                  "search",
	"GET /v1/releases/{release_id}/style.json":        "style_redirect",
	"GET /v1/releases/{release_id}/styles/{style}":    "style",
	"GET /v1/releases/{release_id}/tiles/{z}/{x}/{y}": "tile",
	"GET /v1/fonts/{fontstack}/{range}":               "font",
	"GET /v1/openapi.yaml":                            "openapi",
	"GET /demo/":                                      "demo",
	"GET /{$}":                                        "demo",
	"GET /demo":                                       "demo",
	"/":                                               "not_found",
	"":                                                "refused", // answered by the middleware (method, body, CORS preflight)
}

func routeName(pattern string) string {
	if n, ok := routes[pattern]; ok {
		return n
	}
	return "other"
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	}
	return "2xx"
}

func (m *Metrics) observe(pattern string, status int, d time.Duration) {
	if status == 0 {
		status = http.StatusOK
	}
	route := routeName(pattern)
	m.mu.Lock()
	m.requests[[2]string{route, statusClass(status)}]++
	h := m.duration[route]
	if h == nil {
		h = promtext.NewHistogram(requestBuckets...)
		m.duration[route] = h
	}
	m.mu.Unlock()
	h.Observe(d.Seconds())
}

// Handler serves the metrics in the Prometheus text format: requests and
// their latency by route, requests in flight, readiness and the release
// served, and the process's goroutines and heap. The caller authenticates.
func (m *Metrics) Handler(releases Releases) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		p := promtext.New(&b)
		st := releases.Status()
		p.Gauge("karta_api_ready", "1 if the API is ready: a compatible release is loaded and the database is reachable.", nil, promtext.Bool(st.Ready))
		p.Gauge("karta_api_readiness_info", "The readiness reason (value 1).", map[string]string{"reason": st.Reason}, 1)
		if st.ReleaseID != "" {
			p.Gauge("karta_api_release_info", "The active release this API serves (value 1).", map[string]string{"release_id": st.ReleaseID}, 1)
		}
		m.mu.Lock()
		keys := make([][2]string, 0, len(m.requests))
		for k := range m.requests {
			keys = append(keys, k)
		}
		counts := make(map[[2]string]uint64, len(keys))
		for _, k := range keys {
			counts[k] = m.requests[k]
		}
		hists := make(map[string]promtext.Snapshot, len(m.duration))
		for k, h := range m.duration {
			hists[k] = h.Snapshot()
		}
		m.mu.Unlock()
		sort.Slice(keys, func(i, j int) bool {
			if keys[i][0] != keys[j][0] {
				return keys[i][0] < keys[j][0]
			}
			return keys[i][1] < keys[j][1]
		})
		p.Header("karta_api_requests_total", "Requests answered, by route and status class.", "counter")
		for _, k := range keys {
			p.Sample("karta_api_requests_total", map[string]string{"route": k[0], "code": k[1]}, float64(counts[k]))
		}
		names := make([]string, 0, len(hists))
		for k := range hists {
			names = append(names, k)
		}
		sort.Strings(names)
		if len(names) > 0 {
			p.Header("karta_api_request_duration_seconds", "Time to answer a request, by route.", "histogram")
		}
		for _, k := range names {
			p.HistogramSamples("karta_api_request_duration_seconds", map[string]string{"route": k}, hists[k])
		}
		p.Gauge("karta_api_requests_in_flight", "Requests being answered.", nil, float64(m.inFlight.Load()))
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		p.Gauge("karta_api_heap_inuse_bytes", "Heap bytes in use by the API process.", nil, float64(ms.HeapInuse))
		p.Gauge("karta_api_goroutines", "Goroutines of the API process.", nil, float64(runtime.NumGoroutine()))
		p.Gauge("karta_api_start_time_seconds", "When this API process started (Unix time).", nil, float64(m.started.UnixMilli())/1000)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b.Bytes())
	})
}
