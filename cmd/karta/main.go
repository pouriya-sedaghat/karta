// Command karta runs the Karta map and place-search service.
//
//	karta serve                 serve the public HTTP API (configured by KARTA_* environment variables)
//	karta publisher             watch the inbox, publish releases and serve the operator API
//	karta import [flags]        publish an OSM snapshot file through the same path as the inbox
//	karta operator COMMAND      call the operator API (status, audit, authorize, revoke, activate, rollback, cleanup)
//	karta healthcheck [--live]  exit 0 if the local server is ready (or live)
//	karta version               print the build version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/api"
	"github.com/pouriya-sedaghat/karta/internal/config"
	"github.com/pouriya-sedaghat/karta/internal/failpoint"
	"github.com/pouriya-sedaghat/karta/internal/glyphs"
	"github.com/pouriya-sedaghat/karta/internal/importer"
	"github.com/pouriya-sedaghat/karta/internal/operator"
	"github.com/pouriya-sedaghat/karta/internal/publish"
	"github.com/pouriya-sedaghat/karta/internal/registry"
	"github.com/pouriya-sedaghat/karta/internal/release"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// Exit codes of `karta import`.
const (
	exitOK          = 0
	exitFailure     = 1
	exitUsage       = 2
	exitInput       = 3
	exitPolicy      = 4
	exitValidation  = 5
	exitStorage     = 6
	exitBusy        = 7
	exitInterrupted = 130
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(serve())
	case "publisher":
		os.Exit(publisher())
	case "import":
		os.Exit(runImport(os.Args[2:]))
	case "operator":
		os.Exit(runOperator(os.Args[2:]))
	case "healthcheck":
		os.Exit(healthcheck(os.Args[2:]))
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(exitUsage)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: karta serve | publisher | import --snapshot FILE --region FILE [flags] | operator COMMAND [flags] | healthcheck [--live] [--url URL] | version")
}

func logger(level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func logFailpoints(log *slog.Logger) {
	if on, unknown := failpoint.Enabled(); len(on)+len(unknown) > 0 {
		log.Warn("FAULT INJECTION ENABLED (KARTA_FAILPOINTS): this process exits at these points; never use in a deployment",
			"failpoints", on, "unknown", unknown)
	}
}

func serve() int {
	cfg, err := config.LoadServe(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "karta serve: invalid configuration:\n%v\n", err)
		return exitUsage
	}
	log := logger(cfg.LogLevel).With("service", "karta-api", "version", version)
	g, err := glyphs.New()
	if err != nil {
		log.Error("load fonts", "err", err)
		return exitFailure
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// A release that stops being served keeps its pool for longer than any
	// request can run, so requests that resolved it finish on it.
	drain := cfg.RequestTimeout + 5*time.Second
	mgr := release.NewManager(cfg.DB, cfg.RegistryDB, g.Fontstacks(), drain, log)
	handler, err := api.New(api.Config{
		PublicBaseURL: cfg.PublicBaseURL, RequestTimeout: cfg.RequestTimeout,
		CORSAllowedOrigins: cfg.CORSAllowedOrigins, WebDir: cfg.WebDir,
	}, mgr, g, log)
	if err != nil {
		log.Error("build handler", "err", err)
		return exitFailure
	}
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Error("listen", "addr", cfg.ListenAddr, "err", err)
		return exitFailure
	}
	mgrCtx, stopMgr := context.WithCancel(context.Background())
	mgrDone := make(chan struct{})
	go func() {
		mgr.Run(mgrCtx, cfg.ReleasePollInterval)
		close(mgrDone)
	}()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String(), "public_base_url", cfg.PublicBaseURL)
	select {
	case err := <-errc:
		log.Error("server stopped", "err", err)
		stopMgr()
		return exitFailure
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	// Pools close only after the server stopped taking requests.
	stopMgr()
	<-mgrDone
	return exitOK
}

