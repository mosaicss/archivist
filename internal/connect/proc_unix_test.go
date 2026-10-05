//go:build !windows

package connect

import (
	"os/exec"
	"testing"
	"time"
)

// (f) killPID leaves a live process alone when the start time differs.
func TestKillPIDChecksStartTime(t *testing.T) {
	c := exec.Command("sleep", "30")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	t.Cleanup(func() { _ = c.Process.Kill(); <-done })
	pid := c.Process.Pid
	start := startTime(pid)
	if start == "" {
		t.Fatal("no start time")
	}
	killPID(procID{pid: pid, start: "Thu Jan  1 00:00:00 1970"})
	killPID(procID{pid: pid, start: ""})
	time.Sleep(200 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("a process with another start time was killed")
	default:
	}
	killPID(procID{pid: pid, start: start})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("matching start time not killed")
	}
}
