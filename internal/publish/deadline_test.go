package publish

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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

func TestStoppedStagingIsInterruptedNotFailed(t *testing.T) {
	for _, cause := range []error{context.Canceled, ErrPublicationTimeout} {
		out := stageOutcome(7, fmt.Errorf("staging a.osm.pbf stopped: %w", cause))
		if out.State != registry.SubInterrupted || out.Code != CodeInterrupted {
			t.Errorf("%v: %+v", cause, out)
		}
	}
	// At the deadline, timeoutOutcome makes it a final publication_timeout.
	s := &Service{cfg: Config{PublishTimeout: time.Millisecond}}
	job, cancel := s.jobContext(context.Background())
	defer cancel()
	<-job.Done()
	out := s.timeoutOutcome(job, context.Background(), "staging", "touch the ready marker to submit again",
		stageOutcome(7, fmt.Errorf("staging a.osm.pbf stopped: %w", context.Cause(job))))
	if out.State != registry.SubFailed || out.Code != CodePublicationTimeout || !strings.Contains(out.Reason, "while staging") {
		t.Errorf("staging stopped at the deadline: %+v", out)
	}
	// An input fault while staging stays what it is.
	if out := stageOutcome(7, errors.New("disk")); out.State != registry.SubFailed {
		t.Errorf("an I/O failure: %+v", out)
	}
}

// blackholeDB accepts PostgreSQL connections and never answers: every query
// through it blocks until its context ends, like one stuck on a lock.
func blackholeDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	cfg, err := pgxpool.ParseConfig(fmt.Sprintf("host=127.0.0.1 port=%d user=karta dbname=karta sslmode=disable connect_timeout=60",
		ln.Addr().(*net.TCPAddr).Port))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return pool
}

func TestCapacityQueryCrossingTheDeadlineIsATimeout(t *testing.T) {
	s := &Service{cfg: Config{PublishTimeout: 300 * time.Millisecond}, reg: blackholeDB(t)}
	parent := context.Background()
	job, cancel := s.jobContext(parent)
	defer cancel()
	start := time.Now()
	err := s.checkCapacity(job, 1<<20, "fixture")
	took := time.Since(start)
	if err == nil || took < 250*time.Millisecond || took > 10*time.Second {
		t.Fatalf("capacity query: %v after %s, want it blocked until the 300ms deadline", err, took)
	}
	state, code := capacityFailure(err)
	if state != registry.SubInterrupted || code != CodeInterrupted {
		t.Fatalf("a capacity query stopped by the deadline is %s/%s, want interrupted (never insufficient_storage)", state, code)
	}
	out := s.timeoutOutcome(job, parent, "capacity", "touch the ready marker to submit again",
		Outcome{State: state, Code: code, Reason: err.Error(), Err: err})
	if out.State != registry.SubFailed || out.Code != CodePublicationTimeout || !errors.Is(out.Err, ErrPublicationTimeout) ||
		!strings.Contains(out.Reason, "while capacity") {
		t.Errorf("outcome %+v, want failed/publication_timeout naming the capacity phase", out)
	}
}

func TestCapacityFailure(t *testing.T) {
	storage := fmt.Errorf("%w: release databases use 10 bytes and the candidate needs about 20, above the budget of 15 bytes", ErrStorage)
	for _, c := range []struct {
		name  string
		err   error
		state string
		code  string
	}{
		{"a refusal for space", storage, registry.SubRejected, CodeInsufficient},
		{"a query cancelled by the deadline", fmt.Errorf("timeout: %w", context.DeadlineExceeded), registry.SubInterrupted, CodeInterrupted},
		{"a query cancelled by a shutdown", fmt.Errorf("query: %w", context.Canceled), registry.SubInterrupted, CodeInterrupted},
		{"the database failed", errors.New("connection reset by peer"), registry.SubInterrupted, CodeInterrupted},
	} {
		if state, code := capacityFailure(c.err); state != c.state || code != c.code {
			t.Errorf("%s: %s/%s, want %s/%s", c.name, state, code, c.state, c.code)
		}
	}
	// A refusal for space decided before the deadline stays a refusal.
	s := &Service{cfg: Config{PublishTimeout: time.Millisecond}}
	job, cancel := s.jobContext(context.Background())
	defer cancel()
	<-job.Done()
	state, code := capacityFailure(storage)
	out := s.timeoutOutcome(job, context.Background(), "capacity", "", Outcome{State: state, Code: code, Reason: storage.Error()})
	if out.State != registry.SubRejected || out.Code != CodeInsufficient {
		t.Errorf("a storage refusal that completed before the deadline became %s/%s", out.State, out.Code)
	}
}
