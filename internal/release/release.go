// Package release loads servable releases for the API. Each release has its
// own read-only connection pool to its own database, and every request
// resolves exactly one release, either the explicit release_id or the
// active one. Stage 1 serves a single active release; the per-release pools
// and the registry pointer are the seams Stage 2 uses to serve a pinned
// previous release next to a newly activated one.
package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pouriya-sedaghat/karta/internal/dbconn"
	"github.com/pouriya-sedaghat/karta/internal/registry"
	"github.com/pouriya-sedaghat/karta/internal/releaseid"
	"github.com/pouriya-sedaghat/karta/internal/schema"
	"github.com/pouriya-sedaghat/karta/internal/style"
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
}

// Release is a loaded, validated release.
type Release struct {
	Info    Info
	Catalog schema.Catalog
	Style   json.RawMessage
	Pool    *pgxpool.Pool
}

// Status is the readiness state.
type Status struct {
	Ready     bool
	Reason    string
	Detail    string
	ReleaseID string
	CheckedAt time.Time
}

// Manager keeps the active release loaded and reports readiness.
type Manager struct {
	db         dbconn.Params
	registryDB string
	fontstacks []string
	log        *slog.Logger

	mu       sync.RWMutex
	active   *Release
	status   Status
	registry *pgxpool.Pool
}

// NewManager creates a manager; call Run to start loading.
func NewManager(db dbconn.Params, registryDB string, fontstacks []string, log *slog.Logger) *Manager {
	return &Manager{
		db: db, registryDB: registryDB, fontstacks: fontstacks, log: log,
		status: Status{Reason: ReasonStarting, Detail: "release not loaded yet", CheckedAt: time.Now()},
	}
}

// Active returns the active release, or nil.
func (m *Manager) Active() *Release {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

// Get returns a loaded release by id.
func (m *Manager) Get(id string) (*Release, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.active != nil && m.active.Info.ReleaseID == id {
		return m.active, true
	}
	return nil, false
}

// Status returns the last readiness state.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

// Run refreshes until ctx is cancelled. A running service keeps answering
// from the loaded release even if the registry becomes unreachable.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	m.Refresh(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.close()
			return
		case <-t.C:
			m.Refresh(ctx)
		}
	}
}

func (m *Manager) setStatus(s Status) {
	s.CheckedAt = time.Now()
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

// Refresh re-reads the active pointer and (re)loads the release if needed.
func (m *Manager) Refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if m.registry == nil {
		regParams := m.db
		regParams.MaxConns = 2
		p, err := regParams.Pool(ctx, m.registryDB)
		if err != nil {
			m.setStatus(Status{Reason: ReasonDBUnavailable, Detail: "registry: " + err.Error()})
			return
		}
		m.registry = p
	}
	// Until the first import migrates the registry its tables do not exist;
	// check first so every poll does not log an error in PostgreSQL.
	var migrated bool
	err := m.registry.QueryRow(ctx, `SELECT to_regclass('registry.active_release') IS NOT NULL`).Scan(&migrated)
	var ref *registry.Release
	if err == nil && migrated {
		ref, err = registry.Active(ctx, m.registry)
	}
	if err != nil {
		if isUndefinedTable(err) {
			ref = nil // registry dropped concurrently: nothing to serve
		} else {
			cur := m.Active()
			s := Status{Reason: ReasonDBUnavailable, Detail: "registry: " + err.Error()}
			if cur != nil {
				// Keep serving the loaded release; only report degraded state.
				s = Status{Ready: true, Reason: ReasonReady, ReleaseID: cur.Info.ReleaseID, Detail: "registry unreachable; serving loaded release"}
				if perr := cur.Pool.Ping(ctx); perr != nil {
					s = Status{Reason: ReasonDBUnavailable, ReleaseID: cur.Info.ReleaseID, Detail: perr.Error()}
				}
			}
			m.setStatus(s)
			return
		}
	}
	if ref == nil {
		m.swap(nil)
		m.setStatus(Status{Reason: ReasonNoRelease, Detail: "no release has been imported and activated; run an import"})
		return
	}
	if cur := m.Active(); cur != nil && cur.Info.ReleaseID == ref.ID {
		if err := cur.Pool.Ping(ctx); err != nil {
			m.setStatus(Status{Reason: ReasonDBUnavailable, ReleaseID: ref.ID, Detail: err.Error()})
			return
		}
		m.setStatus(Status{Ready: true, Reason: ReasonReady, ReleaseID: ref.ID})
		return
	}
	rel, reason, err := m.load(ctx, ref)
	if err != nil {
		m.setStatus(Status{Reason: reason, ReleaseID: ref.ID, Detail: err.Error()})
		return
	}
	m.swap(rel)
	m.setStatus(Status{Ready: true, Reason: ReasonReady, ReleaseID: ref.ID})
}

