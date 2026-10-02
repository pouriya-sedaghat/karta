package publish

import (
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/registry"
)

func TestPublicationStats(t *testing.T) {
	var p pubStats
	p.record(SourceInbox, registry.SubPublished, "", 3*time.Second)
	p.record(SourceInbox, registry.SubPublished, "", 90*time.Second)
	p.record(SourceOnline, registry.SubFailed, CodePublicationTimeout, 2*time.Hour)
	p.record(SourceCLI, registry.SubRejected, "unauthorized_digest", 0)
	outcomes, durations, timeouts := p.snapshot()
	if outcomes[[2]string{SourceInbox, registry.SubPublished}] != 2 || outcomes[[2]string{SourceOnline, registry.SubFailed}] != 1 ||
		outcomes[[2]string{SourceCLI, registry.SubRejected}] != 1 || timeouts != 1 {
		t.Fatalf("outcomes %v timeouts %d", outcomes, timeouts)
	}
	in := durations[SourceInbox]
	if in.Count != 2 || in.Sum != 93 {
		t.Fatalf("inbox durations %+v", in)
	}
	// 3 s falls in the 5 s bucket, 90 s in the 120 s bucket.
	for i, b := range in.Bounds {
		want := uint64(0)
		if b == 5 || b == 120 {
			want = 1
		}
		if in.Counts[i] != want {
			t.Errorf("bucket le=%g: %d", b, in.Counts[i])
		}
	}
	// The snapshot is a copy.
	p.record(SourceInbox, registry.SubPublished, "", time.Second)
	if outcomes[[2]string{SourceInbox, registry.SubPublished}] != 2 || durations[SourceInbox].Count != 2 {
		t.Error("snapshot changed after a later record")
	}
}
