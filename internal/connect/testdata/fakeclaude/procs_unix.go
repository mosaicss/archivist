//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// helperMode runs a helper process role (Windows only).
func helperMode() bool { return false }

// detachedSleeper is `sleep 300` in its own session (setsid).
func detachedSleeper() *exec.Cmd {
	c := exec.Command("sleep", "300")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return c
}

// groupSleeper is `sleep 300` leading its own process group.
func groupSleeper() *exec.Cmd {
	c := exec.Command("sleep", "300")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return c
}

// spawnOrphan starts a setsid `sleep <seconds>` whose parent shell exits at
// once, writing its pid to pidFile.
func spawnOrphan(pidFile, seconds string) {
	_ = exec.Command("sh", "-c", "setsid sleep "+seconds+" </dev/null >/dev/null 2>&1 & echo $! > "+pidFile).Run()
}

func killPID(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }
