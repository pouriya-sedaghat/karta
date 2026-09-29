// Command karta runs the Karta map and place-search service.
//
//	karta serve                 serve the HTTP API (configured by KARTA_* environment variables)
//	karta import [flags]        import an OSM snapshot as a new release
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
	"syscall"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/api"
	"github.com/pouriya-sedaghat/karta/internal/config"
	"github.com/pouriya-sedaghat/karta/internal/glyphs"
	"github.com/pouriya-sedaghat/karta/internal/importer"
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
	exitActive      = 4
	exitValidation  = 5
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
	case "import":
		os.Exit(runImport(os.Args[2:]))
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
	fmt.Fprintln(os.Stderr, "usage: karta serve | import --snapshot FILE --region FILE [--report FILE] | healthcheck [--live] | version")
}

func logger(level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
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

	mgr := release.NewManager(cfg.DB, cfg.RegistryDB, g.Fontstacks(), log)
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
	mgrDone := make(chan struct{})
	go func() {
		mgr.Run(ctx, cfg.ReleasePollInterval)
		close(mgrDone)
	}()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String(), "public_base_url", cfg.PublicBaseURL)
	select {
	case err := <-errc:
		log.Error("server stopped", "err", err)
		return exitFailure
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	<-mgrDone
	return exitOK
}

func runImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	snapshot := fs.String("snapshot", "", "OSM snapshot (.osm.pbf or .osm) to import")
	regionPath := fs.String("region", "", "region configuration JSON")
	provenancePath := fs.String("provenance", "", "provenance sidecar (default: <snapshot>.provenance.json if present)")
	reportPath := fs.String("report", "", "also write the JSON import report to this file")
	keepFailed := fs.Bool("keep-failed", false, "keep the candidate database of a failed import for inspection")
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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	res, err := importer.Run(ctx, importer.Options{
		SnapshotPath: *snapshot, RegionPath: *regionPath, ProvenancePath: *provenancePath,
		MaxInputBytes: cfg.MaxInputBytes, Osm2pgsql: cfg.Osm2pgsql, CacheMB: cfg.CacheMB,
		Processes: cfg.Processes, Slim: cfg.Slim, DB: cfg.DB, RegistryDB: cfg.RegistryDB,
		TemplateDB: cfg.TemplateDB, KeepFailed: *keepFailed, Version: version, LockTimeout: cfg.LockTimeout,
	}, log)
	if res != nil {
		out, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(out))
		if *reportPath != "" && res.Report != nil {
			b, _ := json.MarshalIndent(res.Report, "", "  ")
			if werr := os.WriteFile(*reportPath, append(b, '\n'), 0o644); werr != nil { // #nosec G306 -- report is not secret
				log.Error("write report", "err", werr)
			}
		}
	}
	if err == nil {
		return exitOK
	}
	log.Error("import failed", "err", err)
	switch {
	case errors.Is(err, context.Canceled):
		return exitInterrupted
	case errors.Is(err, importer.ErrInput):
		return exitInput
	case errors.Is(err, importer.ErrActiveExists):
		return exitActive
	case errors.Is(err, importer.ErrValidation):
		return exitValidation
	}
	return exitFailure
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
