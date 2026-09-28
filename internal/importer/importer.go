// Package importer builds one immutable release database from an OSM
// snapshot and registers it. The pipeline:
//
//  1. verify the input: regular file, size limit, SHA-256 against the region's
//     pinned digest, provenance sidecar, header box and data timestamp
//  2. derive the release id and take the registry import lock
//  3. create an isolated database from the PostGIS template and import with
//     osm2pgsql (flex), then run the post-import SQL in one transaction
//  4. validate: row-count gates, the tile layer contract, the style against
//     the layer catalog and served fonts, representative tiles and searches
//  5. record metadata, make the database read-only and activate it (Stage 1:
//     only when no release is active yet)
//
// Any failure drops the candidate database and records the reason; an
// existing active release is never touched.
package importer

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pouriya-sedaghat/karta/internal/dbconn"
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
)

// Error classes, mapped to distinct process exit codes by the CLI.
var (
	ErrInput        = errors.New("input verification failed")
	ErrActiveExists = errors.New("a different release is already active")
	ErrValidation   = errors.New("release validation failed")
)

// Attribution and license recorded in every release.
const (
	Attribution = `<a href="https://www.openstreetmap.org/copyright" target="_blank" rel="noopener">© OpenStreetMap contributors</a>`
	License     = "ODbL-1.0"
	LicenseURL  = "https://www.openstreetmap.org/copyright"
)

// Options configure one import run.
type Options struct {
	SnapshotPath string
	RegionPath   string
	// ProvenancePath defaults to <snapshot>.provenance.json when that exists.
	ProvenancePath string
	MaxInputBytes  int64
	Osm2pgsql      string
	CacheMB        int
	Processes      int
	Slim           bool
	DB             dbconn.Params
	RegistryDB     string
	TemplateDB     string
	KeepFailed     bool
	Version        string
	LockTimeout    time.Duration
}

