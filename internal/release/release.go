// Package release loads servable releases for the API. Each release has its
// own read-only connection pool to its own database, and every request
// resolves exactly one release, either the explicit release_id or the
// active one, once, and uses it for all of its queries.
//
// The manager serves the active release and every retired release whose pin
// grace period has not ended (registry.releases.pinned_until, set by the
// publisher in the transaction that replaced it). When a release stops being
// servable it is first removed from lookup and its pool is closed only after
// a drain period longer than the request timeout, so requests that resolved
// it finish on it. Lookups distinguish a release that is served, one that
// was published but is no longer served (expired or removed, 410), one that
// should be served but cannot be loaded (503), and an unknown id (404).
//
// Before a release is served, and on every new connection to its database,
// the serving database's toolchain must equal the one recorded at import
// (package toolchain), so an immutable tile URL never serves bytes computed
// by a different PostgreSQL/PostGIS/GEOS/PROJ/ICU.
package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pouriya-sedaghat/karta/internal/dbconn"
	"github.com/pouriya-sedaghat/karta/internal/registry"
	"github.com/pouriya-sedaghat/karta/internal/releaseid"
	"github.com/pouriya-sedaghat/karta/internal/schema"
	"github.com/pouriya-sedaghat/karta/internal/style"
	"github.com/pouriya-sedaghat/karta/internal/toolchain"
)

// Readiness reasons.
const (
	ReasonReady          = "ready"
	ReasonStarting       = "starting"
	ReasonNoRelease      = "no_active_release"
	ReasonDBUnavailable  = "database_unavailable"
	ReasonIncompatible   = "release_incompatible"
	ReasonRegistryBroken = "registry_error"
)

// Info is the release metadata served in the manifest.
type Info struct {
	ReleaseID           string
	SchemaMajor         int
	SchemaRevision      string
	StyleRevision       string
	RegionID            string
	RegionName          string
	BBox                [4]float64
	Center              [2]float64
	Zoom                float64
	MinZoom             int
	MaxZoom             int
	SourceSHA256        string
	SourceSize          int64
	DataTimestamp       time.Time
	DataTimestampSource string
	ImportedAt          time.Time
	ImporterVersion     string
	Osm2pgsqlVersion    string
	Attribution         string
	License             string
	LicenseURL          string
	Toolchain           map[string]string
}

// Release is a loaded, validated release.
type Release struct {
	Info    Info
	Catalog schema.Catalog
	Style   json.RawMessage
	Pool    *pgxpool.Pool
	// ActivatedAt is when the release last became active (zero if unknown).
	ActivatedAt time.Time
	// PinnedUntil is set for a retired release: it is served to clients that
	// pinned it until then. Nil for the active release.
	PinnedUntil *time.Time
}

// Status is the readiness state.
type Status struct {
	Ready     bool
	Reason    string
	Detail    string
	ReleaseID string
	CheckedAt time.Time
}

// Lookup is the result of resolving an explicit release id.
type Lookup int

// Lookup results.
const (
	// Unknown: no published release has this id (404 unknown_release).
	Unknown Lookup = iota
	// Served: the release is served.
	Served
	// Expired: the release was published but is no longer served, because
	// its pin grace ended or it was removed (410 release_expired).
	Expired
	// Unavailable: the release should be served but cannot be loaded right
	// now (503).
	Unavailable
)

