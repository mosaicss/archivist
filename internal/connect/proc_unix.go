//go:build !windows

package connect

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

const (
	sigTerm = syscall.SIGTERM
	sigKill = syscall.SIGKILL
)

// Supported reports whether harness supervision works on this platform.
func Supported() bool { return true }

func setProcessGroup(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

func signalGroup(pgid int, sig syscall.Signal) {
	_ = syscall.Kill(-pgid, sig)
}

// groupAlive reports whether any process remains in group pgid.
func groupAlive(pgid int) bool {
	return syscall.Kill(-pgid, 0) == nil
}

func killPID(pid int) {
	if pid > 1 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// descendants lists every live descendant of root (any process group), via
// ps so it works on Linux and macOS. Errors yield nil.
func descendants(root int) []int {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	for _, line := range bytes.Split(out, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			children[ppid] = append(children[ppid], pid)
		}
	}
	var res []int
	queue := []int{root}
	seen := map[int]bool{root: true}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, c := range children[n] {
			if !seen[c] {
				seen[c] = true
				res = append(res, c)
				queue = append(queue, c)
			}
		}
	}
	return res
}
