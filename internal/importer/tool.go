package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// outputGrace bounds how long the output of a finished or stopped build tool
// is still read. A descendant that inherited the pipe (a helper the tool
// started, or the children of a wrapper script) cannot keep a build waiting
// beyond it, also when it escaped the tool's process group.
var outputGrace = 10 * time.Second

// newTool prepares a build tool that stops with ctx: in its own process
// group, whose every member is killed when ctx ends, and with its output
// reading bounded by outputGrace.
func newTool(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...) // #nosec G204 -- configured binary, fixed arguments
	isolate(cmd)
	cmd.WaitDelay = outputGrace
	return cmd
}

// runTool runs a command prepared by newTool to completion or until ctx
// ends, logging its output line by line. Once it has exited, any process
// left in its group is killed. It returns the tool's peak RSS (KiB) and the
// last lines of its output. When ctx ended first, the error wraps
// context.Cause(ctx), so callers can tell a deadline or a shutdown from a
// tool failure.
func runTool(ctx context.Context, cmd *exec.Cmd, name string, log *slog.Logger) (int64, string, error) {
	out := &lineLog{log: log, name: name}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return 0, "", fmt.Errorf("start %s: %w", name, err)
	}
	err := cmd.Wait()
	out.flush()
	if kerr := killGroup(cmd); kerr != nil {
		log.Warn("could not stop the processes left by "+name, "err", kerr)
	}
	rss, tail := childMaxRSS(cmd), out.tailText()
	switch {
	case err == nil:
		return rss, tail, nil
	case errors.Is(err, exec.ErrWaitDelay):
		// The tool exited successfully; a process it started kept the
		// output open and was stopped.
		log.Warn(name+" exited, but a process it started kept its output open; it was stopped", "grace", outputGrace.String())
		return rss, tail, nil
	case ctx.Err() != nil:
		return rss, tail, fmt.Errorf("%s was stopped before it finished: %w", name, context.Cause(ctx))
	}
	return rss, tail, fmt.Errorf("%s failed (%v)", name, err)
}

// maxLogLine bounds one logged output line.
const maxLogLine = 4096

// lineLog logs a tool's output line by line and keeps the last lines for
// error messages. Stdout and stderr share it (one writer: exec calls Write
// from one goroutine at a time).
type lineLog struct {
	log  *slog.Logger
	name string
	buf  []byte
	tail []string
}

func (l *lineLog) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		l.line(l.buf[:i])
		l.buf = append(l.buf[:0], l.buf[i+1:]...)
	}
	if len(l.buf) > maxLogLine {
		// A line without an end: log what there is rather than buffering it.
		l.line(l.buf)
		l.buf = l.buf[:0]
	}
	return len(p), nil
}

func (l *lineLog) line(b []byte) {
	s := strings.TrimRight(string(b), "\r")
	if len(s) > maxLogLine {
		s = s[:maxLogLine] + "..."
	}
	s = strings.ToValidUTF8(s, "?")
	l.log.Info(l.name, "line", s)
	l.tail = append(l.tail, s)
	if len(l.tail) > 20 {
		l.tail = l.tail[1:]
	}
}

func (l *lineLog) flush() {
	if len(l.buf) > 0 {
		l.line(l.buf)
		l.buf = nil
	}
}

func (l *lineLog) tailText() string { return strings.Join(l.tail, " | ") }
