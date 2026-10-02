//go:build !unix

package importer

import "os/exec"

// isolate keeps the default cancellation (killing the direct child): there
// are no process groups to stop.
func isolate(*exec.Cmd) {}

func killGroup(*exec.Cmd) error { return nil }
