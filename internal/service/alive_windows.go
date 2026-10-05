//go:build windows

package service

import "golang.org/x/sys/windows"

// stillActive is STILL_ACTIVE, the exit code of a running process.
const stillActive = 259

// processAlive opens the process for a limited query and reads its exit
// code (Signal(0) does not exist on Windows). A process this user may not
// query (another user's, reusing the id) is not ours.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
