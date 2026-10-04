package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/safefile"
)

// SubmitOptions configure `karta intake submit`.
type SubmitOptions struct {
	// File is the snapshot to deliver; it may be on untrusted transfer
	// space (a host share): its integrity rests on the expectation.
	File string
	// Provenance is an optional sidecar (default: FILE.provenance.json if
	// present).
	Provenance string
	// The expectation, obtained independently of this copy: a SHA-256 (and
	// optionally a size), or a karta-delivery/1 completion marker...
	ExpectSHA256 string
	ExpectSize   int64
	ExpectFile   string
	// ...or an explicit attestation by the named operator that the file is
	// complete (Reason then says why).
	Attest bool
	Reason string
	// Wait follows the publication to its outcome.
	Wait bool
	Poll time.Duration
}

// SubmitResult is what the command reports.
type SubmitResult struct {
	Name            string      `json:"name"`
	SHA256          string      `json:"sha256"`
	SizeBytes       int64       `json:"size_bytes"`
	AuthorizationID int64       `json:"authorization_id"`
	Expectation     string      `json:"expectation"`
	Submission      *Submission `json:"submission,omitempty"`
}

// ErrExpectation is a command without a usable expectation or attestation.
var ErrExpectation = errors.New("an independent expectation is required: --expect-sha256 [--expect-size], or --expect-file " +
	"(a karta-delivery/1 completion marker), or --attest-complete with --reason")

// Submit delivers one file: a deliberate delivery by the credential's
// named operator (scope intake_submit). It settles the caller's earlier
// handoffs first (orphans of a crashed run are discarded, never resumed).
func Submit(ctx context.Context, h *Handoffer, o SubmitOptions, log *slog.Logger, progress func(Record)) (SubmitResult, error) {
	var res SubmitResult
	sha, size, how, err := expectation(o)
	if err != nil {
		return res, err
	}
	res.Expectation = how
	if _, err := h.reconcile(ctx, nil); err != nil {
		return res, err
	}
	d := delivery{path: o.File, expectSHA256: sha, expectSize: size}
	reason := fmt.Sprintf("intake command: file %s; %s", filepath.Base(o.File), how)
	if o.Reason != "" {
		reason += "; " + o.Reason
	}
	d.reason = reason
	prov := o.Provenance
	if prov == "" {
		if fi, err := os.Lstat(o.File + ".provenance.json"); err == nil && fi.Mode().IsRegular() {
			prov = o.File + ".provenance.json"
		}
	}
	if prov != "" {
		fi, err := os.Lstat(prov)
		switch {
		case err != nil:
			return res, refuse(CodeChanged, "provenance sidecar: %v", err)
		case !fi.Mode().IsRegular():
			return res, refuse(CodeNotRegular, "the provenance sidecar %s is not a regular file", prov)
		case fi.Size() > MaxSidecarBytes:
			return res, refuse(CodeTooLarge, "the provenance sidecar is larger than %d bytes", MaxSidecarBytes)
		}
		d.sidecarPath, d.sidecarWant = prov, stateOf(fi)
	}
	ho, err := h.deliver(ctx, d)
	if err != nil {
		return res, err
	}
	res.Name, res.SHA256, res.SizeBytes, res.AuthorizationID = ho.Name, ho.SHA256, ho.Size, ho.AuthorizationID
	log.Info("delivered to the publisher", "handoff", ho.Name, "sha256", ho.SHA256, "size_bytes", ho.Size, "authorization_id", ho.AuthorizationID,
		"expectation", how)
	if !o.Wait {
		return res, nil
	}
	poll := o.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	rec, err := h.waitOutcome(ctx, ho.Name, ho.AuthorizationID, poll, progress)
	res.Submission = rec.Submission
	return res, err
}

func expectation(o SubmitOptions) (string, int64, string, error) {
	n := 0
	if o.ExpectSHA256 != "" {
		n++
	}
	if o.ExpectFile != "" {
		n++
	}
	if o.Attest {
		n++
	}
	if n != 1 || (o.ExpectSize != 0 && o.ExpectSHA256 == "") {
		return "", 0, "", ErrExpectation
	}
	switch {
	case o.ExpectSHA256 != "":
		if !hexDigest.MatchString(o.ExpectSHA256) || o.ExpectSize < 0 {
			return "", 0, "", fmt.Errorf("--expect-sha256 must be 64 hex digits and --expect-size positive")
		}
		return strings.ToLower(o.ExpectSHA256), o.ExpectSize, "SHA-256 expected by the operator (--expect-sha256)", nil
	case o.ExpectFile != "":
		b, err := safefile.ReadRegular(o.ExpectFile, MaxCompletionBytes)
		if err != nil {
			return "", 0, "", fmt.Errorf("--expect-file: %w", err)
		}
		c, err := ParseCompletion(b, filepath.Base(o.File))
		if err != nil {
			return "", 0, "", fmt.Errorf("--expect-file: %w", err)
		}
		return c.SHA256, c.SizeBytes, "SHA-256 and size from the completion marker " + filepath.Base(o.ExpectFile), nil
	}
	if strings.TrimSpace(o.Reason) == "" {
		return "", 0, "", fmt.Errorf("--attest-complete needs --reason: the attestation is recorded with your credential")
	}
	return "", 0, "the operator attests the file is complete (--attest-complete)", nil
}