// Querier runs single-row queries.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Check reads a release's metadata from its database and verifies that this
// build can serve it there: the id matches, the schema major version is
// supported, the style uses only catalogued layers and served fonts, and the
// serving toolchain equals the recorded one. It returns the release without
// a pool, a readiness reason and the problem.
func Check(ctx context.Context, q Querier, expectID string, fontstacks []string) (*Release, string, error) {
	rel := &Release{}
	var layers, styleJSON, tools []byte
	var bbox, center []float64
	i := &rel.Info
	err := q.QueryRow(ctx, `
SELECT release_id, schema_major, schema_revision, style_revision, region_id, region_name, bbox, center,
       default_zoom, minzoom, maxzoom, source_sha256, source_size, data_timestamp, data_timestamp_source,
       imported_at, importer_version, osm2pgsql_version, attribution, license, license_url, layers, style, toolchain
FROM karta.release_info`).Scan(&i.ReleaseID, &i.SchemaMajor, &i.SchemaRevision, &i.StyleRevision, &i.RegionID,
		&i.RegionName, &bbox, &center, &i.Zoom, &i.MinZoom, &i.MaxZoom, &i.SourceSHA256, &i.SourceSize,
		&i.DataTimestamp, &i.DataTimestampSource, &i.ImportedAt, &i.ImporterVersion, &i.Osm2pgsqlVersion,
		&i.Attribution, &i.License, &i.LicenseURL, &layers, &styleJSON, &tools)
	if err != nil {
		if isConnError(err) {
			return nil, ReasonDBUnavailable, err
		}
		return nil, ReasonIncompatible, fmt.Errorf("read release metadata: %w", err)
	}
	problem := func(format string, args ...any) (*Release, string, error) {
		return nil, ReasonIncompatible, fmt.Errorf(format, args...)
	}
	if i.ReleaseID != expectID {
		return problem("database holds release %s, expected %s", i.ReleaseID, expectID)
	}
	if i.SchemaMajor != schema.Major {
		return problem("release schema major %d, this server supports %d", i.SchemaMajor, schema.Major)
	}
	if len(bbox) != 4 || len(center) != 2 {
		return problem("release bbox/center malformed")
	}
	copy(i.BBox[:], bbox)
	copy(i.Center[:], center)
	if err := json.Unmarshal(layers, &rel.Catalog); err != nil {
		return problem("release layer catalog: %v", err)
	}
	if err := json.Unmarshal(tools, &i.Toolchain); err != nil {
		return problem("release toolchain: %v", err)
	}
	rel.Style = styleJSON
	if err := style.Validate(rel.Style, rel.Catalog, fontstacks); err != nil {
		return problem("release style does not match its tile layers or the served fonts: %v", err)
	}
	running, err := toolchain.Read(ctx, q)
	if err != nil {
		if isConnError(err) {
			return nil, ReasonDBUnavailable, err
		}
		return problem("%v", err)
	}
	if err := toolchain.Compare(i.Toolchain, running); err != nil {
		return problem("%v", err)
	}
	return rel, ReasonReady, nil
}

// Manager keeps the servable releases loaded and reports readiness.
type Manager struct {
	db         dbconn.Params
	registryDB string
	fontstacks []string
	log        *slog.Logger
	// drain is how long a pool stays open after its release stopped being
	// servable; it must exceed the request timeout.
	drain time.Duration

	mu        sync.RWMutex
	active    *Release
	served    map[string]*Release
	known     map[string]Lookup
	status    Status
	freshness registry.Freshness
	registry  *pgxpool.Pool

	// Replaceable in tests.
	now     func() time.Time
	serving func(ctx context.Context) (*registry.ServedRelease, []registry.ServedRelease, error)
	fresh   func(ctx context.Context) (registry.Freshness, error)
	load    func(ctx context.Context, ref registry.ServedRelease) (*Release, string, error)
	ping    func(ctx context.Context, rel *Release) error
	close   func(rel *Release)
	after   func(d time.Duration, f func())
}

// NewManager creates a manager; call Run to start loading. drain must be
// longer than the longest request.
func NewManager(db dbconn.Params, registryDB string, fontstacks []string, drain time.Duration, log *slog.Logger) *Manager {
	m := &Manager{
		db: db, registryDB: registryDB, fontstacks: fontstacks, log: log, drain: drain,
		served: map[string]*Release{}, known: map[string]Lookup{},
		status:    Status{Reason: ReasonStarting, Detail: "release not loaded yet", CheckedAt: time.Now()},
		freshness: registry.Freshness{UpdateMode: "manual"},
		now:       time.Now,
		after:     func(d time.Duration, f func()) { time.AfterFunc(d, f) },
	}
	m.serving = m.readRegistry
	m.fresh = func(ctx context.Context) (registry.Freshness, error) {
		if m.registry == nil { // the registry was never reached (or a test replaced serving)
			return registry.Freshness{UpdateMode: "manual"}, nil
		}
		return registry.ReadFreshness(ctx, m.registry)
	}
	m.load = m.loadRelease
	m.ping = func(ctx context.Context, rel *Release) error { return rel.Pool.Ping(ctx) }
	m.close = func(rel *Release) {
		if rel.Pool != nil {
			rel.Pool.Close()
		}
	}
	return m
}

