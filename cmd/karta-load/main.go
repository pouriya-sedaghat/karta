// Command karta-load drives map and search load against a running Karta
// API and reports throughput, latency percentiles and errors per kind of
// request, as JSON. It reads the manifest first and pins every request to
// the release it names (like a map client), unless -follow is set.
//
//	karta-load -base http://127.0.0.1:8080 -duration 60s -concurrency 16
//	karta-load -base ... -rate 200 -duration 5m      open loop: a fixed request rate
//
// Closed loop (the default) measures the throughput the server sustains with
// -concurrency clients that send their next request as soon as the last one
// is answered. Open loop (-rate) offers a fixed rate; requests that cannot
// start on time because every worker is busy are counted as "late" (the
// server is saturated at that rate). Tiles are spread over the release's
// box at -min-zoom..-max-zoom; searches use -queries.
//
// It is a measuring tool for docs/operations.md ("Capacity"): its numbers
// describe one run on one host and data set, not a service objective.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type options struct {
	base        string
	duration    time.Duration
	concurrency int
	rate        float64
	mix         map[string]int
	minZoom     int
	maxZoom     int
	queries     []string
	follow      bool
	timeout     time.Duration
	seed        uint64
}

type manifest struct {
	Release struct {
		ReleaseID string `json:"release_id"`
	} `json:"release"`
	StyleURL string `json:"style_url"`
	Tiles    struct {
		URLTemplate string     `json:"url_template"`
		MinZoom     int        `json:"minzoom"`
		MaxZoom     int        `json:"maxzoom"`
		Bounds      [4]float64 `json:"bounds"`
	} `json:"tiles"`
	Search struct {
		URL string `json:"url"`
	} `json:"search"`
}

// pathFrom keeps the part of an absolute URL the API issued from "/v1/", to
// send it to the base under test (which may differ from the public URL).
func pathFrom(u string) (string, error) {
	i := strings.Index(u, "/v1/")
	if i < 0 {
		return "", fmt.Errorf("unexpected URL %q", u)
	}
	return u[i:], nil
}

// tileXY returns the tile containing lon, lat at zoom z (Web Mercator).
func tileXY(lon, lat float64, z int) (int, int) {
	n := math.Exp2(float64(z))
	x := int(math.Floor((lon + 180) / 360 * n))
	r := lat * math.Pi / 180
	y := int(math.Floor((1 - math.Log(math.Tan(r)+1/math.Cos(r))/math.Pi) / 2 * n))
	clamp := func(v int) int { return max(0, min(int(n)-1, v)) }
	return clamp(x), clamp(y)
}

func parseMix(s string) (map[string]int, error) {
	m := map[string]int{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		n, err := strconv.Atoi(v)
		if !ok || err != nil || n < 0 {
			return nil, fmt.Errorf("mix entry %q: want kind=weight", part)
		}
		switch k {
		case "tile", "search", "manifest", "style":
		default:
			return nil, fmt.Errorf("mix kind %q: want tile, search, manifest or style", k)
		}
		m[k] = n
	}
	total := 0
	for _, n := range m {
		total += n
	}
	if total == 0 {
		return nil, errors.New("the mix has no weight")
	}
	return m, nil
}

// target builds one request path per call.
type target struct {
	release  string
	tile     string // path template with {z}/{x}/{y}
	search   string
	style    string
	bounds   [4]float64
	minZoom  int
	maxZoom  int
	queries  []string
	kinds    []string
	weights  []int
	totalW   int
	follow   bool
	pinParam string
}

func newTarget(m manifest, o options) (*target, error) {
	tile, err := pathFrom(m.Tiles.URLTemplate)
	if err != nil {
		return nil, err
	}
	search, err := pathFrom(m.Search.URL)
	if err != nil {
		return nil, err
	}
	style, err := pathFrom(m.StyleURL)
	if err != nil {
		return nil, err
	}
	t := &target{release: m.Release.ReleaseID, tile: tile, search: search, style: style, bounds: m.Tiles.Bounds,
		minZoom: max(o.minZoom, m.Tiles.MinZoom), maxZoom: min(o.maxZoom, m.Tiles.MaxZoom), queries: o.queries, follow: o.follow}
	if t.minZoom > t.maxZoom {
		return nil, fmt.Errorf("zoom range %d..%d is outside the release's %d..%d", o.minZoom, o.maxZoom, m.Tiles.MinZoom, m.Tiles.MaxZoom)
	}
	kinds := make([]string, 0, len(o.mix))
	for k := range o.mix {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		t.kinds = append(t.kinds, k)
		t.weights = append(t.weights, o.mix[k])
		t.totalW += o.mix[k]
	}
	if !o.follow {
		t.pinParam = "&release_id=" + url.QueryEscape(t.release)
	}
	return t, nil
}

