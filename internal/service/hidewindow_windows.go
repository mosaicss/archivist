//go:build windows

package service

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// hideWindow starts cmd without a console window (CREATE_NO_WINDOW).
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
	cmd.SysProcAttr.HideWindow = true
}
