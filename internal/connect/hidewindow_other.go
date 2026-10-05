//go:build !windows

package connect

import "os/exec"

// hideWindow is a no-op where processes have no console windows.
func hideWindow(*exec.Cmd) {}
