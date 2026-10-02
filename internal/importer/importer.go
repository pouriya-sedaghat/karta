// Package importer verifies an OSM snapshot and builds one immutable,
// validated release database from it. It does not activate releases: the
// publication service (package publish) decides whether and when a built
// release becomes active.
//
// Verify checks the input without touching any database except for a
// read-only lookup of operator authorizations:
//
//   - a regular file within the size limit, scanned completely (osmfile)
//   - the snapshot's box (PBF header or provenance) equals the region box
//   - the provenance sidecar, when present or required, matches the file and
//     is internally consistent (provenance.Verify)
//   - the data timestamp comes from the provenance source header or the
//     snapshot header (never a file time), is consistent between them, and
//     is neither before OSM existed nor in the future
//   - the SHA-256 is pinned in the region configuration or authorized for
//     the region by an operator
//
// Build then, under the caller's build lock:
//
//  1. creates an isolated candidate database from the PostGIS template and
//     derives the release id (it includes the database toolchain)
//  2. returns the existing release if that id is already built
//  3. imports with osm2pgsql (flex) and runs the post-import SQL
//  4. validates: row-count gates, the drop relative to the active release,
//     the tile layer contract, the style, representative tiles and searches
//  5. records metadata, makes the database read-only, renames it to
//     karta_<release_id> and marks the release ready
//
// Any failure drops the candidate database and records the reason; no
// existing release is touched.
package importer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pouriya-sedaghat/karta/internal/dbconn"
	"github.com/pouriya-sedaghat/karta/internal/failpoint"
	"github.com/pouriya-sedaghat/karta/internal/glyphs"
	"github.com/pouriya-sedaghat/karta/internal/mvt"
	"github.com/pouriya-sedaghat/karta/internal/osmfile"
	"github.com/pouriya-sedaghat/karta/internal/provenance"
	"github.com/pouriya-sedaghat/karta/internal/region"
	"github.com/pouriya-sedaghat/karta/internal/registry"
	"github.com/pouriya-sedaghat/karta/internal/releaseid"
	"github.com/pouriya-sedaghat/karta/internal/schema"
	"github.com/pouriya-sedaghat/karta/internal/search"
	"github.com/pouriya-sedaghat/karta/internal/style"
	"github.com/pouriya-sedaghat/karta/internal/toolchain"
)

// Error classes, mapped to distinct process exit codes by the CLI.
var (
	ErrInput      = errors.New("input verification failed")
	ErrValidation = errors.New("release validation failed")
	// ErrStorage is a build that ran out of disk space.
	ErrStorage = errors.New("insufficient storage")
)

// Input rejection codes (recorded with submissions and audit records).
const (
	CodeRegionConfig       = "region_config"
	CodeMalformed          = "malformed_snapshot"
	CodeRegionMismatch     = "region_mismatch"
	CodeProvenanceRequired = "provenance_required"
	CodeProvenanceInvalid  = "provenance_invalid"
	CodeTimestampMissing   = "timestamp_missing"
	CodeTimestampUntrusted = "timestamp_untrusted"
	CodeUnauthorizedDigest = "unauthorized_digest"
	CodeSnapshotChanged    = "snapshot_changed"
)

// InputError is an input rejection with a stable code.
type InputError struct {
	Code string
	Msg  string
}

func (e *InputError) Error() string { return e.Msg }
func (e *InputError) Unwrap() error { return ErrInput }

func inputErr(code, format string, args ...any) error {
	return &InputError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// InputCode returns the rejection code of an input error, or "".
func InputCode(err error) string {
	var ie *InputError
	if errors.As(err, &ie) {
		return ie.Code
	}
	return ""
}

// Attribution and license recorded in every release.
const (
	Attribution = `<a href="https://www.openstreetmap.org/copyright" target="_blank" rel="noopener">© OpenStreetMap contributors</a>`
	License     = "ODbL-1.0"
	LicenseURL  = "https://www.openstreetmap.org/copyright"
)

// Plausible data timestamps: not before OpenStreetMap existed.
var minDataTimestamp = time.Date(2004, 8, 9, 0, 0, 0, 0, time.UTC)

// Authorizer reports whether an operator authorized a digest for the region,
// returning a description of the authorization ("" if none).
type Authorizer func(ctx context.Context, regionID, digest string, size int64) (string, error)

// VerifyOptions configure input verification.
type VerifyOptions struct {
	SnapshotPath string
	// ProvenancePath defaults to <snapshot>.provenance.json when that exists.
	ProvenancePath string
	Region         region.Config
	MaxInputBytes  int64
	// MaxFutureSkew bounds how far in the future a data timestamp may be.
	MaxFutureSkew time.Duration
	Now           func() time.Time
	// Authorize is consulted when the digest is not pinned in the region.
	Authorize Authorizer
}

// Verified is a snapshot that passed every input check.
type Verified struct {
	Region     region.Config
	Info       osmfile.Info
	Provenance *provenance.Sidecar
	// ProvenanceSHA256 is the digest of the sidecar bytes ("" without one).
	ProvenanceSHA256 string
	Source           SourceReport
}

// Verify checks a snapshot (see the package documentation).
func Verify(ctx context.Context, o VerifyOptions) (*Verified, error) {
	cfg := o.Region
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	info, err := osmfile.Inspect(o.SnapshotPath, o.MaxInputBytes)
	if err != nil {
		return nil, inputErr(CodeMalformed, "%v", err)
	}
	src := SourceReport{File: filepath.Base(info.Path), Format: string(info.Format), Size: info.Size, SHA256: info.SHA256,
		HeaderBBox: info.BBox, Objects: map[string]int64{"nodes": info.Nodes, "ways": info.Ways, "relations": info.Relations}}
	if info.BBox != nil && !region.SameBBox(*info.BBox, cfg.BBox) {
		return nil, inputErr(CodeRegionMismatch, "file header box %v differs from region %q box %v; the snapshot belongs to another extract",
			*info.BBox, cfg.ID, cfg.BBox)
	}
	var prov *provenance.Sidecar
	var provTS time.Time
	provPath := o.ProvenancePath
	if provPath == "" {
		if _, err := os.Lstat(o.SnapshotPath + ".provenance.json"); err == nil {
			provPath = o.SnapshotPath + ".provenance.json"
		}
	}
	if provPath != "" {
		s, err := provenance.Load(provPath)
		if err != nil {
			return nil, inputErr(CodeProvenanceInvalid, "provenance: %v", err)
		}
		box, ts, err := s.Verify(info.SHA256, info.Size)
		if err != nil {
			return nil, inputErr(CodeProvenanceInvalid, "provenance: %v", err)
		}
		if !region.SameBBox(box, cfg.BBox) {
			return nil, inputErr(CodeRegionMismatch, "provenance box %q does not match region %q box %v", s.BBoxWGS84, cfg.ID, cfg.BBox)
		}
		prov, provTS = &s, ts
		src.Provenance = true
	} else if cfg.Source.RequireProvenance {
		return nil, inputErr(CodeProvenanceRequired, "region %q requires %s", cfg.ID, filepath.Base(o.SnapshotPath)+".provenance.json")
	}
	if info.BBox == nil && prov == nil {
		return nil, inputErr(CodeRegionMismatch, "the snapshot has no header box and no provenance sidecar, so its extract cannot be matched to region %q", cfg.ID)
	}
	src.DataTimestamp, src.DataTimestampSource, err = trustedTimestamp(info, prov, provTS, now(), o.MaxFutureSkew)
	if err != nil {
		return nil, err
	}
	switch {
	case cfg.Source.Pinned(info.SHA256):
		src.AuthorizedBy = "region configuration"
	case o.Authorize != nil:
		by, err := o.Authorize(ctx, cfg.ID, info.SHA256, info.Size)
		if err != nil {
			return nil, fmt.Errorf("look up digest authorizations: %w", err)
		}
		src.AuthorizedBy = by
	}
	if src.AuthorizedBy == "" {
		return nil, inputErr(CodeUnauthorizedDigest,
			"SHA-256 %s (%d bytes) of %s is not pinned in region %q and not authorized by an operator; refusing to import a different snapshot "+
				"(verify it, then authorize it: docs/runbook.md, \"Authorize a new snapshot\")", info.SHA256, info.Size, filepath.Base(info.Path), cfg.ID)
	}
	v := &Verified{Region: cfg, Info: info, Provenance: prov, Source: src}
	if prov != nil {
		sum := sha256.Sum256(prov.Raw)
		v.ProvenanceSHA256 = hex.EncodeToString(sum[:])
	}
	return v, nil
}

// trustedTimestamp picks the release's data timestamp. The provenance source
// header wins (an extract's own header has none); a snapshot header
// timestamp must then agree with it. File modification times are never used.
func trustedTimestamp(info osmfile.Info, prov *provenance.Sidecar, provTS, now time.Time, skew time.Duration) (time.Time, string, error) {
	var ts time.Time
	var source string
	switch {
	case prov != nil:
		ts, source = provTS, "provenance.source_fileinfo.header"
		if info.Timestamp != nil && !info.Timestamp.Equal(provTS) {
			return ts, source, inputErr(CodeTimestampUntrusted, "snapshot header timestamp %s disagrees with the provenance source timestamp %s",
				info.Timestamp.Format(time.RFC3339), provTS.Format(time.RFC3339))
		}
	case info.Timestamp != nil:
		ts, source = *info.Timestamp, string(info.Format)+"_header"
	default:
		return ts, source, inputErr(CodeTimestampMissing, "no trustworthy data timestamp (no provenance sidecar and none in the file header)")
	}
	if ts.Before(minDataTimestamp) {
		return ts, source, inputErr(CodeTimestampUntrusted, "data timestamp %s is before OpenStreetMap existed", ts.Format(time.RFC3339))
	}
	if ts.After(now.Add(skew)) {
		return ts, source, inputErr(CodeTimestampUntrusted, "data timestamp %s is in the future (now %s, allowed skew %s)",
			ts.Format(time.RFC3339), now.UTC().Format(time.RFC3339), skew)
	}
	return ts, source, nil
}

// BuildOptions configure one build.
type BuildOptions struct {
	Osm2pgsql  string
	CacheMB    int
	Processes  int
	Slim       bool
	DB         dbconn.Params
	TemplateDB string
	// Tablespace, when set, holds new release databases.
	Tablespace string
	KeepFailed bool
	Version    string
	// Actor and Source attribute registry audit records.
	Actor        string
	Source       string
	SubmissionID *int64
	// ActiveID and ActiveCounts are the active release of the same region
	// and its row counts, for the relative drop check (validation.
	// max_drop_fraction). ActiveID "" means there is nothing to compare
	// with; with an ActiveID, empty ActiveCounts fail validation.
	ActiveCounts map[string]int64
	ActiveID     string
}

// Built is the result of Build.
type Built struct {
	ReleaseID string
	// Existing is set when the release was already built (nothing was
	// imported); it is the registry row.
	Existing *registry.Release
	Report   *Report
}

// Report is the machine-readable import record, stored with the release.
type Report struct {
	ReleaseID        string               `json:"release_id"`
	Region           string               `json:"region"`
	Source           SourceReport         `json:"source"`
	SchemaRevision   string               `json:"schema_revision"`
	StyleRevision    string               `json:"style_revision"`
	Osm2pgsqlVersion string               `json:"osm2pgsql_version"`
	ImporterVersion  string               `json:"importer_version"`
	Identity         string               `json:"identity"`
	Toolchain        map[string]string    `json:"toolchain"`
	Counts           map[string]int64     `json:"counts"`
	Features         map[string]int64     `json:"features_by_category"`
	Names            NameReport           `json:"names"`
	Skipped          []SkipReport         `json:"skipped"`
	Geometry         map[string]int64     `json:"geometry_stats"`
	Checks           []Check              `json:"checks"`
	Timings          map[string]float64   `json:"timings_seconds"`
	Resources        ResourceReport       `json:"resources"`
	Searches         []SearchCheckOutcome `json:"searches"`
}

// SourceReport describes the verified input.
type SourceReport struct {
	File                string           `json:"file"`
	Format              string           `json:"format"`
	Size                int64            `json:"size_bytes"`
	SHA256              string           `json:"sha256"`
	HeaderBBox          *[4]float64      `json:"header_bbox"`
	DataTimestamp       time.Time        `json:"data_timestamp"`
	DataTimestampSource string           `json:"data_timestamp_source"`
	Provenance          bool             `json:"provenance_verified"`
	AuthorizedBy        string           `json:"authorized_by"`
	Objects             map[string]int64 `json:"objects,omitempty"`
}

// NameReport distinguishes explicit name:fa tags from Persian-script names.
type NameReport struct {
	SearchNameRows          int64 `json:"search_name_rows"`
	FeaturesWithName        int64 `json:"features_with_name"`
	FeaturesWithNameFa      int64 `json:"features_with_name_fa"`
	FeaturesWithNameEn      int64 `json:"features_with_name_en"`
	FeaturesPersianScriptNm int64 `json:"features_with_persian_script_name"`
}

// SkipReport counts objects osm2pgsql could not turn into geometry.
type SkipReport struct {
	Layer   string `json:"layer"`
	Reason  string `json:"reason"`
	OSMType string `json:"osm_type"`
	Count   int64  `json:"count"`
}

// Check is one validation gate.
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// SearchCheckOutcome records a validation search and its top results.
type SearchCheckOutcome struct {
	Query   string          `json:"q"`
	Lang    string          `json:"lang,omitempty"`
	Want    string          `json:"want"`
	Found   int             `json:"found_at_position"`
	Results []search.Result `json:"top_results"`
}

// ResourceReport records measured resource use.
type ResourceReport struct {
	Osm2pgsqlMaxRSSKiB int64 `json:"osm2pgsql_max_rss_kib"`
	ImporterMaxRSSKiB  int64 `json:"importer_max_rss_kib"`
	DatabaseBytes      int64 `json:"database_bytes"`
}

// storageError classifies a failure caused by a full disk: PostgreSQL's
// insufficient-resources class (53100 disk_full) or an ENOSPC message from a
// tool.
func storageError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if (errors.As(err, &pgErr) && pgErr.Code == "53100") || strings.Contains(err.Error(), "No space left on device") ||
		strings.Contains(err.Error(), "could not extend file") {
		return fmt.Errorf("%w: %v", ErrStorage, err)
	}
	return err
}

