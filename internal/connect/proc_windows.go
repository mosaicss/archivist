//go:build windows

package connect

import (
	"os/exec"
	"syscall"
)

// Windows needs a Job Object to contain a harness tree; until that exists
// archivist connect refuses to run there.

const (
	sigTerm = syscall.Signal(15)
	sigKill = syscall.Signal(9)
)

// Supported reports whether harness supervision works on this platform.
func Supported() bool { return false }

func setProcessGroup(cmd *exec.Cmd) error { return ErrUnsupportedPlatform }

func signalGroup(pgid int, sig syscall.Signal) {}

func groupAlive(pgid int) bool { return false }

func killPID(pid int) {}

func descendants(root int) []int { return nil }
