package publish

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/registry"
)

func TestTimeoutOutcome(t *testing.T) {
	s := &Service{cfg: Config{PublishTimeout: 50 * time.Millisecond, PointerLockTimeout: time.Second}}
	expired := func(parent context.Context) context.Context {
		job, cancel := s.jobContext(parent)
		t.Cleanup(cancel)
		<-job.Done()
		return job
	}
	parent := context.Background()
	job := expired(parent)
	if !timedOut(job, parent) {
		t.Fatal("an expired job is not reported as timed out")
	}

	for _, state := range []string{registry.SubInterrupted, registry.SubFailed} {
		out := s.timeoutOutcome(job, parent, "building", "touch the ready marker to submit again",
			Outcome{State: state, Code: CodeInterrupted, Reason: "osm2pgsql was stopped", ReleaseID: "r0123"})
		if out.State != registry.SubFailed || out.Code != CodePublicationTimeout || !errors.Is(out.Err, ErrPublicationTimeout) || out.ReleaseID != "r0123" {
			t.Fatalf("%s: %+v", state, out)
		}
		for _, want := range []string{"KARTA_PUBLISH_TIMEOUT (50ms)", "while building", "osm2pgsql was stopped", "not retried automatically", "touch the ready marker"} {
			if !strings.Contains(out.Reason, want) {
				t.Errorf("%s: reason lacks %q: %s", state, want, out.Reason)
			}
		}
	}
	// What the publication reached anyway is kept.
	for _, keep := range []Outcome{
		{State: registry.SubPublished, ReleaseID: "r1"},
		{State: registry.SubReady, Code: CodeManualActivation},
		{State: registry.SubRejected, Code: "unauthorized_digest"},
		{State: registry.SubDuplicate, Code: CodeDuplicateActive},
	} {
		if got := s.timeoutOutcome(job, parent, "building", "", keep); got.State != keep.State || got.Code != keep.Code {
			t.Errorf("%+v became %+v", keep, got)
		}
	}

	// A shutdown is not a timeout: the publication stays interrupted and is
	// retried at the next start.
	shut, stop := context.WithCancel(context.Background())
	sjob := expired(shut)
	stop()
	if got := s.timeoutOutcome(sjob, shut, "building", "", Outcome{State: registry.SubInterrupted, Code: CodeInterrupted}); got.Code != CodeInterrupted {
		t.Errorf("shutdown became %+v", got)
	}
	// A job cancelled for another reason, or still running, is unchanged.
	live, cancel := s.jobContext(context.Background())
	defer cancel()
	if got := s.timeoutOutcome(live, context.Background(), "building", "", Outcome{State: registry.SubFailed, Code: CodeBuildFailed}); got.Code != CodeBuildFailed {
		t.Errorf("running job: %+v", got)
	}
}

func TestSwitchContextIsDetachedAndBounded(t *testing.T) {
	s := &Service{cfg: Config{PublishTimeout: time.Millisecond, PointerLockTimeout: time.Second}}
	job, cancel := s.jobContext(context.Background())
	defer cancel()
	<-job.Done()
	sw, scancel := s.switchContext(job)
	defer scancel()
	if sw.Err() != nil {
		t.Fatal("the switch inherits the expired job deadline")
	}
	dl, ok := sw.Deadline()
	if !ok || time.Until(dl) > time.Second+switchBound || time.Until(dl) < switchBound {
		t.Fatalf("switch deadline %v", dl)
	}
}
