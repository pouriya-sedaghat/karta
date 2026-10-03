//go:build unix

package importer

import (
	"errors"
	"os/exec"
	"syscall"
)

// isolate starts the command in a process group of its own and makes
// cancellation stop the whole group: osm2pgsql's helper processes, or the
// children of a wrapper script, are stopped with it instead of being left
// running (and holding the output pipe) after the direct child is killed.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
}

// killGroup sends SIGKILL to the command's process group. A group without
// members (everything already exited) is not an error.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