func (t *target) next(r *rand.Rand) (kind, path string) {
	w := r.IntN(t.totalW)
	for i, k := range t.kinds {
		if w < t.weights[i] {
			kind = k
			break
		}
		w -= t.weights[i]
	}
	switch kind {
	case "tile":
		z := t.minZoom + r.IntN(t.maxZoom-t.minZoom+1)
		lon := t.bounds[0] + r.Float64()*(t.bounds[2]-t.bounds[0])
		lat := t.bounds[1] + r.Float64()*(t.bounds[3]-t.bounds[1])
		x, y := tileXY(lon, lat, z)
		p := strings.NewReplacer("{z}", strconv.Itoa(z), "{x}", strconv.Itoa(x), "{y}", strconv.Itoa(y)).Replace(t.tile)
		return kind, p
	case "search":
		q := t.queries[r.IntN(len(t.queries))]
		return kind, t.search + "?q=" + url.QueryEscape(q) + "&limit=10" + t.pinParam
	case "style":
		return kind, t.style
	}
	return "manifest", "/v1/manifest"
}

// recorder collects outcomes per kind.
type recorder struct {
	mu       sync.Mutex
	latency  map[string][]time.Duration
	statuses map[string]map[string]int
	errors   map[string]int
	samples  []string
	late     int
	releases map[string]bool
}

func newRecorder() *recorder {
	return &recorder{latency: map[string][]time.Duration{}, statuses: map[string]map[string]int{}, errors: map[string]int{}, releases: map[string]bool{}}
}

func (r *recorder) add(kind string, status int, d time.Duration, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latency[kind] = append(r.latency[kind], d)
	if r.statuses[kind] == nil {
		r.statuses[kind] = map[string]int{}
	}
	key := strconv.Itoa(status)
	if err != nil {
		key = "transport_error"
	}
	r.statuses[kind][key]++
	if err != nil || status >= 400 {
		r.errors[kind]++
		if len(r.samples) < 20 {
			msg := key
			if err != nil {
				msg += ": " + err.Error()
			}
			r.samples = append(r.samples, kind+" "+msg)
		}
	}
}

// KindSummary describes one kind of request.
type KindSummary struct {
	Requests int            `json:"requests"`
	Errors   int            `json:"errors"`
	Statuses map[string]int `json:"statuses"`
	P50ms    float64        `json:"p50_ms"`
	P90ms    float64        `json:"p90_ms"`
	P95ms    float64        `json:"p95_ms"`
	P99ms    float64        `json:"p99_ms"`
	MaxMs    float64        `json:"max_ms"`
}

// Summary is the report of one run.
type Summary struct {
	Base           string                 `json:"base"`
	ReleaseID      string                 `json:"release_id"`
	Pinned         bool                   `json:"pinned"`
	Mode           string                 `json:"mode"`
	Concurrency    int                    `json:"concurrency"`
	TargetRate     float64                `json:"target_rate_per_second,omitempty"`
	Seconds        float64                `json:"seconds"`
	Requests       int                    `json:"requests"`
	Errors         int                    `json:"errors"`
	Late           int                    `json:"late,omitempty"`
	RatePerSecond  float64                `json:"achieved_rate_per_second"`
	ErrorRatio     float64                `json:"error_ratio"`
	Kinds          map[string]KindSummary `json:"kinds"`
	ErrorSamples   []string               `json:"error_samples"`
	MinZoom        int                    `json:"min_zoom"`
	MaxZoom        int                    `json:"max_zoom"`
	ReleasesAnswer []string               `json:"releases_answered,omitempty"`
	StartedAt      time.Time              `json:"started_at"`
}

func percentile(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return float64(sorted[int(math.Ceil(p*float64(len(sorted))))-1].Microseconds()) / 1000
}

func (r *recorder) summary(s *Summary) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s.Kinds = map[string]KindSummary{}
	for k, lat := range r.latency {
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		ks := KindSummary{Requests: len(lat), Errors: r.errors[k], Statuses: r.statuses[k],
			P50ms: percentile(lat, 0.5), P90ms: percentile(lat, 0.9), P95ms: percentile(lat, 0.95), P99ms: percentile(lat, 0.99), MaxMs: percentile(lat, 1)}
		s.Kinds[k] = ks
		s.Requests += ks.Requests
		s.Errors += ks.Errors
	}
	s.ErrorSamples = append([]string{}, r.samples...)
	s.Late = r.late
	if s.Seconds > 0 {
		s.RatePerSecond = math.Round(float64(s.Requests)/s.Seconds*10) / 10
	}
	if s.Requests > 0 {
		s.ErrorRatio = float64(s.Errors) / float64(s.Requests)
	}
	for id := range r.releases {
		s.ReleasesAnswer = append(s.ReleasesAnswer, id)
	}
	sort.Strings(s.ReleasesAnswer)
}

func getManifest(ctx context.Context, c *http.Client, base string) (manifest, error) {
	var m manifest
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/manifest", nil)
	resp, err := c.Do(req)
	if err != nil {
		return m, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return m, fmt.Errorf("manifest: HTTP %d", resp.StatusCode)
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m)
	return m, err
}