// Build imports a verified snapshot into a new candidate database and
// records it as a ready release. The caller holds the build lock on reg.
func Build(ctx context.Context, reg *pgx.Conn, v *Verified, opts BuildOptions, log *slog.Logger) (*Built, error) {
	start := time.Now()
	timings := map[string]float64{}
	lap := func(name string, t time.Time) { timings[name] = round3(time.Since(t).Seconds()) }
	cfg, src := v.Region, v.Source

	t := time.Now()
	glyphSet, err := glyphs.New()
	if err != nil {
		return nil, err
	}
	catalog := schema.Layers()
	styleJSON := style.Template()
	if err := style.Validate(styleJSON, catalog, glyphSet.Fontstacks()); err != nil {
		return nil, fmt.Errorf("%w: style does not match the tile layers: %v", ErrValidation, err)
	}
	o2pVersion, err := osm2pgsqlVersion(ctx, opts.Osm2pgsql)
	if err != nil {
		return nil, err
	}
	lap("verify_tools", t)

	// The release id depends on the versions of the database tools that
	// produce tiles and search results, which are only known inside a
	// database with PostGIS installed. The import therefore runs in a
	// randomly named candidate database; only a fully validated candidate is
	// renamed to karta_<release_id>.
	schemaRev, styleRev := schema.Revision(), style.Revision()
	if err := dropLeftoverCandidates(ctx, reg, log); err != nil {
		return nil, err
	}
	t = time.Now()
	candidate, err := candidateName()
	if err != nil {
		return nil, err
	}
	create := fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`, candidate, opts.TemplateDB)
	if opts.Tablespace != "" {
		create += " TABLESPACE " + opts.Tablespace
	}
	if _, err := reg.Exec(ctx, create); err != nil {
		return nil, storageError(fmt.Errorf("create candidate database from template %s: %w", opts.TemplateDB, err))
	}
	discard := func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := dropDatabase(cctx, reg, candidate); err != nil {
			log.Error("could not drop candidate", "database", candidate, "err", err)
		}
	}
	if _, err := reg.Exec(ctx, fmt.Sprintf(`REVOKE ALL ON DATABASE %s FROM PUBLIC; GRANT CONNECT ON DATABASE %s TO %s`, candidate, candidate, registry.ReaderRole)); err != nil {
		discard()
		return nil, err
	}
	rel, err := opts.DB.Connect(ctx, candidate)
	if err != nil {
		discard()
		return nil, fmt.Errorf("connect candidate database: %w", err)
	}
	defer rel.Close(context.Background())
	tools, err := toolchain.Read(ctx, rel)
	if err != nil {
		_ = rel.Close(ctx)
		discard()
		return nil, err
	}
	tools["osm2pgsql"] = o2pVersion

	inputs := releaseid.Inputs{
		SourceSHA256: src.SHA256, ProvenanceSHA256: v.ProvenanceSHA256,
		DataTimestamp: src.DataTimestamp, DataTimestampSource: src.DataTimestampSource,
		Region: cfg.Identity(), SchemaRevision: schemaRev, StyleRevision: styleRev,
		Attribution: Attribution, License: License, LicenseURL: LicenseURL,
		Toolchain: tools,
	}
	id, identity, err := releaseid.Derive(inputs)
	if err != nil {
		_ = rel.Close(ctx)
		discard()
		return nil, inputErr(CodeRegionConfig, "%v", err)
	}
	dbName := releaseid.DatabaseName(id)
	log.Info("release identified", "release_id", id, "database", dbName, "candidate", candidate,
		"schema", schemaRev, "style", styleRev, "toolchain", tools)
	existing, err := registry.Get(ctx, reg, id)
	if err != nil {
		_ = rel.Close(ctx)
		discard()
		return nil, err
	}
	if existing != nil {
		switch existing.State {
		case registry.StateActive, registry.StateReady, registry.StateRetired, registry.StateRemoving:
			_ = rel.Close(ctx)
			discard()
			log.Info("release already built; nothing to import", "release_id", id, "state", existing.State)
			return &Built{ReleaseID: id, Existing: existing}, nil
		}
	}
	failpoint.Hit("build.after_create")
	// A karta_<id> database without a ready registry row is the leftover of
	// an interrupted rename.
	if err := dropDatabase(ctx, reg, dbName); err != nil {
		_ = rel.Close(ctx)
		discard()
		return nil, err
	}
	size := src.Size
	if err := registry.BeginImport(ctx, reg, registry.Release{ID: id, Database: dbName, RegionID: cfg.ID, SourceSHA256: src.SHA256,
		SourceSize: &size, SchemaRevision: schemaRev, StyleRevision: styleRev, SubmissionID: opts.SubmissionID}, opts.Actor, opts.Source); err != nil {
		_ = rel.Close(ctx)
		discard()
		return nil, err
	}
	report := &Report{
		ReleaseID: id, Region: cfg.ID, Source: src, SchemaRevision: schemaRev, StyleRevision: styleRev,
		Osm2pgsqlVersion: o2pVersion, ImporterVersion: opts.Version, Timings: timings,
		Identity: identity, Toolchain: tools,
	}
	fail := func(cause error) (*Built, error) {
		cause = storageError(cause)
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = rel.Close(cctx)
		reason := cause.Error()
		if len(reason) > 2000 {
			reason = reason[:2000]
		}
		if opts.KeepFailed && candidate != dbName {
			log.Warn("keeping failed candidate for inspection until the next import", "database", candidate)
		} else {
			discard()
		}
		if err := registry.Fail(cctx, reg, id, reason, opts.Actor, opts.Source); err != nil {
			log.Error("could not record failure", "release_id", id, "err", err)
		}
		return &Built{ReleaseID: id, Report: report}, cause
	}

	if _, err := rel.Exec(ctx, schema.SetupSQL().SQL); err != nil {
		return fail(fmt.Errorf("setup SQL: %w", err))
	}
	if _, err := rel.Exec(ctx, `INSERT INTO karta.release_params (region_id, west, south, east, north) VALUES ($1, $2, $3, $4, $5)`,
		cfg.ID, cfg.BBox[0], cfg.BBox[1], cfg.BBox[2], cfg.BBox[3]); err != nil {
		return fail(err)
	}
	lap("create_database", t)

	t = time.Now()
	rss, err := runOsm2pgsql(ctx, opts, v.Info.Path, candidate, log)
	report.Resources.Osm2pgsqlMaxRSSKiB = rss
	if err != nil {
		return fail(err)
	}
	lap("osm2pgsql", t)
	if digest, err := osmfile.Digest(v.Info.Path); err != nil || digest != src.SHA256 {
		return fail(inputErr(CodeSnapshotChanged, "snapshot changed during import"))
	}

	t = time.Now()
	if err := registry.SetValidating(ctx, reg, id, src.DataTimestamp); err != nil {
		return fail(err)
	}
	if err := runPostSQL(ctx, rel); err != nil {
		return fail(err)
	}
	lap("post_import_sql", t)
	failpoint.Hit("build.after_import")

	t = time.Now()
	if err := collect(ctx, rel, report); err != nil {
		return fail(err)
	}
	report.Checks = append(report.Checks, Check{Name: "style_matches_layer_catalog", Passed: true, Detail: "all style layers, fields and fontstacks are provided"})
	validate(ctx, rel, cfg, catalog, report)
	report.Checks = append(report.Checks, RelativeChecks(cfg.Validation.MaxDropFraction, opts.ActiveID, opts.ActiveCounts, report.Counts)...)
	lap("validate", t)
	var failed []string
	for _, c := range report.Checks {
		if !c.Passed {
			failed = append(failed, c.Name+": "+c.Detail)
		}
	}
	if len(failed) > 0 {
		return fail(fmt.Errorf("%w: %s", ErrValidation, strings.Join(failed, "; ")))
	}

	t = time.Now()
	report.Resources.ImporterMaxRSSKiB = selfMaxRSS()
	var provRaw any
	if v.Provenance != nil {
		provRaw = json.RawMessage(v.Provenance.Raw)
	}
	layersJSON, _ := json.Marshal(catalog)
	toolchainJSON, _ := json.Marshal(tools)
	if _, err := rel.Exec(ctx, `VACUUM (ANALYZE)`); err != nil {
		return fail(err)
	}
	if err := rel.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&report.Resources.DatabaseBytes); err != nil {
		return fail(err)
	}
	lap("finalize", t)
	timings["total"] = round3(time.Since(start).Seconds())
	reportJSON, err := json.Marshal(report)
	if err != nil {
		return fail(err)
	}
	if _, err := rel.Exec(ctx, `
INSERT INTO karta.release_info (release_id, schema_major, schema_revision, style_revision, region_id, region_name,
    bbox, center, default_zoom, minzoom, maxzoom, source_sha256, source_size, data_timestamp, data_timestamp_source,
    provenance, imported_at, importer_version, osm2pgsql_version, attribution, license, license_url, layers, style, report,
    identity, toolchain)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, now(), $17, $18, $19, $20, $21, $22, $23, $24, $25, $26)`,
		id, schema.Major, schemaRev, styleRev, cfg.ID, cfg.Name,
		cfg.BBox[:], cfg.View.Center[:], cfg.View.Zoom, catalog.MinZoom, catalog.MaxZoom,
		src.SHA256, src.Size, src.DataTimestamp, src.DataTimestampSource,
		provRaw, opts.Version, o2pVersion, Attribution, License, LicenseURL,
		layersJSON, []byte(styleJSON), reportJSON, identity, toolchainJSON); err != nil {
		return fail(fmt.Errorf("record release metadata: %w", err))
	}
	if _, err := rel.Exec(ctx, fmt.Sprintf(`ALTER DATABASE %s SET default_transaction_read_only = on`, candidate)); err != nil {
		return fail(err)
	}
	if err := rel.Close(ctx); err != nil {
		return fail(err)
	}
	if _, err := reg.Exec(ctx, fmt.Sprintf(`ALTER DATABASE %s RENAME TO %s`, candidate, dbName)); err != nil {
		return fail(fmt.Errorf("rename candidate to %s: %w", dbName, err))
	}
	candidate = dbName // a failure from here on drops the renamed database
	failpoint.Hit("build.after_rename")
	if err := registry.MarkReady(ctx, reg, id, schema.Major, report.Resources.DatabaseBytes, report.Counts); err != nil {
		return fail(err)
	}
	if err := registry.Audit(ctx, reg, registry.AuditEntry{Actor: opts.Actor, Source: opts.Source, Action: "import_validated", Target: id,
		Outcome: registry.OutcomeSucceeded, Detail: map[string]any{"database_bytes": report.Resources.DatabaseBytes, "seconds": timings["total"]}}); err != nil {
		return fail(err)
	}
	log.Info("release ready", "release_id", id, "seconds", timings["total"])
	return &Built{ReleaseID: id, Report: report}, nil
}

// RelativeChecks bounds the drop of every counted table relative to the
// active release of the same region (validation.max_drop_fraction): each
// table the active release counts must keep at least 1 - fraction of its
// rows. It returns nothing when the gate is off (no fraction) or there is no
// active release to compare with (activeID ""); when there is one, its
// counts must be known: missing counts fail the gate instead of skipping it.
func RelativeChecks(maxDrop *float64, activeID string, active, candidate map[string]int64) []Check {
	if maxDrop == nil || activeID == "" {
		return nil
	}
	if len(active) == 0 {
		return []Check{{Name: "relative_counts_available", Passed: false,
			Detail: "the row counts of the active release " + activeID + " are unknown, so the relative drop cannot be checked"}}
	}
	tables := make([]string, 0, len(active))
	for k := range active {
		tables = append(tables, k)
	}
	sort.Strings(tables)
	var checks []Check
	for _, table := range tables {
		prev := active[table]
		if prev <= 0 {
			continue
		}
		minimum := int64(math.Ceil(float64(prev) * (1 - *maxDrop)))
		checks = append(checks, Check{Name: "relative_count_" + table, Passed: candidate[table] >= minimum,
			Detail: fmt.Sprintf("%d rows; active release %s has %d, minimum %d (max drop %.0f%%)", candidate[table], activeID, prev, minimum, *maxDrop*100)})
	}
	return checks
}

// candidateName returns a fresh name for an import's working database.
func candidateName() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "karta_c" + hex.EncodeToString(b), nil
}

var candidatePattern = regexp.MustCompile(`^karta_c[0-9a-f]{24}$`)

// dropLeftoverCandidates removes working databases of interrupted or kept
// failed imports. It runs under the import lock, so no import is using them.
// DropLeftoverCandidates is dropLeftoverCandidates for recovery.
func DropLeftoverCandidates(ctx context.Context, conn *pgx.Conn, log *slog.Logger) error {
	return dropLeftoverCandidates(ctx, conn, log)
}

func dropLeftoverCandidates(ctx context.Context, conn *pgx.Conn, log *slog.Logger) error {
	rows, err := conn.Query(ctx, `SELECT datname FROM pg_database WHERE datname LIKE 'karta\_c%'`)
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, n := range names {
		if !candidatePattern.MatchString(n) {
			continue
		}
		log.Info("dropping leftover candidate database", "database", n)
		if err := dropDatabase(ctx, conn, n); err != nil {
			return err
		}
	}
	return nil
}

// DropDatabase drops a release or candidate database if it exists. Without
// force it fails while any session is connected, so a database in use is
// never removed.
func DropDatabase(ctx context.Context, conn *pgx.Conn, name string, force bool) error {
	if !releaseid.Valid(strings.TrimPrefix(name, "karta_")) && !candidatePattern.MatchString(name) {
		return fmt.Errorf("refusing to drop %q: not a release or candidate database", name)
	}
	sql := fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, name)
	if force {
		sql += ` WITH (FORCE)`
	}
	_, err := conn.Exec(ctx, sql)
	return err
}

func dropDatabase(ctx context.Context, conn *pgx.Conn, name string) error {
	_, err := conn.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
	return err
}

func runPostSQL(ctx context.Context, conn *pgx.Conn) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, f := range schema.PostImportSQL() {
		if _, err := tx.Exec(ctx, f.SQL); err != nil {
			return fmt.Errorf("post-import SQL %s: %w", f.Name, err)
		}
	}
	return tx.Commit(ctx)
}

func osm2pgsqlVersion(ctx context.Context, bin string) (string, error) {
	cmd := newTool(ctx, bin, "--version")
	out, err := cmd.CombinedOutput()
	_ = killGroup(cmd)
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return "", fmt.Errorf("run %s --version: %w", bin, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.Index(line, "osm2pgsql version "); i >= 0 {
			return strings.TrimSpace(line[i+len("osm2pgsql version "):]), nil
		}
	}
	return "", fmt.Errorf("unrecognised osm2pgsql --version output")
}

// runOsm2pgsql imports the snapshot. The password is passed through a
// temporary 0600 pgpass file, never on the command line or in the environment.
// osm2pgsql runs in its own process group (see runTool): when ctx ends (the
// publication deadline, or a shutdown) it is stopped with every process it
// started, and the error wraps the context's cause.
func runOsm2pgsql(ctx context.Context, opts BuildOptions, snapshot, dbName string, log *slog.Logger) (int64, error) {
	dir, err := os.MkdirTemp("", "karta-import-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	lua := filepath.Join(dir, "karta.lua")
	if err := os.WriteFile(lua, schema.FlexConfig(), 0o600); err != nil {
		return 0, err
	}
	pw, err := opts.DB.Password()
	if err != nil {
		return 0, err
	}
	esc := strings.NewReplacer(`\`, `\\`, `:`, `\:`)
	pgpass := filepath.Join(dir, "pgpass")
	line := fmt.Sprintf("%s:%d:%s:%s:%s\n", esc.Replace(opts.DB.Host), opts.DB.Port, dbName, esc.Replace(opts.DB.User), esc.Replace(pw))
	if err := os.WriteFile(pgpass, []byte(line), 0o600); err != nil {
		return 0, err
	}
	args := []string{
		"--create", "--output=flex", "--style=" + lua,
		"--host=" + opts.DB.Host, "--port=" + strconv.Itoa(opts.DB.Port),
		"--user=" + opts.DB.User, "--database=" + dbName,
		"--cache=" + strconv.Itoa(opts.CacheMB), "--number-processes=" + strconv.Itoa(opts.Processes),
		"--log-level=info",
	}
	if opts.Slim {
		args = append(args, "--slim", "--drop")
	}
	args = append(args, snapshot)
	cmd := newTool(ctx, opts.Osm2pgsql, args...)
	cmd.Env = []string{"PGPASSFILE=" + pgpass, "PGSSLMODE=" + opts.DB.SSLMode, "PGAPPNAME=karta-osm2pgsql", "PGCONNECT_TIMEOUT=10", "HOME=" + dir}
	cmd.Dir = dir
	rss, tail, err := runTool(ctx, cmd, "osm2pgsql", log)
	if err != nil {
		return rss, fmt.Errorf("%w: %s", err, tail)
	}
	return rss, nil
}

func collect(ctx context.Context, conn *pgx.Conn, r *Report) error {
	r.Counts = map[string]int64{}
	for _, table := range []string{"roads", "water", "waterways", "landcover", "buildings", "features", "place_names"} {
		var n int64
		if err := conn.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM karta.%s`, table)).Scan(&n); err != nil {
			return err
		}
		r.Counts[table] = n
	}
	var places, pois int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FILTER (WHERE category = 'place'), count(*) FILTER (WHERE category <> 'place') FROM karta.features`).Scan(&places, &pois); err != nil {
		return err
	}
	r.Counts["places"], r.Counts["pois"] = places, pois
	r.Features = map[string]int64{}
	rows, err := conn.Query(ctx, `SELECT category, count(*) FROM karta.features GROUP BY 1`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			rows.Close()
			return err
		}
		r.Features[k] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Persian-script test: Arabic, Arabic Supplement, Arabic Extended-A and
	// both presentation-form blocks.
	if err := conn.QueryRow(ctx, `