func publicationConfig(c config.Import, regionFile, inboxDir string) publish.Config {
	return publish.Config{
		RegionPath: regionFile, InboxDir: inboxDir, StagingDir: c.StagingDir, MaxInputBytes: c.MaxInputBytes,
		PinGrace: c.PinGrace, RetainReleases: c.RetainReleases, CleanupMargin: c.CleanupMargin, CleanupInterval: c.CleanupInterval,
		MaxAttempts: c.MaxAttempts, LockTimeout: c.LockTimeout, PointerLockTimeout: c.PointerLockTimeout, MaxFutureSkew: c.MaxFutureSkew,
		StagingReserveBytes: c.StagingReserveBytes, StorageBudgetBytes: c.StorageBudgetBytes, DBVolumePath: c.DBVolumePath,
		MinFreeBytes: c.MinFreeBytes, CandidateSizeFactor: c.CandidateSizeFactor,
		Build: importer.BuildOptions{
			Osm2pgsql: c.Osm2pgsql, CacheMB: c.CacheMB, Processes: c.Processes, Slim: c.Slim, DB: c.DB,
			TemplateDB: c.TemplateDB, Tablespace: c.Tablespace, Version: version,
		},
		RegistryDB: c.RegistryDB,
	}
}

// retry runs f until it succeeds or ctx ends, backing off to 30 s.
func retry(ctx context.Context, log *slog.Logger, what string, f func() error) error {
	delay := time.Second
	for {
		err := f()
		if err == nil {
			return nil
		}
		log.Warn(what+" failed; retrying", "err", err, "in", delay.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(2*delay, 30*time.Second)
	}
}

func publisher() int {
	cfg, err := config.LoadPublisher(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "karta publisher: invalid configuration:\n%v\n", err)
		return exitUsage
	}
	log := logger(cfg.LogLevel).With("service", "karta-publisher", "version", version)
	logFailpoints(log)
	creds, err := operator.LoadCredentials(cfg.OperatorTokensFile)
	if err != nil {
		log.Error("operator credentials", "file", cfg.OperatorTokensFile, "err", err)
		return exitUsage
	}
	names := make([]string, 0, len(creds))
	for _, c := range creds {
		names = append(names, c.Name+"("+fmt.Sprint(c.ScopeList())+")")
	}
	g, err := glyphs.New()
	if err != nil {
		log.Error("load fonts", "err", err)
		return exitFailure
	}
	lock, err := publish.LockStaging(cfg.StagingDir)
	if err != nil {
		log.Error("staging", "err", err)
		return exitFailure
	}
	defer lock.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pc := publicationConfig(cfg.Import, cfg.RegionFile, cfg.InboxDir)
	pc.InboxPoll, pc.InboxSettle, pc.InboxMaxEntries, pc.AutoActivate = cfg.InboxPoll, cfg.InboxSettle, cfg.InboxMaxEntries, cfg.AutoActivate
	pc.Fontstacks = g.Fontstacks()
	var svc *publish.Service
	if err := retry(ctx, log, "connect and migrate the registry", func() (err error) {
		svc, err = publish.New(ctx, pc, log)
		return err
	}); err != nil {
		return exitOK
	}
	defer svc.Close()
	if err := retry(ctx, log, "recovery", func() error {
		_, err := svc.Recover(ctx, true)
		return err
	}); err != nil {
		return exitOK
	}

	srv := &http.Server{
		Addr:              cfg.OperatorListenAddr,
		Handler:           operator.New(creds, svc, log, cfg.OperatorRequestTimeout),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      cfg.OperatorRequestTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", cfg.OperatorListenAddr)
	if err != nil {
		log.Error("listen", "addr", cfg.OperatorListenAddr, "err", err)
		return exitFailure
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("publisher ready", "operator_addr", ln.Addr().String(), "inbox", cfg.InboxDir, "region_file", cfg.RegionFile,
		"auto_activate", cfg.AutoActivate, "pin_grace", cfg.PinGrace.String(), "retain", cfg.RetainReleases, "credentials", names)
	done := make(chan struct{}, 2)
	go func() { svc.RunInbox(ctx); done <- struct{}{} }()
	go func() { svc.RunMaintenance(ctx); done <- struct{}{} }()
	select {
	case err := <-errc:
		log.Error("operator server stopped", "err", err)
		stop()
	case <-ctx.Done():
	}
	log.Info("shutting down; a publication in progress is abandoned and recovered at the next start")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	<-done
	<-done
	return exitOK
}

func runImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	snapshot := fs.String("snapshot", "", "OSM snapshot (.osm.pbf or .osm) to publish")
	regionPath := fs.String("region", "", "region configuration JSON")
	provenancePath := fs.String("provenance", "", "provenance sidecar (default: <snapshot>.provenance.json if present)")
	reportPath := fs.String("report", "", "also write the JSON import report to this file")
	keepFailed := fs.Bool("keep-failed", false, "keep the candidate database of a failed import for inspection")
	noActivate := fs.Bool("no-activate", false, "build and validate only; leave the release ready for an operator to activate")
	allowRegion := fs.Bool("allow-region-change", false, "allow replacing an active release of another region")
	reason := fs.String("reason", "command-line import", "reason recorded in the audit log")
	actor := fs.String("actor", "cli", "name recorded as the actor in the audit log")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *snapshot == "" || *regionPath == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "karta import: --snapshot and --region are required")
		return exitUsage
	}
	cfg, err := config.LoadImport(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "karta import: invalid configuration:\n%v\n", err)
		return exitUsage
	}
	log := logger(cfg.LogLevel).With("service", "karta-importer", "version", version)
	logFailpoints(log)
	g, err := glyphs.New()
	if err != nil {
		log.Error("load fonts", "err", err)
		return exitFailure
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pc := publicationConfig(cfg, *regionPath, "")
	pc.Build.KeepFailed = *keepFailed
	pc.Fontstacks = g.Fontstacks()
	svc, err := publish.New(ctx, pc, log)
	if err != nil {
		log.Error("import failed", "err", err)
		return exitFailure
	}
	defer svc.Close()
	// The import holds the build lock only while building; recover first
	// what an interrupted import left behind (never the active release).
	if _, err := svc.Recover(ctx, false); err != nil {
		log.Error("import failed", "err", err)
		if errors.Is(err, registry.ErrBusy) {
			return exitBusy
		}
		return exitFailure
	}
	out := svc.ImportFile(ctx, publish.ImportOptions{SnapshotPath: *snapshot, ProvenancePath: *provenancePath,
		Activate: !*noActivate, AllowRegionChange: *allowRegion, Actor: *actor, Reason: *reason})
	res := struct {
		ReleaseID     string           `json:"release_id"`
		AlreadyActive bool             `json:"already_active"`
		Outcome       publish.Outcome  `json:"outcome"`
		Report        *importer.Report `json:"report,omitempty"`
	}{ReleaseID: out.ReleaseID, AlreadyActive: out.Code == publish.CodeDuplicateActive, Outcome: out, Report: out.Report}
	res.Outcome.Report = nil
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	if *reportPath != "" && out.Report != nil {
		rb, _ := json.MarshalIndent(out.Report, "", "  ")
		if werr := os.WriteFile(*reportPath, append(rb, '\n'), 0o644); werr != nil { // #nosec G306 -- report is not secret
			log.Error("write report", "err", werr)
		}
	}
	switch out.State {
	case registry.SubPublished:
		return exitOK
	case registry.SubDuplicate:
		if out.Code == publish.CodeDuplicateActive {
			log.Info("this exact release is already active; nothing to do", "release_id", out.ReleaseID)
			return exitOK
		}
	case registry.SubReady:
		if *noActivate && out.Code == publish.CodeManualActivation {
			log.Info("release validated and ready; not activated (--no-activate)", "release_id", out.ReleaseID)
			return exitOK
		}
	}
	err = out.Err
	if err == nil {
		err = fmt.Errorf("%s: %s", out.Code, out.Reason)
	}
	log.Error("import failed", "state", out.State, "reason_code", out.Code, "err", err)
	switch {
	case errors.Is(err, context.Canceled):
		return exitInterrupted
	case errors.Is(err, importer.ErrStorage):
		return exitStorage
	case errors.Is(err, importer.ErrInput):
		return exitInput
	case errors.Is(err, importer.ErrValidation):
		return exitValidation
	case errors.Is(err, registry.ErrBusy):
		return exitBusy
	case errors.Is(err, publish.ErrPolicy), out.State == registry.SubDuplicate, out.State == registry.SubReady:
		return exitPolicy
	}
	return exitFailure
}

