//go:build unix

package importer

import (
	"os/exec"
	"syscall"
)

// childMaxRSS returns the peak resident set size of a finished child in KiB.
func childMaxRSS(cmd *exec.Cmd) int64 {
	if cmd.ProcessState == nil {
		return 0
	}
	if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		return ru.Maxrss // KiB on Linux
	}
	return 0
}

// selfMaxRSS returns this process's peak resident set size in KiB.
func selfMaxRSS() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return ru.Maxrss
}
