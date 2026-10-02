package promtext

import (
	"bytes"
	"math"
	"testing"
)

func TestWriter(t *testing.T) {
	var b bytes.Buffer
	w := New(&b)
	w.Gauge("karta_x", "Help.", map[string]string{"z": `a"b\c`, "a": "line\nbreak"}, 1.5)
	w.Opt("karta_absent", "Not written.", nil, nil)
	v := 3.0
	w.Opt("karta_present", "Written.", nil, &v)
	w.Header("karta_multi", "Several samples.", "gauge")
	w.Sample("karta_multi", map[string]string{"k": "1"}, 0)
	w.Sample("karta_multi", map[string]string{"k": "2"}, math.Inf(1))
	w.Counter("karta_total", "A counter.", nil, 7)
	want := "# HELP karta_x Help.\n# TYPE karta_x gauge\n" + `karta_x{a="line\nbreak",z="a\"b\\c"} 1.5` + "\n" +
		"# HELP karta_present Written.\n# TYPE karta_present gauge\nkarta_present 3\n" +
		"# HELP karta_multi Several samples.\n# TYPE karta_multi gauge\n" + `karta_multi{k="1"} 0` + "\n" + `karta_multi{k="2"} +Inf` + "\n" +
		"# HELP karta_total A counter.\n# TYPE karta_total counter\nkarta_total 7\n"
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}

func TestHistogram(t *testing.T) {
	h := NewHistogram(0.1, 1, 10)
	for _, v := range []float64{0.05, 0.1, 0.5, 1, 9, 50} {
		h.Observe(v)
	}
	var b bytes.Buffer
	w := New(&b)
	w.Header("karta_d_seconds", "Durations.", "histogram")
	w.HistogramSamples("karta_d_seconds", map[string]string{"route": "tile"}, h.Snapshot())
	want := "# HELP karta_d_seconds Durations.\n# TYPE karta_d_seconds histogram\n" +
		`karta_d_seconds_bucket{route="tile",le="0.1"} 2` + "\n" +
		`karta_d_seconds_bucket{route="tile",le="1"} 4` + "\n" +
		`karta_d_seconds_bucket{route="tile",le="10"} 5` + "\n" +
		`karta_d_seconds_bucket{route="tile",le="+Inf"} 6` + "\n" +
		`karta_d_seconds_sum{route="tile"} 60.65` + "\n" +
		`karta_d_seconds_count{route="tile"} 6` + "\n"
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}