// runOperator is the operator API client.
func runOperator(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: karta operator status | audit [--limit N] [--before-id N] | authorize --sha256 HEX [--size N] [--expires RFC3339] --reason TEXT |\n"+
			"  revoke --sha256 HEX --reason TEXT | activate --release ID --reason TEXT [--expected ID|none] |\n"+
			"  rollback [--release ID] --reason TEXT [--expected ID|none] | cleanup --reason TEXT [--dry-run]")
		return exitUsage
	}
	cmd := args[0]
	fs := flag.NewFlagSet("operator "+cmd, flag.ContinueOnError)
	tokenFile := fs.String("token-file", "", "file holding the operator token (default $KARTA_OPERATOR_TOKEN_FILE)")
	url := fs.String("url", "", "operator API base URL (default $KARTA_OPERATOR_URL or http://127.0.0.1:8081)")
	reason := fs.String("reason", "", "reason recorded in the audit log (required for actions)")
	sha := fs.String("sha256", "", "snapshot SHA-256")
	size := fs.Int64("size", 0, "snapshot size in bytes (authorize)")
	expires := fs.String("expires", "", "authorization end, RFC 3339 (authorize)")
	rel := fs.String("release", "", "release id (activate, rollback)")
	expected := fs.String("expected", "", "expected active release id, or none (activate, rollback)")
	allowRegion := fs.Bool("allow-region-change", false, "allow a region change (activate, rollback)")
	dryRun := fs.Bool("dry-run", false, "only report what cleanup would remove")
	limit := fs.Int("limit", 50, "audit records to show")
	before := fs.Int64("before-id", 0, "show audit records before this id")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	cc, err := config.LoadOperatorClient(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "karta operator: invalid configuration:\n%v\n", err)
		return exitUsage
	}
	if *tokenFile != "" {
		cc.TokenFile = *tokenFile
	}
	if *url != "" {
		cc.URL = *url
	}
	if cc.TokenFile == "" {
		fmt.Fprintln(os.Stderr, "karta operator: --token-file or KARTA_OPERATOR_TOKEN_FILE is required")
		return exitUsage
	}
	token, err := operator.ReadToken(cc.TokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "karta operator:", err)
		return exitUsage
	}
	client, err := operator.NewClient(cc.URL, token, cc.Timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "karta operator:", err)
		return exitUsage
	}
	var method, path string
	var body any
	switch cmd {
	case "status":
		method, path = http.MethodGet, "/v1/operator/status"
	case "audit":
		method, path = http.MethodGet, "/v1/operator/audit?limit="+strconv.Itoa(*limit)
		if *before > 0 {
			path += "&before_id=" + strconv.FormatInt(*before, 10)
		}
	case "authorize":
		b := map[string]any{"sha256": *sha, "reason": *reason}
		if *size > 0 {
			b["size_bytes"] = *size
		}
		if *expires != "" {
			b["expires_at"] = *expires
		}
		method, path, body = http.MethodPost, "/v1/operator/authorizations", b
	case "revoke":
		method, path, body = http.MethodPost, "/v1/operator/authorizations/"+*sha+"/revoke", map[string]any{"reason": *reason}
	case "activate":
		b := map[string]any{"reason": *reason, "allow_region_change": *allowRegion}
		if *expected != "" {
			b["expected_active_release_id"] = *expected
		}
		method, path, body = http.MethodPost, "/v1/operator/releases/"+*rel+"/activate", b
	case "rollback":
		b := map[string]any{"reason": *reason, "allow_region_change": *allowRegion}
		if *rel != "" {
			b["release_id"] = *rel
		}
		if *expected != "" {
			b["expected_active_release_id"] = *expected
		}
		method, path, body = http.MethodPost, "/v1/operator/rollback", b
	case "cleanup":
		method, path, body = http.MethodPost, "/v1/operator/cleanup", map[string]any{"reason": *reason, "dry_run": *dryRun}
	default:
		fmt.Fprintf(os.Stderr, "karta operator: unknown command %q\n", cmd)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	resp, err := client.Do(ctx, method, path, body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "karta operator:", err)
		return exitFailure
	}
	os.Stdout.Write(resp.Body)
	fmt.Println()
	if resp.Status/100 != 2 {
		fmt.Fprintf(os.Stderr, "karta operator: %s %s: HTTP %d\n", method, filepath.Clean(path), resp.Status)
		return exitFailure
	}
	return exitOK
}

// healthcheck is the container health probe; the distroless image has no
// shell or curl.
func healthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	live := fs.Bool("live", false, "check liveness instead of readiness")
	addr := fs.String("url", "http://127.0.0.1:8080", "server base URL")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	path := "/health/ready"
	if *live {
		path = "/health/live"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(*addr + path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitFailure
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "status", resp.Status)
		return exitFailure
	}
	return exitOK
}
