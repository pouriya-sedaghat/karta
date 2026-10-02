package publish

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/registry"
)

// ErrPublicationTimeout is a publication stopped because it ran longer than
// its whole-job deadline (KARTA_PUBLISH_TIMEOUT).
var ErrPublicationTimeout = errors.New("publication timed out")

// CodePublicationTimeout records a publication stopped at its deadline.
const CodePublicationTimeout = "publication_timeout"

// switchBound is added to the pointer lock timeout to bound the switch
// transaction, which runs outside the job deadline (see publish).
const switchBound = 30 * time.Second

// jobContext bounds one whole publication: staging, verification, the
// build and validation. When the deadline passes, every database statement
// is cancelled and osm2pgsql is stopped with all the processes it started
// (importer.runTool); the candidate is dropped as for any failed build.
func (s *Service) jobContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.cfg.PublishTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeoutCause(ctx, s.cfg.PublishTimeout, ErrPublicationTimeout)
}

// switchContext bounds the switch. It is detached from the job deadline and
// from a shutdown: a release that was built and validated in time is not
// refused because the deadline passes during its (short) switch, and the
// switch is never cancelled between its commit and its result. The pointer
// only moves in that committed transaction either way.
func (s *Service) switchContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), s.cfg.PointerLockTimeout+switchBound)
}

// timedOut reports whether job ended because of its own deadline (not
// because the process is shutting down: then parent ended too).
func timedOut(job, parent context.Context) bool {
	return parent.Err() == nil && errors.Is(context.Cause(job), ErrPublicationTimeout)
}

// timeoutOutcome turns the outcome of a publication that its deadline
// stopped (interrupted, or failed because a step was cancelled) into a
// final failure with its own reason code. It is not retried automatically,
// unlike an interrupted publication: a build that does not fit the budget
// would only run into it again. resubmit tells how to submit it again.
// Outcomes the publication reached anyway (published, ready, a rejection
// of the input, a duplicate) are kept.
func (s *Service) timeoutOutcome(job, parent context.Context, phase, resubmit string, out Outcome) Outcome {
	if !timedOut(job, parent) || (out.State != registry.SubInterrupted && out.State != registry.SubFailed) {
		return out
	}
	cause := ""
	if out.Reason != "" {
		cause = " (" + out.Reason + ")"
	}
	out.State, out.Code = registry.SubFailed, CodePublicationTimeout
	out.Reason = fmt.Sprintf("the publication did not finish within KARTA_PUBLISH_TIMEOUT (%s) and was stopped while %s%s; "+
		"the active release is unchanged and any candidate database was dropped. It is not retried automatically: find out why it took so long, "+
		"raise KARTA_PUBLISH_TIMEOUT if the build legitimately needs longer, then %s", s.cfg.PublishTimeout, phase, cause, resubmit)
	out.Err = fmt.Errorf("%w after %s while %s", ErrPublicationTimeout, s.cfg.PublishTimeout, phase)
	return out
}

// currentPhase is the phase of the publication in progress ("" if none).
func (s *Service) currentPhase() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return ""
	}
	return s.job.Phase
}
