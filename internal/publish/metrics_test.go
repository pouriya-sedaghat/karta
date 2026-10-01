package publish

import (
	"bytes"
	"testing"
)

func TestMetricWriter(t *testing.T) {
	var b bytes.Buffer
	m := metricWriter{&b}
	m.gauge("karta_x", "Help.", map[string]string{"z": `a"b\c`, "a": "line\nbreak"}, 1.5)
	m.opt("karta_absent", "Not written.", nil, nil)
	v := 3.0
	m.opt("karta_present", "Written.", nil, &v)
	m.sample("karta_multi", "", map[string]string{"k": "1"}, 0, false)
	want := "# HELP karta_x Help.\n# TYPE karta_x gauge\n" + `karta_x{a="line\nbreak",z="a\"b\\c"} 1.5` + "\n" +
		"# HELP karta_present Written.\n# TYPE karta_present gauge\nkarta_present 3\n" + `karta_multi{k="1"} 0` + "\n"
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}
