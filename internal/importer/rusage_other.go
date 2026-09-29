//go:build !unix

package importer

import "os/exec"

func childMaxRSS(*exec.Cmd) int64 { return 0 }

func selfMaxRSS() int64 { return 0 }