// run drives the load until ctx ends and returns the summary.
func run(ctx context.Context, o options) (Summary, error) {
	client := &http.Client{Timeout: o.timeout, Transport: &http.Transport{MaxIdleConnsPerHost: o.concurrency * 2, MaxConnsPerHost: o.concurrency * 2},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	m, err := getManifest(ctx, client, o.base)
	if err != nil {
		return Summary{}, err
	}
	tg, err := newTarget(m, o)
	if err != nil {
		return Summary{}, err
	}
	s := Summary{Base: o.base, ReleaseID: m.Release.ReleaseID, Pinned: !o.follow, Concurrency: o.concurrency,
		MinZoom: tg.minZoom, MaxZoom: tg.maxZoom, StartedAt: time.Now().UTC(), Mode: "closed_loop"}
	rec := newRecorder()
	ctx, cancel := context.WithTimeout(ctx, o.duration)
	defer cancel()
	var tokens chan struct{}
	if o.rate > 0 {
		s.Mode, s.TargetRate = "open_loop", o.rate
		tokens = make(chan struct{}, o.concurrency)
		go func() {
			interval := time.Duration(float64(time.Second) / o.rate)
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					select {
					case tokens <- struct{}{}:
					default:
						rec.mu.Lock()
						rec.late++
						rec.mu.Unlock()
					}
				}
			}
		}()
	}
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < o.concurrency; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(o.seed, uint64(w)))
			for {
				if tokens != nil {
					select {
					case <-ctx.Done():
						return
					case <-tokens:
					}
				} else if ctx.Err() != nil {
					return
				}
				kind, path := tg.next(r)
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, o.base+path, nil)
				t0 := time.Now()
				resp, err := client.Do(req)
				if ctx.Err() != nil {
					return // the run ended during this request: not counted
				}
				status := 0
				if err == nil {
					status = resp.StatusCode
					var body struct {
						ReleaseID string `json:"release_id"`
					}
					if kind == "search" && status == http.StatusOK {
						_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if body.ReleaseID != "" {
						rec.mu.Lock()
						rec.releases[body.ReleaseID] = true
						rec.mu.Unlock()
					}
				}
				rec.add(kind, status, time.Since(t0), err)
			}
		}(w)
	}
	wg.Wait()
	s.Seconds = math.Round(time.Since(start).Seconds()*100) / 100
	rec.summary(&s)
	return s, nil
}

func main() {
	var o options
	var mix, queries, out string
	flag.StringVar(&o.base, "base", "http://127.0.0.1:8080", "base URL of the API under test (with any path prefix)")
	flag.DurationVar(&o.duration, "duration", time.Minute, "how long to run")
	flag.IntVar(&o.concurrency, "concurrency", 8, "concurrent clients (closed loop) or workers (open loop)")
	flag.Float64Var(&o.rate, "rate", 0, "requests per second to offer (open loop); 0 = closed loop")
	flag.StringVar(&mix, "mix", "tile=70,search=20,manifest=5,style=5", "request mix by weight")
	flag.IntVar(&o.minZoom, "min-zoom", 10, "lowest tile zoom requested")
	flag.IntVar(&o.maxZoom, "max-zoom", 16, "highest tile zoom requested")
	flag.StringVar(&queries, "queries", "دریاچه,پارک,خیابان,بیمارستان,مدرسه,Lake,Park,Mall", "comma-separated search queries")
	flag.BoolVar(&o.follow, "follow", false, "do not pin requests to the release the manifest named at the start")
	flag.DurationVar(&o.timeout, "timeout", 15*time.Second, "per-request timeout")
	flag.Uint64Var(&o.seed, "seed", 1, "random seed (runs are repeatable for the same seed and data)")
	flag.StringVar(&out, "out", "", "also write the JSON summary to this file")
	flag.Parse()
	var err error
	if o.mix, err = parseMix(mix); err != nil || o.concurrency < 1 || o.duration <= 0 || o.rate < 0 {
		fmt.Fprintln(os.Stderr, "karta-load: invalid flags:", err)
		os.Exit(2)
	}
	for _, q := range strings.Split(queries, ",") {
		if q = strings.TrimSpace(q); q != "" {
			o.queries = append(o.queries, q)
		}
	}
	if len(o.queries) == 0 && o.mix["search"] > 0 {
		fmt.Fprintln(os.Stderr, "karta-load: -queries is empty")
		os.Exit(2)
	}
	o.base = strings.TrimRight(o.base, "/")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	s, err := run(ctx, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "karta-load:", err)
		os.Exit(1)
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	fmt.Println(string(b))
	if out != "" {
		if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil { // #nosec G306 -- a measurement report
			fmt.Fprintln(os.Stderr, "karta-load:", err)
			os.Exit(1)
		}
	}
}
