package release

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pouriya-sedaghat/karta/internal/registry"
)

// fakeWorld drives a Manager without a database: the registry rows, which
// releases load, which pools are open, a fake clock and the drain timers.
type fakeWorld struct {
	mu      sync.Mutex
	now     time.Time
	active  string
	rows    map[string]registry.ServedRelease
	regErr  error
	loadErr map[string]error
	loads   map[string]int
	open    map[*pgxpool.Pool]string
	closed  []string
	timers  []timer
}

type timer struct {
	at time.Time
	f  func()
}

func newWorld() *fakeWorld {
	return &fakeWorld{now: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC), rows: map[string]registry.ServedRelease{},
		loadErr: map[string]error{}, loads: map[string]int{}, open: map[*pgxpool.Pool]string{}}
}

func (w *fakeWorld) manager() *Manager {
	m := NewManager(dbParamsForTest(), "karta_registry", nil, 15*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.now = func() time.Time { w.mu.Lock(); defer w.mu.Unlock(); return w.now }
	m.serving = func(context.Context) (*registry.ServedRelease, []registry.ServedRelease, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.regErr != nil {
			return nil, nil, w.regErr
		}
		var all []registry.ServedRelease
		for _, r := range w.rows {
			all = append(all, r)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
		var act *registry.ServedRelease
		if r, ok := w.rows[w.active]; ok {
			act = &r
		}
		return act, all, nil
	}
	m.load = func(_ context.Context, ref registry.ServedRelease) (*Release, string, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.loads[ref.ID]++
		if err := w.loadErr[ref.ID]; err != nil {
			return nil, ReasonIncompatible, err
		}
		// A distinct, never-used pool value stands for each opened pool.
		rel := &Release{Info: Info{ReleaseID: ref.ID}, Pool: new(pgxpool.Pool)}
		w.open[rel.Pool] = ref.ID
		return rel, ReasonReady, nil
	}
	m.ping = func(context.Context, *Release) error { return nil }
	m.close = func(rel *Release) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if _, ok := w.open[rel.Pool]; !ok {
			panic("closing a pool that is not open (or twice): " + rel.Info.ReleaseID)
		}
		delete(w.open, rel.Pool)
		w.closed = append(w.closed, rel.Info.ReleaseID)
	}
	m.after = func(d time.Duration, f func()) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.timers = append(w.timers, timer{at: w.now.Add(d), f: f})
	}
	return m
}

// advance moves the clock and fires due drain timers.
func (w *fakeWorld) advance(d time.Duration) {
	w.mu.Lock()
	w.now = w.now.Add(d)
	var due []func()
	var rest []timer
	for _, t := range w.timers {
		if !t.at.After(w.now) {
			due = append(due, t.f)
		} else {
			rest = append(rest, t)
		}
	}
	w.timers = rest
	w.mu.Unlock()
	for _, f := range due {
		f()
	}
}

// activate is what registry.Activate does to the rows.
func (w *fakeWorld) activate(id string, grace time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if old, ok := w.rows[w.active]; ok {
		until := w.now.Add(grace)
		old.State, old.PinnedUntil = registry.StateRetired, &until
		w.rows[old.ID] = old
	}
	at := w.now
	w.rows[id] = registry.ServedRelease{ID: id, Database: "karta_" + id, State: registry.StateActive, ActivatedAt: &at}
	w.active = id
}

func (w *fakeWorld) setState(id, state string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.rows[id]
	r.State = state
	w.rows[id] = r
}

func (w *fakeWorld) openCount(id string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, r := range w.open {
		if r == id {
			n++
		}
	}
	return n
}

const (
	relA = "raaaaaaaaaaaaaaaaaaaaaaaa"
	relB = "rbbbbbbbbbbbbbbbbbbbbbbbb"
	relC = "rcccccccccccccccccccccccc"
)

func expectLookup(t *testing.T, m *Manager, id string, want Lookup) *Release {
	t.Helper()
	rel, got := m.Lookup(id)
	if got != want || (want == Served) != (rel != nil) {
		t.Fatalf("Lookup(%s) = %v, %v; want %v", id, rel, got, want)
	}
	return rel
}

// The Stage 1 design gate: switching the active release keeps the previous
// release resolvable, on the same pool, until its pin grace ends; the pool is
// closed only a drain period after it stops being served, never while a
// request that resolved it may still run.
func TestSwitchKeepsPreviousReleaseUntilPinExpiry(t *testing.T) {
	w := newWorld()
	m := w.manager()
	w.activate(relA, time.Minute)
	m.Refresh(context.Background())
	a := expectLookup(t, m, relA, Served)
	if m.Active() != a || !m.Status().Ready {
		t.Fatalf("active %v status %+v", m.Active(), m.Status())
	}

	// A request resolves A just before the switch and keeps using it.
	inFlight := m.Active()
	w.activate(relB, time.Minute)
	m.Refresh(context.Background())
	if m.Active().Info.ReleaseID != relB {
		t.Fatalf("active %s after switch", m.Active().Info.ReleaseID)
	}
	pinnedA := expectLookup(t, m, relA, Served)
	if pinnedA.Pool != inFlight.Pool || pinnedA.PinnedUntil == nil || w.openCount(relA) != 1 {
		t.Fatalf("pinned A %+v, open pools %d", pinnedA, w.openCount(relA))
	}
	if w.loads[relA] != 1 {
		t.Fatalf("A loaded %d times; a switch must not reload the previous release", w.loads[relA])
	}

	// During the grace period, refreshes keep serving A.
	w.advance(59 * time.Second)
	m.Refresh(context.Background())
	expectLookup(t, m, relA, Served)

	// The grace ends: Lookup reports Expired immediately, even before the
	// next refresh, but the pool stays open for in-flight requests.
	w.advance(time.Second)
	expectLookup(t, m, relA, Expired)
	m.Refresh(context.Background())
	expectLookup(t, m, relA, Expired)
	if w.openCount(relA) != 1 {
		t.Fatal("A's pool closed without a drain period")
	}
	w.advance(14 * time.Second)
	if w.openCount(relA) != 1 {
		t.Fatal("A's pool closed before the drain period ended")
	}
	w.advance(time.Second)
	if w.openCount(relA) != 0 || len(w.closed) != 1 || w.closed[0] != relA {
		t.Fatalf("A's pool not closed after the drain: open %d closed %v", w.openCount(relA), w.closed)
	}
	expectLookup(t, m, relB, Served)
	expectLookup(t, m, relC, Unknown)
}

