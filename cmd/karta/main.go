// Command karta runs the Karta map and place-search service.
//
//	karta serve                 serve the public HTTP API (configured by KARTA_* environment variables)
//	karta publisher             watch the inbox (and online deliveries), publish releases and serve the operator API
//	karta fetcher               poll the configured online source and deliver verified snapshots (opt-in)
//	karta intake watch|submit   the local intake: protected-folder watcher and authenticated command (opt-in)
//	karta intake check          the landing area preflight
//	karta bridge acquire|sign|serve  the controlled source bridge: download, sign without network, serve (opt-in)
//	karta import [flags]        publish an OSM snapshot file through the same path as the inbox
//	karta operator COMMAND      call the operator API (status, audit, metrics, authorize, revoke, activate, rollback, cleanup, online-*)
//	karta healthcheck [--live]  exit 0 if the local server is ready (or live)
//	karta registry-summary      print what a backup records about the registry (JSON)
//	karta restore-check [flags] verify a restored registry and its releases; --finalize records the restore
//	karta region-draft [flags]  print a region file for a snapshot (its exact box and digest; checks left empty)
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
	"github.com/pouriya-sedaghat/karta/internal/online"
	"github.com/pouriya-sedaghat/karta/internal/operator"
	"github.com/pouriya-sedaghat/karta/internal/osmfile"
	"github.com/pouriya-sedaghat/karta/internal/provenance"
	"github.com/pouriya-sedaghat/karta/internal/publish"
	"github.com/pouriya-sedaghat/karta/internal/region"
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
	exitTimeout     = 8
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
	case "fetcher":
		os.Exit(fetcher())
	case "intake":
		os.Exit(runIntake(os.Args[2:]))
	case "bridge":
		os.Exit(runBridge(os.Args[2:]))
	case "import":
		os.Exit(runImport(os.Args[2:]))
	case "operator":
		os.Exit(runOperator(os.Args[2:]))
	case "healthcheck":
		os.Exit(healthcheck(os.Args[2:]))
	case "registry-summary":
		os.Exit(registrySummary(os.Args[2:]))
	case "restore-check":
		os.Exit(restoreCheck(os.Args[2:]))
	case "region-draft":
		os.Exit(regionDraft(os.Args[2:]))
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(exitUsage)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: karta serve | publisher | fetcher | intake watch|submit|check | bridge acquire|sign|serve|status |\n"+
		"  import --snapshot FILE --region FILE [flags] | operator COMMAND [flags] |\n"+
		"  healthcheck [--live] [--url URL] | registry-summary | restore-check [flags] | region-draft --snapshot FILE --id ID --name NAME | version")
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
	if v := os.Getenv(failpoint.ClockVariable); v != "" {
		log.Warn("FIXED CLOCK ENABLED ("+failpoint.ClockVariable+"): handoff names use this time; never use in a deployment", "clock", v)
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
	var metrics *api.Metrics
	if cfg.MetricsListenAddr != "" {
		metrics = api.NewMetrics()
	}
	handler, err := api.New(api.Config{
		PublicBaseURL: cfg.PublicBaseURL, RequestTimeout: cfg.RequestTimeout,
		CORSAllowedOrigins: cfg.CORSAllowedOrigins, WebDir: cfg.WebDir, Metrics: metrics,
	}, mgr, g, log)
	if err != nil {
		log.Error("build handler", "err", err)
		return exitFailure
	}
	// Request metrics: a separate listener, authenticated with a scoped
	// monitoring credential; never on the public listener.
	var metricsSrv *http.Server
	if metrics != nil {
		creds, err := operator.OpenCredentialFile(cfg.MetricsTokensFile, log)
		if err != nil {
			log.Error("metrics credentials", "file", cfg.MetricsTokensFile, "err", err)
			return exitUsage
		}
		metricsSrv = &http.Server{
			Addr:              cfg.MetricsListenAddr,
			Handler:           operator.RequireScope(creds, operator.ScopeStatus, "/metrics", metrics.Handler(mgr), log),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    16 << 10,
			ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		}
		mln, err := net.Listen("tcp", cfg.MetricsListenAddr)
		if err != nil {
			log.Error("listen", "addr", cfg.MetricsListenAddr, "err", err)
			return exitFailure
		}
		go func() {
			if err := metricsSrv.Serve(mln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("metrics listener stopped", "err", err)
			}
		}()
		log.Info("metrics listening", "addr", mln.Addr().String())
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
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutdownCtx)
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
		MaxAttempts: c.MaxAttempts, PublishTimeout: c.PublishTimeout, LockTimeout: c.LockTimeout, PointerLockTimeout: c.PointerLockTimeout,
		MaxFutureSkew:       c.MaxFutureSkew,
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
	// Reloaded when the file changes, failing closed while it is broken: a
	// removed credential stops working without a restart (ADR 0006).
	credFile, err := operator.OpenStrictCredentialFile(cfg.OperatorTokensFile, log)
	if err != nil {
		log.Error("operator credentials", "file", cfg.OperatorTokensFile, "err", err)
		return exitUsage
	}
	creds, _ := credFile.Credentials()
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
	pc.OnlineSourcePath, pc.OnlineDir, pc.StaleAfter = cfg.OnlineSourceFile, cfg.OnlineDir, cfg.StaleAfter
	pc.IntakeDir, pc.IntakeMaxOpen = cfg.IntakeDir, cfg.IntakeMaxOpen
	derived := publish.DefaultIntakeMaxAge(cfg.IntakeMaxOpen, cfg.MaxAttempts, cfg.PublishTimeout)
	pc.IntakeMaxAge = cfg.IntakeMaxAge
	if pc.IntakeMaxAge == 0 {
		pc.IntakeMaxAge = derived
	} else if pc.IntakeMaxAge < derived && cfg.IntakeDir != "" {
		log.Warn("KARTA_INTAKE_AUTHORIZATION_MAX_AGE is below the derived bound: an intake delivery queued behind a long build, or retried "+
			"after interruptions, can expire before it is activated (it is then reported as authorization_expired)",
			"configured", pc.IntakeMaxAge.String(), "derived", derived.String())
	}
	if cfg.IntakeDir != "" {
		log.Info("local intake enabled", "handoff", cfg.IntakeDir, "authorization_max_age", pc.IntakeMaxAge.String(), "max_open", pc.IntakeMaxOpen)
	}
	if cfg.OnlineSourceFile != "" {
		src, err := checkSource(cfg.OnlineSourceFile, cfg.RegionFile)
		if err != nil {
			log.Error("online source", "file", cfg.OnlineSourceFile, "err", err)
			return exitUsage
		}
		log.Info("online updates enabled", "source", cfg.OnlineSourceFile, "outbox", cfg.OnlineDir, "trusted_keys", src.KeyIDs(),
			"require_operator_authorization", src.RequireOperatorAuthorization)
	}
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
	if err := retry(ctx, log, "record the update mode", func() error { return svc.Announce(ctx) }); err != nil {
		return exitOK
	}

	srv := &http.Server{
		Addr:              cfg.OperatorListenAddr,
		Handler:           operator.New(credFile, svc, log, cfg.OperatorRequestTimeout),
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

// checkSource loads the online source file and checks it belongs to the
// region the process serves.
func checkSource(sourceFile, regionFile string) (*online.Source, error) {
	src, err := online.LoadSource(sourceFile)
	if err != nil {
		return nil, err
	}
	reg, err := region.Load(regionFile)
	if err != nil {
		return nil, err
	}
	if src.RegionID != reg.ID {
		return nil, fmt.Errorf("the source is for region %q, but %s is region %q", src.RegionID, regionFile, reg.ID)
	}
	return src, nil
}

// fetcher runs the online source poller. It has no database credential and
// no listener: it writes verified deliveries into its outbox, which the
// publisher reads.
func fetcher() int {
	cfg, err := config.LoadFetcher(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "karta fetcher: invalid configuration:\n%v\n", err)
		return exitUsage
	}
	log := logger(cfg.LogLevel).With("service", "karta-fetcher", "version", version)
	logFailpoints(log)
	src, err := checkSource(cfg.SourceFile, cfg.RegionFile)
	if err != nil {
		log.Error("online source", "file", cfg.SourceFile, "err", err)
		return exitUsage
	}
	lock, err := online.LockOutbox(cfg.Dir)
	if err != nil {
		log.Error("outbox", "err", err)
		return exitFailure
	}
	defer lock.Close()
	f, err := online.NewFetcher(online.FetcherConfig{SourcePath: cfg.SourceFile, RegionPath: cfg.RegionFile, Dir: cfg.Dir,
		MaxInputBytes: cfg.MaxInputBytes, ReserveBytes: cfg.ReserveBytes, MaxFutureSkew: cfg.MaxFutureSkew, Version: version}, log)
	if err != nil {
		log.Error("prepare outbox", "dir", cfg.Dir, "err", err)
		return exitFailure
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	u := src.ManifestURLParsed()
	log.Info("fetcher ready", "manifest", u.Scheme+"://"+u.Host+u.EscapedPath(), "region", src.RegionID, "outbox", cfg.Dir,
		"trusted_keys", src.KeyIDs(), "poll_interval", src.PollInterval.D().String(), "allowed_networks", src.AllowedNetworks)
	f.Run(ctx)
	log.Info("fetcher stopped; a download in progress resumes at the next start")
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
	case errors.Is(err, publish.ErrPublicationTimeout):
		return exitTimeout
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
			"  revalidate --release ID --reason TEXT |\n"+
			"  rollback [--release ID] --reason TEXT [--expected ID|none] | cleanup --reason TEXT [--dry-run] | metrics |\n"+
			"  online-pause --reason TEXT | online-resume --reason TEXT | online-retry --reason TEXT |\n"+
			"  intake-pause --reason TEXT | intake-resume --reason TEXT")
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
	rel := fs.String("release", "", "release id (activate, revalidate, rollback)")
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
	case "revalidate":
		method, path, body = http.MethodPost, "/v1/operator/releases/"+*rel+"/revalidate", map[string]any{"reason": *reason}
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
	case "metrics":
		method, path = http.MethodGet, "/v1/operator/metrics"
	case "online-pause", "online-resume", "online-retry":
		method, path, body = http.MethodPost, "/v1/operator/online/"+cmd[len("online-"):], map[string]any{"reason": *reason}
	case "intake-pause", "intake-resume":
		method, path, body = http.MethodPost, "/v1/operator/intake/"+cmd[len("intake-"):], map[string]any{"reason": *reason}
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
	_, _ = os.Stdout.Write(resp.Body)
	fmt.Println()
	if resp.Status/100 != 2 {
		fmt.Fprintf(os.Stderr, "karta operator: %s %s: HTTP %d\n", method, filepath.Clean(path), resp.Status)
		return exitFailure
	}
	if cmd == "revalidate" {
		var r struct {
			Result struct {
				Passed          bool     `json:"passed"`
				DurationSeconds *float64 `json:"duration_seconds"`
			} `json:"result"`
		}
		err := json.Unmarshal(resp.Body, &r)
		if err == nil && r.Result.DurationSeconds != nil {
			fmt.Fprintln(os.Stderr, "karta operator:", timeoutAdvice(*r.Result.DurationSeconds))
		}
		// A completed evaluation the release failed: not activatable.
		if err != nil || !r.Result.Passed {
			fmt.Fprintln(os.Stderr, "karta operator: the release did not pass the validation policy in force; it cannot be activated under it")
			return exitValidation
		}
	}
	return exitOK
}

// timeoutAdvice states the operator timeouts an evaluation measured to take
// seconds needs, or that it is too long for the operator endpoint
// (runbook, "Measured-timeout gate").
func timeoutAdvice(seconds float64) string {
	d := time.Duration(seconds * float64(time.Second))
	request, client, ok := config.RevalidationTimeouts(d)
	if !ok {
		return fmt.Sprintf("the evaluation took %.3f s: twice that, with the client %v longer, exceeds the %v limit; do not use the operator "+
			"endpoint for this release and policy: resubmit the snapshot with `karta import --no-activate` (runbook, \"Measured-timeout gate\")",
			seconds, config.OperatorClientMargin, config.MaxOperatorTimeout)
	}
	return fmt.Sprintf("the evaluation took %.3f s; to revalidate this release under this policy on this host through the operator endpoint, "+
		"set KARTA_OPERATOR_REQUEST_TIMEOUT=%v (publisher) and KARTA_OPERATOR_CLIENT_TIMEOUT=%v (runbook, \"Measured-timeout gate\")",
		seconds, request, client)
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

// oneOffService opens the publication service for a one-off maintenance
// command, with the importer's configuration (KARTA_* variables) and the
// region in KARTA_REGION_FILE.
func oneOffService(ctx context.Context, name string) (*publish.Service, *slog.Logger, int) {
	cfg, err := config.LoadImport(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "karta %s: invalid configuration:\n%v\n", name, err)
		return nil, nil, exitUsage
	}
	log := logger(cfg.LogLevel).With("service", "karta-"+name, "version", version)
	g, err := glyphs.New()
	if err != nil {
		log.Error("load fonts", "err", err)
		return nil, nil, exitFailure
	}
	pc := publicationConfig(cfg, os.Getenv("KARTA_REGION_FILE"), "")
	pc.Fontstacks = g.Fontstacks()
	svc, err := publish.New(ctx, pc, log)
	if err != nil {
		log.Error("open the registry", "err", err)
		return nil, nil, exitFailure
	}
	return svc, log, exitOK
}

// registrySummary prints what a backup records about the registry.
func registrySummary(args []string) int {
	fs := flag.NewFlagSet("registry-summary", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	svc, log, code := oneOffService(ctx, "registry-summary")
	if svc == nil {
		return code
	}
	defer svc.Close()
	sum, err := svc.Summary(ctx)
	if err != nil {
		log.Error("registry summary", "err", err)
		return exitFailure
	}
	b, _ := json.MarshalIndent(sum, "", "  ")
	fmt.Println(string(b))
	return exitOK
}

func readSummary(path string) (*publish.RegistrySummary, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- operator-supplied backup file
	if err != nil {
		return nil, err
	}
	var s publish.RegistrySummary
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// restoreCheck verifies a restored registry and its release databases
// against what the backup recorded and, with --finalize, records the
// restore: automatic online activation is paused and the restored active
// pointer is audited (docs/operations.md, "Restore").
func restoreCheck(args []string) int {
	fs := flag.NewFlagSet("restore-check", flag.ContinueOnError)
	before := fs.String("expect-before", "", "registry summary taken before the backup (registry.before.json)")
	after := fs.String("expect-after", "", "registry summary taken after the backup (registry.after.json)")
	outbox := fs.String("fetcher-outbox", "", "restored fetcher outbox directory, to compare its verified serial")
	finalize := fs.Bool("finalize", false, "record the restore if every check passes: pause automatic online activation and audit it")
	backupID := fs.String("backup-id", "", "backup identifier recorded with --finalize")
	reason := fs.String("reason", "restore from backup", "reason recorded in the audit log")
	actor := fs.String("actor", "restore", "name recorded as the actor in the audit log")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	if *finalize && *backupID == "" {
		fmt.Fprintln(os.Stderr, "karta restore-check: --finalize needs --backup-id")
		return exitUsage
	}
	exp := publish.RestoreExpect{}
	if *before != "" && *after == "" {
		fmt.Fprintln(os.Stderr, "karta restore-check: --expect-before needs --expect-after")
		return exitUsage
	}
	if *after != "" {
		a, err := readSummary(*after)
		if err != nil {
			fmt.Fprintln(os.Stderr, "karta restore-check:", err)
			return exitUsage
		}
		exp.After = a
		if *before != "" {
			b, err := readSummary(*before)
			if err != nil {
				fmt.Fprintln(os.Stderr, "karta restore-check:", err)
				return exitUsage
			}
			exp.Before = b
		}
	}
	if *outbox != "" {
		st, err := online.ReadState(*outbox)
		if err != nil {
			fmt.Fprintln(os.Stderr, "karta restore-check: fetcher state:", err)
			return exitFailure
		}
		if st != nil {
			v := st.HighestSerial
			exp.FetcherSerial = &v
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	svc, log, code := oneOffService(ctx, "restore-check")
	if svc == nil {
		return code
	}
	defer svc.Close()
	rep, err := svc.CheckRestore(ctx, exp)
	if err != nil {
		log.Error("restore check", "err", err)
		return exitFailure
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(b))
	if !rep.Passed {
		log.Error("the restored registry failed its checks; nothing was recorded")
		return exitFailure
	}
	if *finalize {
		if err := svc.FinalizeRestore(ctx, publish.Principal{Name: *actor, Source: "cli"}, *backupID, *reason, rep, exp.FetcherSerial); err != nil {
			log.Error("record the restore", "err", err)
			return exitFailure
		}
		log.Info("restore recorded: automatic online activation is paused until an operator resumes it", "backup_id", *backupID,
			"active_release_id", rep.Summary.ActiveRelease)
	}
	return exitOK
}

// regionDraft prints a region file for a snapshot: its box exactly as the
// snapshot's header or verified provenance sidecar states it, its digest
// pinned, a default view, and empty validation checks to fill in from a
// measured import of that snapshot (docs/operations.md, "The Iran region").
func regionDraft(args []string) int {
	fs := flag.NewFlagSet("region-draft", flag.ContinueOnError)
	snapshot := fs.String("snapshot", "", "the snapshot the region is for (.osm.pbf)")
	provPath := fs.String("provenance", "", "its provenance sidecar (default: <snapshot>.provenance.json if present)")
	id := fs.String("id", "", "region id: lowercase letters, digits and -")
	name := fs.String("name", "", "region name")
	maxMB := fs.Int64("max-input-mb", 4096, "largest snapshot accepted (as KARTA_MAX_INPUT_MB)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *snapshot == "" || *id == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "usage: karta region-draft --snapshot FILE [--provenance FILE] --id ID --name NAME")
		return exitUsage
	}
	info, err := osmfile.Inspect(*snapshot, *maxMB<<20)
	if err != nil {
		fmt.Fprintln(os.Stderr, "karta region-draft:", err)
		return exitInput
	}
	if *provPath == "" {
		if fi, err := os.Lstat(*snapshot + ".provenance.json"); err == nil && fi.Mode().IsRegular() {
			*provPath = *snapshot + ".provenance.json"
		}
	}
	var box *[4]float64
	from := "the snapshot header"
	if info.BBox != nil {
		b := *info.BBox
		box = &b
	}
	if *provPath != "" {
		sc, err := provenance.Load(*provPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "karta region-draft:", err)
			return exitInput
		}
		pbox, _, err := sc.Verify(info.SHA256, info.Size)
		if err != nil {
			fmt.Fprintln(os.Stderr, "karta region-draft: the sidecar does not describe this snapshot:", err)
			return exitInput
		}
		if box != nil && !region.SameBBox(*box, pbox) {
			fmt.Fprintf(os.Stderr, "karta region-draft: the header box %v and the sidecar box %v differ; the publisher refuses such a snapshot\n", *box, pbox)
			return exitInput
		}
		box, from = &pbox, "the provenance sidecar"
	}
	if box == nil {
		fmt.Fprintln(os.Stderr, "karta region-draft: the snapshot has no header box and no provenance sidecar; no region can be matched to it")
		return exitInput
	}
	cfg, err := region.Draft(*id, *name, *box, info.SHA256, *provPath != "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "karta region-draft:", err)
		return exitUsage
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	fmt.Println(string(b))
	fmt.Fprintf(os.Stderr, "box from %s; %d nodes, %d ways, %d relations; SHA-256 %s pinned.\n"+
		"The validation checks are empty: import this snapshot (--no-activate), then set min_counts, max_drop_fraction, searches and tiles\n"+
		"from its measured report (docs/operations.md, \"The Iran region\"); never copy them from another region.\n",
		from, info.Nodes, info.Ways, info.Relations, info.SHA256)
	return exitOK
}
