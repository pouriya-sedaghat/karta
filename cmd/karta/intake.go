package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/pouriya-sedaghat/karta/internal/config"
	"github.com/pouriya-sedaghat/karta/internal/failpoint"
	"github.com/pouriya-sedaghat/karta/internal/intake"
)

// runIntake is the local intake (Stage 5): `watch` (the protected-folder
// watcher, credential scope intake_watch), `submit` (the authenticated
// command, scope intake_submit) and `check` (the landing preflight).
func runIntake(args []string) int {
	if len(args) == 0 {
		intakeUsage()
		return exitUsage
	}
	switch args[0] {
	case "watch":
		return intakeWatch(args[1:])
	case "submit":
		return intakeSubmit(args[1:])
	case "check":
		return intakeCheck(args[1:])
	}
	intakeUsage()
	return exitUsage
}

func intakeUsage() {
	fmt.Fprintln(os.Stderr, "usage: karta intake watch | submit FILE (--expect-sha256 HEX [--expect-size N] | --expect-file MARKER | "+
		"--attest-complete --reason TEXT) [--provenance FILE] [--reason TEXT] [--no-wait] |\n"+
		"  check [--landing DIR] [--host-root DIR]")
}

func intakeHandoff(c config.Intake, letter string) (*intake.Handoffer, error) {
	client, err := intake.NewClient(c.OperatorURL, c.TokenFile, c.Timeout)
	if err != nil {
		return nil, err
	}
	now, err := failpoint.Clock()
	if err != nil {
		return nil, err
	}
	return intake.NewHandoffer(intake.HandoffConfig{Dir: c.HandoffDir, RegionPath: c.RegionFile, Client: client, Letter: letter, TTL: c.TTL,
		MaxInputBytes: c.MaxInputBytes, ReserveBytes: c.ReserveBytes, Log: logger(c.LogLevel), Now: now})
}

func writerGID(c config.Intake) *uint32 {
	if c.WriterGID < 0 {
		return nil
	}
	g := uint32(c.WriterGID) // #nosec G115 -- bounded by LoadIntake
	return &g
}

