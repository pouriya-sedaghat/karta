package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/bridge"
	"github.com/pouriya-sedaghat/karta/internal/config"
	"github.com/pouriya-sedaghat/karta/internal/operator"
)

// runBridge runs one process of the controlled source bridge (Stage 5):
// acquire (the only one with a route to the distributor), sign (no network;
// holds the key), serve (read-only HTTPS to the fetcher) or status.
func runBridge(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: karta bridge acquire | sign [--raise-high-water N --reason TEXT] | serve | status")
		return exitUsage
	}
	role := args[0]
	c, err := config.LoadBridge(os.Getenv, role)
	if err != nil {
		fmt.Fprintf(os.Stderr, "karta bridge %s: invalid configuration:\n%v\n", role, err)
		return exitUsage
	}
	log := logger(c.LogLevel).With("service", "karta-bridge-"+role, "version", version)
	logFailpoints(log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	switch role {
	case "acquire":
		return bridgeAcquire(ctx, c, log)
	case "sign":
		return bridgeSign(ctx, c, log, args[1:])
	case "serve":
		return bridgeServe(ctx, c, log)
	case "status":
		a, s, err := bridge.Statuses(c.SpoolDir, c.PublishDir)
		b, _ := json.MarshalIndent(map[string]any{"acquire": a, "sign": s, "error": errString(err)}, "", "  ")
		fmt.Println(string(b))
		if err != nil {
			return exitFailure
		}
		return exitOK
	}
	fmt.Fprintf(os.Stderr, "karta bridge: unknown role %q\n", role)
	return exitUsage
}

func bridgeAcquire(ctx context.Context, c config.Bridge, log *slog.Logger) int {
	src, err := bridge.LoadSource(c.SourceFile)
	if err != nil {
		log.Error("bridge source", "err", err)
		return exitUsage
	}
	a, err := bridge.NewAcquirer(bridge.AcquireConfig{SourcePath: c.SourceFile, Spool: c.SpoolDir, MaxInputBytes: c.MaxInputBytes,
		ReserveBytes: c.ReserveBytes, Version: version}, log)
	if err != nil {
		log.Error("prepare the spool", "err", err)
		return exitFailure
	}
	defer a.Close()
	log.Info("bridge acquire ready", "snapshot_url", src.SnapshotURL, "md5_url", src.MD5URL, "poll_interval", src.PollInterval.D().String(),
		"reverify_interval", src.ReverifyInterval.D().String(), "spool", c.SpoolDir)
	a.Run(ctx)
	return exitOK
}

func bridgeSign(ctx context.Context, c config.Bridge, log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("bridge sign", flag.ContinueOnError)
	raiseTo := fs.Int64("raise-high-water", 0, "raise the high-water serial to N after a state restore or loss, then exit (never lowers it)")
	reason := fs.String("reason", "", "why (recorded in the signer state)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	var raise *bridge.Raise
	if *raiseTo != 0 {
		raise = &bridge.Raise{To: *raiseTo, Reason: *reason}
	}
	s, err := bridge.NewSigner(bridge.SignConfig{ConfigPath: c.SignerFile, RegionPath: c.RegionFile, Spool: c.SpoolDir, Publish: c.PublishDir,
		StateDir: c.StateDir, MaxInputBytes: c.MaxInputBytes, MaxFutureSkew: c.MaxFutureSkew, Poll: c.SignPoll, Version: version}, log, raise)
	if err != nil {
		log.Error("the signer does not start", "err", err)
		if errors.Is(err, bridge.ErrFailClosed) {
			return exitPolicy
		}
		return exitFailure
	}
	defer s.Close()
	if raise != nil {
		log.Info("high-water serial raised; start the signer normally", "high_water", s.State().HighWater)
		return exitOK
	}
	log.Info("bridge sign ready", "high_water", s.State().HighWater, "spool", c.SpoolDir, "publish", c.PublishDir)
	s.Run(ctx)
	return exitOK
}

func bridgeServe(ctx context.Context, c config.Bridge, log *slog.Logger) int {
	cert, err := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
	if err != nil {
		log.Error("TLS certificate", "err", err)
		return exitUsage
	}
	srv := &http.Server{Addr: c.ListenAddr, Handler: bridge.Handler(c.PublishDir, log), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}},
		ErrorLog:  slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	errc := make(chan error, 2)
	go func() { errc <- srv.ListenAndServeTLS("", "") }()
	var msrv *http.Server
	if c.MetricsListenAddr != "" {
		creds, err := operator.OpenCredentialFile(c.MetricsTokensFile, log)
		if err != nil {
			log.Error("metrics credentials", "err", err)
			return exitUsage
		}
		h := operator.RequireScope(creds, operator.ScopeStatus, "/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			_, _ = w.Write(bridge.Metrics(c.SpoolDir, c.PublishDir, time.Now())) // #nosec G705 -- generated Prometheus text, served as text/plain with nosniff
		}), log)
		msrv = &http.Server{Addr: c.MetricsListenAddr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
			WriteTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10}
		go func() { errc <- msrv.ListenAndServe() }()
	}
	log.Info("bridge serve ready", "addr", c.ListenAddr, "publish", c.PublishDir, "metrics_addr", c.MetricsListenAddr)
	select {
	case err := <-errc:
		log.Error("server stopped", "err", err)
		return exitFailure
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	if msrv != nil {
		_ = msrv.Shutdown(sctx)
	}
	return exitOK
}
