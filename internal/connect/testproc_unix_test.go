//go:build !windows

package connect

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// processAlive reports whether pid exists (signal 0).
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// processCommandLines is "pid args" for every process (ps), without the
// ps call itself.
func processCommandLines() []string {
	out, _ := exec.Command("ps", "-A", "-o", "pid=,args=").Output()
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if line != "" && !strings.Contains(line, "ps -A") {
			lines = append(lines, line)
		}
	}
	return lines
}

// linkedToOwner checks that the session auth.json is a symlink to the
// owner's.
func linkedToOwner(link, ownerAuth string) error {
	target, err := os.Readlink(link)
	if err != nil {
		return err
	}
	if target != ownerAuth {
		return fmt.Errorf("links to %q", target)
	}
	return nil
}