// Result of a run.
type Result struct {
	ReleaseID     string  `json:"release_id"`
	AlreadyActive bool    `json:"already_active"`
	Report        *Report `json:"report,omitempty"`
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
	File                string      `json:"file"`
	Format              string      `json:"format"`
	Size                int64       `json:"size_bytes"`
	SHA256              string      `json:"sha256"`
	HeaderBBox          *[4]float64 `json:"header_bbox"`
	DataTimestamp       time.Time   `json:"data_timestamp"`
	DataTimestampSource string      `json:"data_timestamp_source"`
	Provenance          bool        `json:"provenance_verified"`
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

// Run executes the pipeline.
func Run(ctx context.Context, opts Options, log *slog.Logger) (*Result, error) {
	start := time.Now()
	timings := map[string]float64{}
	lap := func(name string, t time.Time) { timings[name] = round3(time.Since(t).Seconds()) }

	// 1. Verify input -----------------------------------------------------
	t := time.Now()
	cfg, err := region.Load(opts.RegionPath)
	if err != nil {
		return nil, fmt.Errorf("%w: region: %v", ErrInput, err)
	}
	info, err := osmfile.Inspect(opts.SnapshotPath, opts.MaxInputBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInput, err)
	}
	log.Info("snapshot inspected", "file", filepath.Base(info.Path), "bytes", info.Size, "sha256", info.SHA256)
	if want := cfg.Source.ExpectedSHA256; want != "" && info.SHA256 != want {
		return nil, fmt.Errorf("%w: SHA-256 of %s is %s but region %q pins %s; refusing to import a different snapshot",
			ErrInput, filepath.Base(info.Path), info.SHA256, cfg.ID, want)
	}
	src := SourceReport{File: filepath.Base(info.Path), Format: string(info.Format), Size: info.Size, SHA256: info.SHA256, HeaderBBox: info.BBox}
	if info.BBox != nil && !region.SameBBox(*info.BBox, cfg.BBox) {
		return nil, fmt.Errorf("%w: file header box %v differs from region %q box %v; the snapshot belongs to another extract",
			ErrInput, *info.BBox, cfg.ID, cfg.BBox)
	}
	var prov *provenance.Sidecar
	provPath := opts.ProvenancePath
	if provPath == "" {
		if _, err := os.Lstat(opts.SnapshotPath + ".provenance.json"); err == nil {
			provPath = opts.SnapshotPath + ".provenance.json"
		}
	}
	if provPath != "" {
		s, err := provenance.Load(provPath)
		if err != nil {
			return nil, fmt.Errorf("%w: provenance: %v", ErrInput, err)
		}
		if err := s.Check(info.SHA256, info.Size); err != nil {
			return nil, fmt.Errorf("%w: provenance: %v", ErrInput, err)
		}
		pb, err := s.BBox()
		if err != nil || !region.SameBBox(pb, cfg.BBox) {
			return nil, fmt.Errorf("%w: provenance box %q does not match region %q box %v", ErrInput, s.BBoxWGS84, cfg.ID, cfg.BBox)
		}
		prov = &s
		src.Provenance = true
	} else if cfg.Source.RequireProvenance {
		return nil, fmt.Errorf("%w: region %q requires %s", ErrInput, cfg.ID, filepath.Base(opts.SnapshotPath)+".provenance.json")
	}
	switch {
	case prov != nil:
		ts, err := prov.SourceTimestamp()
		if err != nil {
			return nil, fmt.Errorf("%w: provenance: %v", ErrInput, err)
		}
		src.DataTimestamp, src.DataTimestampSource = ts, "provenance.source_fileinfo.header"
	case info.Timestamp != nil:
		src.DataTimestamp, src.DataTimestampSource = *info.Timestamp, string(info.Format)+"_header"
	default:
		return nil, fmt.Errorf("%w: no trustworthy data timestamp (no provenance sidecar and none in the file header)", ErrInput)
	}
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
	lap("verify_input", t)

	// 2. Lock and build a candidate ------------------------------------
	// The release id depends on the versions of the database tools that
	// produce tiles and search results, which are only known inside a
	// database with PostGIS installed. The import therefore runs in a
	// randomly named candidate database; only a fully validated candidate is
	// renamed to karta_<release_id>.
	schemaRev, styleRev := schema.Revision(), style.Revision()
	provDigest := ""
	if prov != nil {
		sum := sha256.Sum256(prov.Raw)
		provDigest = hex.EncodeToString(sum[:])
	}
	reg, err := opts.DB.Connect(ctx, opts.RegistryDB)
	if err != nil {
		return nil, fmt.Errorf("connect registry: %w", err)
	}
	defer reg.Close(context.Background())
	if err := registry.Migrate(ctx, reg); err != nil {
		return nil, fmt.Errorf("migrate registry: %w", err)
	}
	if err := registry.LockImports(ctx, reg, opts.LockTimeout); err != nil {
		return nil, err
	}
	if err := dropLeftoverCandidates(ctx, reg, log); err != nil {
		return nil, err
	}
	t = time.Now()
	candidate, err := candidateName()
	if err != nil {
		return nil, err
	}
	if _, err := reg.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`, candidate, opts.TemplateDB)); err != nil {
		return nil, fmt.Errorf("create candidate database from template %s: %w", opts.TemplateDB, err)
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
	toolchain, err := readToolchain(ctx, rel)
	if err != nil {
		_ = rel.Close(ctx)
		discard()
		return nil, err
	}
	toolchain["osm2pgsql"] = o2pVersion

	// 3. Identify ------------------------------------------------------------
	inputs := releaseid.Inputs{
		SourceSHA256: info.SHA256, ProvenanceSHA256: provDigest,
		DataTimestamp: src.DataTimestamp, DataTimestampSource: src.DataTimestampSource,
		Region: cfg.Identity(), SchemaRevision: schemaRev, StyleRevision: styleRev,
		Attribution: Attribution, License: License, LicenseURL: LicenseURL,
		Toolchain: toolchain,
	}
	id, identity, err := releaseid.Derive(inputs)
	if err != nil {
		_ = rel.Close(ctx)
		discard()
		return nil, fmt.Errorf("%w: %v", ErrInput, err)
	}
	dbName := releaseid.DatabaseName(id)
	log.Info("release identified", "release_id", id, "database", dbName, "candidate", candidate,
		"schema", schemaRev, "style", styleRev, "toolchain", toolchain)
	active, err := registry.Active(ctx, reg)
	if err != nil {
		_ = rel.Close(ctx)
		discard()
		return nil, err
	}
	if active != nil && active.ID == id {
		_ = rel.Close(ctx)
		discard()
		log.Info("release is already active; nothing to do", "release_id", id)
		return &Result{ReleaseID: id, AlreadyActive: true}, nil
	}
	if active != nil {
		_ = rel.Close(ctx)
		discard()
		return nil, fmt.Errorf("%w: %s is serving; Stage 1 keeps one release, so run `make reset` before importing %s (switching releases while serving is Stage 2)",
			ErrActiveExists, active.ID, id)
	}
	if err := dropDatabase(ctx, reg, dbName); err != nil { // leftover of an interrupted rename
		_ = rel.Close(ctx)
		discard()
		return nil, err
	}
	if err := registry.Begin(ctx, reg, registry.Release{ID: id, Database: dbName, RegionID: cfg.ID, SourceSHA256: info.SHA256}, schemaRev, styleRev); err != nil {
		_ = rel.Close(ctx)
		discard()
		return nil, err
	}
	report := &Report{
		ReleaseID: id, Region: cfg.ID, Source: src, SchemaRevision: schemaRev, StyleRevision: styleRev,
		Osm2pgsqlVersion: o2pVersion, ImporterVersion: opts.Version, Timings: timings,
		Identity: identity, Toolchain: toolchain,
	}
	fail := func(cause error) (*Result, error) {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = rel.Close(cctx)
		reason := cause.Error()
		if len(reason) > 2000 {
			reason = reason[:2000]
		}
		if opts.KeepFailed {
			log.Warn("keeping failed candidate for inspection until the next import", "database", candidate)
		} else {
			discard()
		}
		if err := registry.Fail(cctx, reg, id, reason); err != nil {
			log.Error("could not record failure", "release_id", id, "err", err)
		}
		return &Result{ReleaseID: id, Report: report}, cause
	}

	// 4. Import ------------------------------------------------------------
	if _, err := rel.Exec(ctx, schema.SetupSQL().SQL); err != nil {
		return fail(fmt.Errorf("setup SQL: %w", err))
	}
	if _, err := rel.Exec(ctx, `INSERT INTO karta.release_params (region_id, west, south, east, north) VALUES ($1, $2, $3, $4, $5)`,
		cfg.ID, cfg.BBox[0], cfg.BBox[1], cfg.BBox[2], cfg.BBox[3]); err != nil {
		return fail(err)
	}
	lap("create_database", t)

	t = time.Now()
	rss, err := runOsm2pgsql(ctx, opts, candidate, log)
	report.Resources.Osm2pgsqlMaxRSSKiB = rss
	if err != nil {
		return fail(err)
	}
	lap("osm2pgsql", t)
	if digest, err := osmfile.Digest(opts.SnapshotPath); err != nil || digest != info.SHA256 {
		return fail(fmt.Errorf("%w: snapshot changed during import", ErrInput))
	}

	t = time.Now()
	if err := registry.SetState(ctx, reg, id, registry.StateValidating, &src.DataTimestamp); err != nil {
		return fail(err)
	}
	if err := runPostSQL(ctx, rel); err != nil {
		return fail(err)
	}
	lap("post_import_sql", t)

	// 5. Validate ----------------------------------------------------------
	t = time.Now()
	if err := collect(ctx, rel, report); err != nil {
		return fail(err)
	}
	report.Checks = append(report.Checks, Check{Name: "style_matches_layer_catalog", Passed: true, Detail: "all style layers, fields and fontstacks are provided"})
	validate(ctx, rel, cfg, catalog, report)
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

	// 6. Record, freeze, rename, activate --------------------------------
	t = time.Now()
	report.Resources.ImporterMaxRSSKiB = selfMaxRSS()
	var provRaw any
	if prov != nil {
		provRaw = json.RawMessage(prov.Raw)
	}
	layersJSON, _ := json.Marshal(catalog)
	toolchainJSON, _ := json.Marshal(toolchain)
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
		info.SHA256, info.Size, src.DataTimestamp, src.DataTimestampSource,
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
	if err := registry.SetState(ctx, reg, id, registry.StateReady, nil); err != nil {
		return fail(err)
	}
	if err := registry.ActivateFirst(ctx, reg, id); err != nil {
		return fail(err)
	}
	log.Info("release active", "release_id", id, "seconds", timings["total"])
	return &Result{ReleaseID: id, Report: report}, nil
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

// readToolchain records the versions of the database components whose
// behaviour shapes tiles and search results.
func readToolchain(ctx context.Context, conn *pgx.Conn) (map[string]string, error) {
	var pg, postgis, geos, proj, trgm, icu string
	err := conn.QueryRow(ctx, `
SELECT split_part(current_setting('server_version'), ' ', 1),
       public.postgis_lib_version(),
       public.postgis_geos_version(),
       split_part(public.postgis_proj_version(), ' ', 1),
       (SELECT extversion FROM pg_extension WHERE extname = 'pg_trgm'),
       COALESCE((SELECT collversion FROM pg_collation WHERE collname = 'und-x-icu'), '')`).Scan(
		&pg, &postgis, &geos, &proj, &trgm, &icu)
	if err != nil {
		return nil, fmt.Errorf("read database toolchain versions: %w", err)
	}
	return map[string]string{"postgresql": pg, "postgis": postgis, "geos": geos, "proj": proj, "pg_trgm": trgm, "icu": icu}, nil
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
	out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput() // #nosec G204 -- configured binary path
	if err != nil {
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
func runOsm2pgsql(ctx context.Context, opts Options, dbName string, log *slog.Logger) (int64, error) {
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
	args = append(args, opts.SnapshotPath)
	cmd := exec.CommandContext(ctx, opts.Osm2pgsql, args...) // #nosec G204 -- fixed arguments, validated values
	cmd.Env = []string{"PGPASSFILE=" + pgpass, "PGSSLMODE=" + opts.DB.SSLMode, "PGAPPNAME=karta-osm2pgsql", "PGCONNECT_TIMEOUT=10", "HOME=" + dir}
	cmd.Dir = dir
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start osm2pgsql: %w", err)
	}
	var tail []string
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		l := sc.Text()
		log.Info("osm2pgsql", "line", l)
		tail = append(tail, l)
		if len(tail) > 20 {
			tail = tail[1:]
		}
	}
	_, _ = io.Copy(io.Discard, out)
	err = cmd.Wait()
	rss := childMaxRSS(cmd)
	if err != nil {
		return rss, fmt.Errorf("osm2pgsql failed (%v): %s", err, strings.Join(tail, " | "))
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
