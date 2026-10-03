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

// procID names a process by pid and start time, so a pid reused by an
// unrelated process after the snapshot is never signalled.
type procID struct {
	pid   int
	start string // ps lstart
}

// killPID SIGKILLs id only while that pid still has the recorded start time.
func killPID(id procID) {
	if id.pid <= 1 || id.start == "" {
		return
	}
	if startTime(id.pid) != id.start {
		return
	}
	_ = syscall.Kill(id.pid, syscall.SIGKILL)
}

// startTime is the process start time as ps prints lstart ("" if gone).
func startTime(pid int) string {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

// liveIDs is every current process identity (pid and start time) from one
// ps call; nil when ps fails.
func liveIDs() map[procID]bool {
	out, err := exec.Command("ps", "-A", "-o", "pid=,lstart=").Output()
	if err != nil {
		return nil
	}
	live := map[procID]bool{}
	for _, line := range bytes.Split(out, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) < 2 {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil {
			live[procID{pid: pid, start: strings.Join(f[1:], " ")}] = true
		}
	}
	return live
}

// descendants lists every live descendant of root (any process group) with
// its start time, via ps so it works on Linux and macOS. Errors yield nil.
func descendants(root int) []procID {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,lstart=").Output()
	if err != nil {
		return nil
	}
	children := map[int][]procID{}
	for _, line := range bytes.Split(out, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			children[ppid] = append(children[ppid], procID{pid: pid, start: strings.Join(f[2:], " ")})
		}
	}
	var res []procID
	queue := []int{root}
	seen := map[int]bool{root: true}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, c := range children[n] {
			if !seen[c.pid] {
				seen[c.pid] = true
				res = append(res, c)
				queue = append(queue, c.pid)
			}
		}
	}
	return res
}
