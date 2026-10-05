//go:build windows

package connect

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/windows"
)

// processAlive reports whether pid names a running process (its exit code
// is STILL_ACTIVE).
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == 259
}

// processCommandLines is "pid command line" for every process (CIM via
// PowerShell, Windows has no ps), without the query itself.
func processCommandLines() []string {
	out, _ := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Get-CimInstance Win32_Process | ForEach-Object { "$($_.ProcessId) $($_.CommandLine)" }`).Output()
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.Contains(line, "Get-CimInstance") {
			lines = append(lines, line)
		}
	}
	return lines
}

// linkedToOwner checks that the session auth.json is a hard link to the
// owner's (one file identity, Story 78.34).
func linkedToOwner(link, ownerAuth string) error {
	a, err := os.Stat(link)
	if err != nil {
		return err
	}
	b, err := os.Stat(ownerAuth)
	if err != nil {
		return err
	}
	if !os.SameFile(a, b) {
		return fmt.Errorf("%s is not the owner's file", link)
	}
	return nil
}