// Active returns the active release, or nil.
func (m *Manager) Active() *Release {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

// Lookup resolves an explicit release id.
func (m *Manager) Lookup(id string) (*Release, Lookup) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if rel, ok := m.served[id]; ok {
		if rel.PinnedUntil != nil && !m.now().Before(*rel.PinnedUntil) {
			return nil, Expired // the grace ended since the last refresh
		}
		return rel, Served
	}
	return nil, m.known[id]
}

// Freshness returns the update mode and staleness threshold last read from
// the registry (manual and none until read).
func (m *Manager) Freshness() registry.Freshness {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.freshness
}

// Status returns the last readiness state.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

// Run refreshes until ctx is cancelled. A running service keeps answering
// from the loaded releases even if the registry becomes unreachable.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	m.Refresh(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return
		case <-t.C:
			m.Refresh(ctx)
		}
	}
}

func (m *Manager) setStatus(s Status) {
	s.CheckedAt = m.now()
	m.mu.Lock()
	prev := m.status
	m.status = s
	m.mu.Unlock()
	if prev.Reason != s.Reason || prev.ReleaseID != s.ReleaseID {
		lvl := slog.LevelInfo
		if !s.Ready {
			lvl = slog.LevelWarn
		}
		m.log.Log(context.Background(), lvl, "readiness changed", "ready", s.Ready, "reason", s.Reason, "detail", s.Detail, "release_id", s.ReleaseID)
	}
}

func (m *Manager) readRegistry(ctx context.Context) (*registry.ServedRelease, []registry.ServedRelease, error) {
	if m.registry == nil {
		regParams := m.db
		regParams.MaxConns = 2
		p, err := regParams.Pool(ctx, m.registryDB)
		if err != nil {
			return nil, nil, err
		}
		m.registry = p
	}
	return registry.Serving(ctx, m.registry)
}

// Refresh re-reads the registry and loads or unloads releases as needed.
func (m *Manager) Refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	activeRef, all, err := m.serving(ctx)
	if err != nil && isUndefinedTable(err) {
		activeRef, all, err = nil, nil, nil // registry dropped concurrently: nothing to serve
	}
	if err != nil {
		m.expireByTime()
		cur := m.Active()
		s := Status{Reason: ReasonDBUnavailable, Detail: "registry: " + err.Error()}
		if cur != nil {
			// Keep serving the loaded releases; only report degraded state.
			s = Status{Ready: true, Reason: ReasonReady, ReleaseID: cur.Info.ReleaseID, Detail: "registry unreachable; serving loaded release"}
			if perr := m.ping(ctx, cur); perr != nil {
				s = Status{Reason: ReasonDBUnavailable, ReleaseID: cur.Info.ReleaseID, Detail: perr.Error()}
			}
		}
		m.setStatus(s)
		return
	}
	now := m.now()
	m.mu.RLock()
	current := make(map[string]*Release, len(m.served))
	for id, rel := range m.served {
		current[id] = rel
	}
	m.mu.RUnlock()

	served := map[string]*Release{}
	known := map[string]Lookup{}
	var newActive *Release
	status := Status{Reason: ReasonNoRelease, Detail: "no release has been imported and activated; run an import"}
	for _, ref := range all {
		isActive := activeRef != nil && ref.ID == activeRef.ID
		pinned := !isActive && ref.State == registry.StateRetired && ref.PinnedUntil != nil && now.Before(*ref.PinnedUntil)
		if !isActive && !pinned {
			known[ref.ID] = Expired
			continue
		}
		rel, ok := current[ref.ID]
		if !ok {
			var reason string
			rel, reason, err = m.load(ctx, ref)
			if err != nil {
				if isActive {
					status = Status{Reason: reason, ReleaseID: ref.ID, Detail: err.Error()}
				} else {
					m.log.Warn("pinned release not servable", "release_id", ref.ID, "reason", reason, "err", err)
				}
				known[ref.ID] = Unavailable
				continue
			}
		}
		// A *Release is never modified: when its pin or activation time
		// changes, a new value sharing the same pool replaces it.
		var until *time.Time
		if !isActive {
			u := *ref.PinnedUntil
			until = &u
		}
		var activated time.Time
		if ref.ActivatedAt != nil {
			activated = *ref.ActivatedAt
		}
		cp := rel
		if !samePin(rel.PinnedUntil, until) || !rel.ActivatedAt.Equal(activated) {
			c := *rel
			c.PinnedUntil, c.ActivatedAt = until, activated
			cp = &c
		}
		served[ref.ID] = cp
		if isActive {
			newActive = cp
			status = Status{Ready: true, Reason: ReasonReady, ReleaseID: ref.ID}
			if ok {
				if err := m.ping(ctx, cp); err != nil {
					status = Status{Reason: ReasonDBUnavailable, ReleaseID: ref.ID, Detail: err.Error()}
				}
			}
		}
	}
	if activeRef != nil && newActive == nil && status.Reason == ReasonNoRelease {
		status = Status{Reason: ReasonRegistryBroken, ReleaseID: activeRef.ID, Detail: "active release is not listed in the registry"}
	}
	fresh, ferr := m.fresh(ctx)
	m.mu.Lock()
	m.active, m.served, m.known = newActive, served, known
	if ferr == nil {
		m.freshness = fresh
	}
	m.mu.Unlock()
	if ferr != nil {
		m.log.Warn("could not read the update mode from the registry; keeping the last one", "err", ferr)
	}
	m.retire(current, served)
	m.setStatus(status)
}

