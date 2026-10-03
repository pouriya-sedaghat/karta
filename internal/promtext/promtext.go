// Package promtext writes the Prometheus text exposition format (version
// 0.0.4): gauges, counters and histograms with escaped, sorted labels.
// Karta renders its metrics when scraped, from its own state; it needs no
// client library.
package promtext

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Writer appends metrics to a buffer.
type Writer struct{ b *bytes.Buffer }

// New returns a writer appending to b.
func New(b *bytes.Buffer) Writer { return Writer{b} }

// Header writes the HELP and TYPE lines of a metric family; its samples
// follow with Sample.
func (w Writer) Header(name, help, typ string) {
	fmt.Fprintf(w.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// Sample writes one sample.
func (w Writer) Sample(name string, labels map[string]string, v float64) {
	w.b.WriteString(name)
	w.labels(labels, "", "")
	w.b.WriteString(" " + formatValue(v) + "\n")
}

func (w Writer) labels(labels map[string]string, extraKey, extraValue string) {
	if len(labels) == 0 && extraKey == "" {
		return
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		parts = append(parts, k+`="`+escape(labels[k])+`"`)
	}
	if extraKey != "" {
		parts = append(parts, extraKey+`="`+escape(extraValue)+`"`)
	}
	w.b.WriteString("{" + strings.Join(parts, ",") + "}")
}

// Gauge writes a one-sample gauge family.
func (w Writer) Gauge(name, help string, labels map[string]string, v float64) {
	w.Header(name, help, "gauge")
	w.Sample(name, labels, v)
}

// Opt writes a one-sample gauge family if v is set.
func (w Writer) Opt(name, help string, labels map[string]string, v *float64) {
	if v != nil {
		w.Gauge(name, help, labels, *v)
	}
}

// Counter writes a one-sample counter family.
func (w Writer) Counter(name, help string, labels map[string]string, v float64) {
	w.Header(name, help, "counter")
	w.Sample(name, labels, v)
}

// HistogramSamples writes the bucket, sum and count samples of one series
// of a histogram family (the caller writes the header once per family).
func (w Writer) HistogramSamples(name string, labels map[string]string, s Snapshot) {
	var cum uint64
	for i, bound := range s.Bounds {
		cum += s.Counts[i]
		w.b.WriteString(name + "_bucket")
		w.labels(labels, "le", formatValue(bound))
		w.b.WriteString(" " + strconv.FormatUint(cum, 10) + "\n")
	}
	w.b.WriteString(name + "_bucket")
	w.labels(labels, "le", "+Inf")
	w.b.WriteString(" " + strconv.FormatUint(s.Count, 10) + "\n")
	w.Sample(name+"_sum", labels, s.Sum)
	w.Sample(name+"_count", labels, float64(s.Count))
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func formatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Bool is 1 for true and 0 for false.
func Bool(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Histogram counts observations into fixed buckets; safe for concurrent use.
type Histogram struct {
	mu     sync.Mutex
	bounds []float64
	counts []uint64 // per bucket, not cumulative; the last is above every bound
	sum    float64
	count  uint64
}

// NewHistogram returns a histogram with the given ascending upper bounds.
func NewHistogram(bounds ...float64) *Histogram {
	if !sort.Float64sAreSorted(bounds) {
		panic("promtext: histogram bounds must ascend")
	}
	return &Histogram{bounds: bounds, counts: make([]uint64, len(bounds)+1)}
}

// Observe records one value.
func (h *Histogram) Observe(v float64) {
	i := sort.SearchFloat64s(h.bounds, v) // first bound >= v: "le" buckets
	h.mu.Lock()
	h.counts[i]++
	h.sum += v
	h.count++
	h.mu.Unlock()
}

// Snapshot is a consistent copy of a histogram.
type Snapshot struct {
	Bounds []float64
	Counts []uint64
	Sum    float64
	Count  uint64
}

// Snapshot copies the histogram's state.
func (h *Histogram) Snapshot() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return Snapshot{Bounds: h.bounds, Counts: append([]uint64(nil), h.counts...), Sum: h.sum, Count: h.count}
}