// intakeWatch runs the unattended watcher of the landing area.
func intakeWatch(args []string) int {
	fs := flag.NewFlagSet("intake watch", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	c, err := config.LoadIntake(os.Getenv, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "karta intake watch: invalid configuration:\n%v\n", err)
		return exitUsage
	}
	log := logger(c.LogLevel).With("service", "karta-intake", "version", version)
	logFailpoints(log)
	client, err := intake.NewClient(c.OperatorURL, c.TokenFile, c.Timeout)
	if err != nil {
		log.Error("intake credential", "err", err)
		return exitUsage
	}
	w, err := intake.NewWatcher(intake.WatcherConfig{Landing: c.LandingDir, OwnerUID: uint32(c.LandingUID), WriterGID: writerGID(c), // #nosec G115 -- bounded by LoadIntake
		Settle: c.Settle, Poll: c.Poll, Handoff: intake.HandoffConfig{Dir: c.HandoffDir, RegionPath: c.RegionFile, Client: client, TTL: c.TTL,
			MaxInputBytes: c.MaxInputBytes, ReserveBytes: c.ReserveBytes, Log: log}}, log)
	if err != nil {
		log.Error("prepare the intake", "err", err)
		return exitFailure
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("intake watcher ready", "landing", c.LandingDir, "landing_uid", c.LandingUID, "writer_gid", c.WriterGID, "handoff", c.HandoffDir,
		"operator", c.OperatorURL, "settle", c.Settle.String(), "poll", c.Poll.String())
	w.Run(ctx)
	log.Info("intake watcher stopped; an interrupted handoff is settled at the next start")
	return exitOK
}

// intakeSubmit is the authenticated command: one deliberate delivery by
// the credential's named operator, followed to its outcome.
func intakeSubmit(args []string) int {
	fs := flag.NewFlagSet("intake submit", flag.ContinueOnError)
	sha := fs.String("expect-sha256", "", "SHA-256 of the file, obtained independently of this copy (from its source)")
	size := fs.Int64("expect-size", 0, "its size in bytes (optional with --expect-sha256)")
	expectFile := fs.String("expect-file", "", "a karta-delivery/1 completion marker written by the file's producer")
	attest := fs.Bool("attest-complete", false, "attest, as the credential's named operator, that the file is complete (with --reason)")
	reason := fs.String("reason", "", "recorded with the authorization")
	prov := fs.String("provenance", "", "provenance sidecar (default: FILE.provenance.json if present)")
	noWait := fs.Bool("no-wait", false, "return once handed off, without waiting for the publication")
	file := ""
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		file, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil || file == "" || fs.NArg() != 0 {
		intakeUsage()
		return exitUsage
	}
	c, err := config.LoadIntake(os.Getenv, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "karta intake submit: invalid configuration:\n%v\n", err)
		return exitUsage
	}
	log := logger(c.LogLevel).With("service", "karta-intake-submit", "version", version)
	logFailpoints(log)
	h, err := intakeHandoff(c, intake.LetterSubmit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "karta intake submit:", err)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	res, err := intake.Submit(ctx, h, intake.SubmitOptions{File: file, Provenance: *prov, ExpectSHA256: *sha, ExpectSize: *size, ExpectFile: *expectFile,
		Attest: *attest, Reason: *reason, Wait: !*noWait}, log, func(r intake.Record) {
		st := "waiting for the publisher"
		if r.Submission != nil {
			st = r.Submission.State
		}
		fmt.Fprintf(os.Stderr, "karta intake submit: %s: %s\n", *r.Authorization.IntakeName, st)
	})
	b, _ := json.MarshalIndent(map[string]any{"result": res, "error": errString(err)}, "", "  ")
	fmt.Println(string(b))
	switch {
	case errors.Is(err, intake.ErrExpectation):
		return exitUsage
	case intake.RefusalCode(err) != "":
		return exitInput
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(os.Stderr, "karta intake submit: stopped while waiting; the publication continues, and the next run cleans up")
		return exitInterrupted
	case err != nil:
		return exitFailure
	case res.Submission == nil:
		return exitOK
	case res.Submission.State == "published" || (res.Submission.State == "duplicate" && res.Submission.ReasonCode != nil &&
		*res.Submission.ReasonCode == "duplicate_active"):
		return exitOK
	}
	return exitPolicy
}

func errString(err error) any {
	if err == nil {
		return nil
	}
	return err.Error()
}

// intakeCheck runs the landing preflight and fails unless it passes. With
// --host-root (the host's root mounted read-only), the landing path is a
// host path and its host parents are checked too.
func intakeCheck(args []string) int {
	fs := flag.NewFlagSet("intake check", flag.ContinueOnError)
	landing := fs.String("landing", "", "landing directory (default $KARTA_INTAKE_LANDING_DIR)")
	root := fs.String("host-root", "", "where the host's root is mounted, to check a host path and its parents")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	r := func(k string) string { return os.Getenv(k) }
	c, _ := config.LoadIntake(r, false)
	if *landing != "" {
		c.LandingDir = *landing
	}
	if c.LandingDir == "" || c.LandingUID < 0 {
		fmt.Fprintln(os.Stderr, "karta intake check: KARTA_INTAKE_LANDING_DIR (or --landing) and KARTA_INTAKE_LANDING_UID are required")
		return exitUsage
	}
	p := intake.RunPreflight(intake.PreflightOptions{Dir: c.LandingDir, Root: *root, OwnerUID: uint32(c.LandingUID), WriterGID: writerGID(c)}) // #nosec G115 -- bounded by LoadIntake
	b, _ := json.MarshalIndent(p, "", "  ")
	fmt.Println(string(b))
	if !p.OK {
		fmt.Fprintln(os.Stderr, "karta intake check: the landing area fails the preflight; the watcher would deliver nothing from it "+
			"(use the authenticated command, karta intake submit, instead)")
		return exitFailure
	}
	return exitOK
}
