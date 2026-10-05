//go:build windows

package connect

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// hideWindow starts cmd without a console window (CREATE_NO_WINDOW): the
// background daemon runs as the console-less archivistw.exe, and a console
// child started from it would otherwise open a visible console or Terminal
// window (Story 78.34).
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
	cmd.SysProcAttr.HideWindow = true
}
