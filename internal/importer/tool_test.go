//go:build unix

package importer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// script writes an executable shell script and returns its path.
func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tool.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// readPID waits for a script to write a process id into a file.
func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pid in %s", path)
	return 0
}

// gone reports whether a process no longer runs: it does not exist, or it
// is a zombie waiting to be reaped by whatever adopted it (the init of the
// test environment may not reap).
func gone(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	// pid (comm) state ...: the state follows the last ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] == 'Z'
}

func waitGone(t *testing.T, what string, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !gone(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("%s (pid %d) still runs", what, pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func withGrace(t *testing.T, d time.Duration) {
	t.Helper()
	saved := outputGrace
	outputGrace = d
	t.Cleanup(func() { outputGrace = saved })
}

func TestToolChildHoldingOutputDoesNotDelayASuccessfulBuild(t *testing.T) {
	withGrace(t, 300*time.Millisecond)
	pidFile := filepath.Join(t.TempDir(), "child")
	// The tool succeeds, but leaves a child behind that keeps the output
	// pipe open for five minutes.
	bin := script(t, `sleep 300 &
echo $! > "$1"
echo "import finished"
exit 0
`)
	start := time.Now()
	_, tail, err := runTool(context.Background(), newTool(context.Background(), bin, pidFile), "tool", quietLog())
	if err != nil {
		t.Fatalf("a successful tool failed: %v", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("the build waited %s for the stray child", el)
	}
	if !strings.Contains(tail, "import finished") {
		t.Errorf("output not kept: %q", tail)
	}
	waitGone(t, "the child left behind", readPID(t, pidFile))
}

func TestToolDeadlineStopsTheWholeProcessGroup(t *testing.T) {
	withGrace(t, 2*time.Second)
	pidFile := filepath.Join(t.TempDir(), "child")
	bin := script(t, `sleep 300 &
echo $! > "$1"
echo "working"
sleep 300
`)
	errDeadline := errors.New("the test deadline")
	ctx, cancel := context.WithTimeoutCause(context.Background(), 500*time.Millisecond, errDeadline)
	defer cancel()
	cmd := newTool(ctx, bin, pidFile)
	start := time.Now()
	_, _, err := runTool(ctx, cmd, "tool", quietLog())
	if !errors.Is(err, errDeadline) {
		t.Fatalf("error %v does not carry the deadline's cause", err)
	}
	if el := time.Since(start); el > 4*time.Second {
		t.Fatalf("stopping took %s", el)
	}
	waitGone(t, "the tool", cmd.Process.Pid)
	waitGone(t, "its child", readPID(t, pidFile))
}

func TestToolCancellationIsReportedAsCancellation(t *testing.T) {
	withGrace(t, 2*time.Second)
	bin := script(t, "sleep 300\n")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := newTool(ctx, bin)
	time.AfterFunc(300*time.Millisecond, cancel)
	_, _, err := runTool(ctx, cmd, "tool", quietLog())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled (a shutdown, retried later)", err)
	}
	waitGone(t, "the tool", cmd.Process.Pid)
}

func TestToolDescendantOutsideTheGroupCannotHangTheBuild(t *testing.T) {
	setsid, err := exec.LookPath("setsid")
	if err != nil {
		t.Skip("setsid not installed")
	}
	withGrace(t, 500*time.Millisecond)
	pidFile := filepath.Join(t.TempDir(), "escaped")
	// The child moves to a session of its own (out of the tool's process
	// group, so the group kill misses it) and keeps the pipe open.
	bin := script(t, setsid+` sh -c 'echo $$ > "$1"; exec sleep 300' sh "$1" &
sleep 300
`)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = runTool(ctx, newTool(ctx, bin, pidFile), "tool", quietLog())
	pid := readPID(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("the escaped child held the build for %s", el)
	}
}

func TestToolFailureKeepsTheLastLines(t *testing.T) {
	bin := script(t, `i=0
while [ $i -lt 30 ]; do echo "line $i"; i=$((i+1)); done
echo "fatal: something broke" >&2
exit 3
`)
	_, tail, err := runTool(context.Background(), newTool(context.Background(), bin), "tool", quietLog())
	if err == nil || !strings.Contains(err.Error(), "tool failed") {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(tail, "fatal: something broke") || strings.Contains(tail, "line 5 ") || !strings.Contains(tail, "line 29") {
		t.Errorf("tail %q", tail)
	}
	if n := strings.Count(tail, " | ") + 1; n != 20 {
		t.Errorf("tail has %d lines, want the last 20", n)
	}
}

func TestLineLogSplitsAndBoundsLines(t *testing.T) {
	var buf bytes.Buffer
	l := &lineLog{log: slog.New(slog.NewTextHandler(&buf, nil)), name: "tool"}
	_, _ = l.Write([]byte("first li"))
	_, _ = l.Write([]byte("ne\r\nsecond\n"))
	_, _ = l.Write(bytes.Repeat([]byte("x"), maxLogLine+10)) // no newline: logged, not buffered without bound
	_, _ = l.Write([]byte("\xff tail"))
	l.flush()
	if len(l.tail) != 4 || l.tail[0] != "first line" || l.tail[1] != "second" || !strings.HasPrefix(l.tail[2], "xxx") ||
		len(l.tail[2]) != maxLogLine+len("...") || l.tail[3] != "? tail" {
		t.Fatalf("lines %q", l.tail)
	}
	_, _ = l.Write(bytes.Repeat([]byte("y"), 2*maxLogLine))
	l.flush()
	if got := l.tail[len(l.tail)-1]; len(got) != maxLogLine+len("...") {
		t.Errorf("an overlong line is not truncated: %d bytes", len(got))
	}
}