// swap replaces the active release and closes the previous release's pool
// immediately. That is only safe in Stage 1, where the pointer changes solely
// after a reset (the previous database no longer exists). Stage 2 design
// gate, not implemented: before switching while serving, keep the previous
// release loaded (Get must still resolve it) for a grace period covering
// in-flight and pinned requests, then close its pool.
func (m *Manager) swap(rel *Release) {
	m.mu.Lock()
	old := m.active
	m.active = rel
	m.mu.Unlock()
	if old != nil && (rel == nil || old != rel) {
		old.Pool.Close()
	}
}

func (m *Manager) close() {
	m.swap(nil)
	if m.registry != nil {
		m.registry.Close()
	}
}

// load opens the release database and checks it is safe to serve.
func (m *Manager) load(ctx context.Context, ref *registry.Release) (*Release, string, error) {
	if !releaseid.Valid(ref.ID) || ref.Database != releaseid.DatabaseName(ref.ID) {
		return nil, ReasonRegistryBroken, fmt.Errorf("registry entry %q/%q is malformed", ref.ID, ref.Database)
	}
	pool, err := m.db.Pool(ctx, ref.Database)
	if err != nil {
		return nil, ReasonDBUnavailable, err
	}
	rel := &Release{Pool: pool}
	var layers, styleJSON []byte
	var bbox, center []float64
	i := &rel.Info
	err = pool.QueryRow(ctx, `
SELECT release_id, schema_major, schema_revision, style_revision, region_id, region_name, bbox, center,
       default_zoom, minzoom, maxzoom, source_sha256, source_size, data_timestamp, data_timestamp_source,
       imported_at, importer_version, osm2pgsql_version, attribution, license, license_url, layers, style
FROM karta.release_info`).Scan(&i.ReleaseID, &i.SchemaMajor, &i.SchemaRevision, &i.StyleRevision, &i.RegionID,
		&i.RegionName, &bbox, &center, &i.Zoom, &i.MinZoom, &i.MaxZoom, &i.SourceSHA256, &i.SourceSize,
		&i.DataTimestamp, &i.DataTimestampSource, &i.ImportedAt, &i.ImporterVersion, &i.Osm2pgsqlVersion,
		&i.Attribution, &i.License, &i.LicenseURL, &layers, &styleJSON)
	if err != nil {
		pool.Close()
		if isConnError(err) {
			return nil, ReasonDBUnavailable, err
		}
		return nil, ReasonIncompatible, fmt.Errorf("read release metadata: %w", err)
	}
	problem := func(format string, args ...any) (*Release, string, error) {
		pool.Close()
		return nil, ReasonIncompatible, fmt.Errorf(format, args...)
	}
	if i.ReleaseID != ref.ID {
		return problem("database %s holds release %s, registry expects %s", ref.Database, i.ReleaseID, ref.ID)
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
	rel.Style = styleJSON
	if err := style.Validate(rel.Style, rel.Catalog, m.fontstacks); err != nil {
		return problem("release style does not match its tile layers or the served fonts: %v", err)
	}
	return rel, ReasonReady, nil
}

func isUndefinedTable(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && (pgErr.SQLState() == "42P01" || pgErr.SQLState() == "3F000")
}

func isConnError(err error) bool {
	var pgErr interface{ SQLState() string }
	return !errors.As(err, &pgErr)
}
