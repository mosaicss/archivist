//go:build !windows

package service

import (
	"os"
	"syscall"
)

// processAlive sends signal 0: nil means the process exists and is this
// user's (EPERM, another user's process reusing the id, is not ours).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
