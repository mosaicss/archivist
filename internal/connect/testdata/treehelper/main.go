//go:build windows

// treehelper is a Windows test harness stand-in (Story 78.34): run as
// `treehelper <out>` it starts a plain grandchild and one in a new process
// group, tries a CREATE_BREAKAWAY_FROM_JOB grandchild, writes "pid <n>"
// lines and the breakaway outcome to <out>, then sleeps; run as
// `treehelper sleep` it only sleeps.
package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] == "sleep" {
		time.Sleep(5 * time.Minute)
		return
	}
	out := os.Args[1]
	lines := []string{"pid " + strconv.Itoa(os.Getpid())}
	start := func(flags uint32) (*exec.Cmd, error) {
		c := exec.Command(os.Args[0], "sleep")
		c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
		return c, c.Start()
	}
	if c, err := start(0); err == nil {
		lines = append(lines, "pid "+strconv.Itoa(c.Process.Pid))
	}
	if c, err := start(windows.CREATE_NEW_PROCESS_GROUP); err == nil {
		lines = append(lines, "pid "+strconv.Itoa(c.Process.Pid))
	}
	if c, err := start(windows.CREATE_BREAKAWAY_FROM_JOB); err != nil {
		lines = append(lines, "breakaway denied")
	} else {
		lines = append(lines, "breakaway started", "pid "+strconv.Itoa(c.Process.Pid))
	}
	tmp := out + ".tmp"
	_ = os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	_ = os.Rename(tmp, out)
	time.Sleep(5 * time.Minute)
}