// expireByTime drops pinned releases whose grace ended while the registry
// cannot be read.
func (m *Manager) expireByTime() {
	now := m.now()
	m.mu.Lock()
	current := m.served
	served := make(map[string]*Release, len(current))
	for id, rel := range current {
		if rel.PinnedUntil != nil && !now.Before(*rel.PinnedUntil) {
			m.known[id] = Expired
			continue
		}
		served[id] = rel
	}
	m.served = served
	m.mu.Unlock()
	m.retire(current, served)
}

// retire closes, after the drain period, the pools of releases that were
// served and no longer are. A pool shared by a copied Release is closed only
// when no served entry still uses it.
func (m *Manager) retire(before, after map[string]*Release) {
	for id, rel := range before {
		if next, ok := after[id]; ok && next.Pool == rel.Pool {
			continue
		}
		r := rel
		m.log.Info("release no longer served; closing its pool after the drain period", "release_id", id, "drain", m.drain.String())
		m.after(m.drain, func() { m.close(r) })
	}
}

func (m *Manager) shutdown() {
	m.mu.Lock()
	served := m.served
	m.active, m.served = nil, map[string]*Release{}
	m.mu.Unlock()
	for _, rel := range served {
		m.close(rel)
	}
	if m.registry != nil {
		m.registry.Close()
	}
}

// loadRelease opens a release database and checks it is safe to serve.
func (m *Manager) loadRelease(ctx context.Context, ref registry.ServedRelease) (*Release, string, error) {
	if !releaseid.Valid(ref.ID) || ref.Database != releaseid.DatabaseName(ref.ID) {
		return nil, ReasonRegistryBroken, fmt.Errorf("registry entry %q/%q is malformed", ref.ID, ref.Database)
	}
	conn, err := m.db.Connect(ctx, ref.Database)
	if err != nil {
		return nil, ReasonDBUnavailable, err
	}
	rel, reason, err := Check(ctx, conn, ref.ID, m.fontstacks)
	_ = conn.Close(context.Background())
	if err != nil {
		return nil, reason, err
	}
	recorded := rel.Info.Toolchain
	pool, err := m.db.PoolAfterConnect(ctx, ref.Database, func(ctx context.Context, c *pgx.Conn) error {
		running, err := toolchain.Read(ctx, c)
		if err != nil {
			return err
		}
		return toolchain.Compare(recorded, running)
	})
	if err != nil {
		return nil, ReasonDBUnavailable, err
	}
	rel.Pool = pool
	return rel, ReasonReady, nil
}

func samePin(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}

func isUndefinedTable(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && (pgErr.SQLState() == "42P01" || pgErr.SQLState() == "3F000")
}

func isConnError(err error) bool {
	var pgErr interface{ SQLState() string }
	return !errors.As(err, &pgErr)
}