SELECT (SELECT count(*) FROM karta.place_names),
       count(*) FILTER (WHERE names ? 'name'),
       count(*) FILTER (WHERE names ? 'name:fa'),
       count(*) FILTER (WHERE names ? 'name:en'),
       count(*) FILTER (WHERE names->>'name' ~ U&'[\0600-\06FF\0750-\077F\08A0-\08FF\FB50-\FDFF\FE70-\FEFF]')
FROM karta.features`).Scan(&r.Names.SearchNameRows, &r.Names.FeaturesWithName, &r.Names.FeaturesWithNameFa,
		&r.Names.FeaturesWithNameEn, &r.Names.FeaturesPersianScriptNm); err != nil {
		return err
	}
	rows, err = conn.Query(ctx, `SELECT layer, reason, karta.osm_type_name(osm_type), count(*) FROM karta.import_skipped GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`)
	if err != nil {
		return err
	}
	r.Skipped = []SkipReport{}
	for rows.Next() {
		var s SkipReport
		if err := rows.Scan(&s.Layer, &s.Reason, &s.OSMType, &s.Count); err != nil {
			rows.Close()
			return err
		}
		r.Skipped = append(r.Skipped, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	r.Geometry = map[string]int64{}
	rows, err = conn.Query(ctx, `SELECT key, value FROM karta.import_stats`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		r.Geometry[k] = v
	}
	return rows.Err()
}

func validate(ctx context.Context, conn *pgx.Conn, cfg region.Config, catalog schema.Catalog, r *Report) {
	add := func(name string, ok bool, format string, args ...any) {
		r.Checks = append(r.Checks, Check{Name: name, Passed: ok, Detail: fmt.Sprintf(format, args...)})
	}
	tables := make([]string, 0, len(cfg.Validation.MinCounts))
	for k := range cfg.Validation.MinCounts {
		tables = append(tables, k)
	}
	sort.Strings(tables)
	for _, table := range tables {
		minimum := cfg.Validation.MinCounts[table]
		add("min_count_"+table, r.Counts[table] >= minimum, "%d rows, minimum %d", r.Counts[table], minimum)
	}

	// The tile layer contract: tiles over the region centre at every other
	// zoom, plus the configured tiles, may only contain catalogued layers
	// and fields.
	type tileReq struct {
		z, x, y int
		want    []string
	}
	var reqs []tileReq
	for z := catalog.MinZoom; z <= catalog.MaxZoom; z += 2 {
		x, y := mvt.TileXY(cfg.View.Center[0], cfg.View.Center[1], z)
		reqs = append(reqs, tileReq{z: z, x: x, y: y})
	}
	for _, tc := range cfg.Validation.Tiles {
		x, y := mvt.TileXY(tc.Lon, tc.Lat, tc.Zoom)
		reqs = append(reqs, tileReq{z: tc.Zoom, x: x, y: y, want: tc.Layers})
	}
	for _, tr := range reqs {
		name := fmt.Sprintf("tile_%d_%d_%d", tr.z, tr.x, tr.y)
		var tile []byte
		if err := conn.QueryRow(ctx, `SELECT karta.tile($1, $2, $3)`, tr.z, tr.x, tr.y).Scan(&tile); err != nil {
			add(name, false, "tile query failed: %v", err)
			continue
		}
		layers, err := mvt.Layers(tile)
		if err != nil {
			add(name, false, "undecodable tile: %v", err)
			continue
		}
		var problems []string
		have := map[string]mvt.Layer{}
		for _, l := range layers {
			have[l.Name] = l
			cl, ok := catalog.Layer(l.Name)
			if !ok {
				problems = append(problems, "unexpected layer "+l.Name)
				continue
			}
			for _, k := range l.Keys {
				if _, ok := cl.Fields[k]; !ok {
					problems = append(problems, fmt.Sprintf("layer %s has uncatalogued field %s", l.Name, k))
				}
			}
			if l.Extent != uint64(catalog.Extent) { // #nosec G115 -- catalog extent is the constant 4096
				problems = append(problems, fmt.Sprintf("layer %s extent %d", l.Name, l.Extent))
			}
		}
		for _, w := range tr.want {
			if have[w].Features == 0 {
				problems = append(problems, "missing layer "+w)
			}
		}
		summary := make([]string, 0, len(layers))
		for _, l := range layers {
			summary = append(summary, fmt.Sprintf("%s=%d", l.Name, l.Features))
		}
		detail := fmt.Sprintf("%d bytes; %s", len(tile), strings.Join(summary, " "))
		if len(problems) > 0 {
			detail += "; " + strings.Join(problems, "; ")
		}
		add(name, len(problems) == 0, "%s", detail)
	}

	for _, sc := range cfg.Validation.Search {
		want := fmt.Sprintf("%s/%d", sc.OSMType, sc.OSMID)
		name := "search_" + sc.Query
		results, err := search.Run(ctx, conn, search.Request{Query: sc.Query, Limit: max(sc.MaxPosition, 5), Lang: sc.Lang})
		if err != nil {
			add(name, false, "search failed: %v", err)
			continue
		}
		pos := 0
		for i, res := range results {
			if res.ID == want {
				pos = i + 1
				break
			}
		}
		r.Searches = append(r.Searches, SearchCheckOutcome{Query: sc.Query, Lang: sc.Lang, Want: want, Found: pos, Results: results})
		add(name, pos >= 1 && pos <= sc.MaxPosition, "%s at position %d (required <= %d)", want, pos, sc.MaxPosition)
	}
}

func round3(v float64) float64 { return float64(int64(v*1000+0.5)) / 1000 }