// Rollback to a retired release that is still pinned reuses its pool; the
// release it replaced is pinned in turn.
func TestRollbackWithinGraceReusesThePool(t *testing.T) {
	w := newWorld()
	m := w.manager()
	w.activate(relA, time.Minute)
	m.Refresh(context.Background())
	a := m.Active()
	w.activate(relB, time.Minute)
	m.Refresh(context.Background())
	w.advance(10 * time.Second)
	w.activate(relA, time.Minute) // rollback
	m.Refresh(context.Background())
	if m.Active().Pool != a.Pool || m.Active().PinnedUntil != nil || w.loads[relA] != 1 {
		t.Fatalf("rollback reloaded A or kept its pin: %+v loads %d", m.Active(), w.loads[relA])
	}
	if b := expectLookup(t, m, relB, Served); b.PinnedUntil == nil {
		t.Fatal("B not pinned after the rollback")
	}
	if len(w.closed) != 0 {
		t.Fatalf("pools closed: %v", w.closed)
	}
}

// Rollback to a release whose pin expired (but that is retained) loads it
// again on a new pool.
func TestRollbackAfterExpiryLoadsAgain(t *testing.T) {
	w := newWorld()
	m := w.manager()
	w.activate(relA, time.Minute)
	m.Refresh(context.Background())
	w.activate(relB, time.Minute)
	m.Refresh(context.Background())
	w.advance(2 * time.Minute)
	m.Refresh(context.Background())
	w.advance(time.Minute)
	expectLookup(t, m, relA, Expired)
	w.activate(relA, time.Minute)
	m.Refresh(context.Background())
	if m.Active().Info.ReleaseID != relA || w.loads[relA] != 2 || w.openCount(relA) != 1 {
		t.Fatalf("active %s loads %d open %d", m.Active().Info.ReleaseID, w.loads[relA], w.openCount(relA))
	}
}

// Removed and removing releases are Expired; a pinned release that cannot
// be loaded is Unavailable and does not affect readiness.
func TestRemovedAndUnloadablePins(t *testing.T) {
	w := newWorld()
	m := w.manager()
	w.activate(relA, time.Minute)
	w.activate(relB, time.Minute)
	w.loadErr[relA] = errors.New("toolchain differs")
	m.Refresh(context.Background())
	expectLookup(t, m, relA, Unavailable)
	if !m.Status().Ready || m.Status().ReleaseID != relB {
		t.Fatalf("status %+v", m.Status())
	}
	w.setState(relA, registry.StateRemoved)
	m.Refresh(context.Background())
	expectLookup(t, m, relA, Expired)
}

// A registry outage keeps serving the loaded releases, but pins still
// expire on time.
func TestRegistryOutageKeepsServingAndExpiresPins(t *testing.T) {
	w := newWorld()
	m := w.manager()
	w.activate(relA, time.Minute)
	w.activate(relB, time.Minute)
	m.Refresh(context.Background())
	expectLookup(t, m, relA, Served)
	w.regErr = errors.New("connection refused")
	w.advance(30 * time.Second)
	m.Refresh(context.Background())
	expectLookup(t, m, relA, Served)
	if !m.Status().Ready || m.Status().ReleaseID != relB {
		t.Fatalf("status during outage %+v", m.Status())
	}
	w.advance(31 * time.Second)
	m.Refresh(context.Background())
	expectLookup(t, m, relA, Expired)
	w.advance(20 * time.Second)
	if w.openCount(relA) != 0 {
		t.Fatal("expired pin's pool not closed during the outage")
	}
	expectLookup(t, m, relB, Served)
}

// An active release that cannot be loaded makes the server not ready, with
// the reason; nothing else is served in its place.
func TestIncompatibleActiveRelease(t *testing.T) {
	w := newWorld()
	m := w.manager()
	w.activate(relA, time.Minute)
	w.loadErr[relA] = errors.New("serving database toolchain differs")
	m.Refresh(context.Background())
	if st := m.Status(); st.Ready || st.Reason != ReasonIncompatible || m.Active() != nil {
		t.Fatalf("status %+v active %v", st, m.Active())
	}
	expectLookup(t, m, relA, Unavailable)
}
