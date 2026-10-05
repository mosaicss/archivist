//go:build windows

package winjob

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The test binary doubles as the helper processes, selected by
// WINJOB_HELPER: "sleep" sleeps; "tree" starts a plain grandchild and one in
// a new process group, tries a breakaway, records the pids and the
// breakaway outcome in WINJOB_OUT, then sleeps; "daemon" contains a "tree"
// child in its own job (as archivist connect does), records that child's
// pid file path, then sleeps.
func TestMain(m *testing.M) {
	switch os.Getenv("WINJOB_HELPER") {
	case "sleep":
		time.Sleep(5 * time.Minute)
		os.Exit(0)
	case "tree":
		helperTree(os.Getenv("WINJOB_OUT"))
		os.Exit(0)
	case "daemon":
		helperDaemon(os.Getenv("WINJOB_OUT"))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helperCmd(role, out string) *exec.Cmd {
	c := exec.Command(os.Args[0], "-test.run=^$")
	c.Env = append(os.Environ(), "WINJOB_HELPER="+role, "WINJOB_OUT="+out)
	return c
}

func helperTree(out string) {
	var lines []string
	plain := helperCmd("sleep", "")
	if err := plain.Start(); err == nil {
		lines = append(lines, "pid "+strconv.Itoa(plain.Process.Pid))
	}
	group := helperCmd("sleep", "")
	group.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	if err := group.Start(); err == nil {
		lines = append(lines, "pid "+strconv.Itoa(group.Process.Pid))
	}
	away := helperCmd("sleep", "")
	away.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB}
	if err := away.Start(); err != nil {
		lines = append(lines, "breakaway denied")
	} else {
		lines = append(lines, "breakaway started", "pid "+strconv.Itoa(away.Process.Pid))
	}
	writeLines(out, lines)
	time.Sleep(5 * time.Minute)
}

func helperDaemon(out string) {
	child := helperCmd("tree", out+".tree")
	if _, err := Start(child); err != nil {
		writeLines(out, []string{"error " + err.Error()})
		os.Exit(1)
	}
	writeLines(out, []string{"pid " + strconv.Itoa(child.Process.Pid)})
	time.Sleep(5 * time.Minute)
}

func writeLines(path string, lines []string) {
	tmp := path + ".tmp"
	_ = os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	_ = os.Rename(tmp, path)
}

// readLines waits until path exists and returns its lines.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			return strings.Split(strings.TrimSpace(string(b)), "\n")
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never written", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func pidsOf(t *testing.T, lines []string) []int {
	t.Helper()
	var pids []int
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, "pid "); ok {
			pid, err := strconv.Atoi(v)
			if err != nil {
				t.Fatal(err)
			}
			pids = append(pids, pid)
		}
	}
	return pids
}

// running reports whether pid names a live process.
func running(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == 259
}

func waitGone(t *testing.T, budget time.Duration, pids ...int) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		var left []int
		for _, p := range pids {
			if running(p) {
				left = append(left, p)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes still running after %s: %v", budget, left)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func startTree(t *testing.T) (*exec.Cmd, *Job, []int) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "tree")
	c := helperCmd("tree", out)
	j, err := Start(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = j.Terminate(1)
		_ = j.Close()
		_ = c.Wait()
	})
	lines := readLines(t, out)
	if !contains(lines, "breakaway denied") {
		t.Fatalf("a breakaway grandchild escaped the job: %v", lines)
	}
	pids := append([]int{c.Process.Pid}, pidsOf(t, lines)...)
	if len(pids) != 3 {
		t.Fatalf("want the child and two grandchildren, got %v (%v)", pids, lines)
	}
	if n, err := j.ActiveProcesses(); err != nil || n < 3 {
		t.Fatalf("active processes %d, %v", n, err)
	}
	return c, j, pids
}

func contains(lines []string, s string) bool {
	for _, l := range lines {
		if l == s {
			return true
		}
	}
	return false
}

// Terminate ends the child, a plain grandchild and one in a new process
// group; a breakaway attempt fails inside the job.
func TestTerminateKillsWholeTree(t *testing.T) {
	_, j, pids := startTree(t)
	if err := j.Terminate(1); err != nil {
		t.Fatal(err)
	}
	waitGone(t, 5*time.Second, pids...)
	if n, err := j.ActiveProcesses(); err != nil || n != 0 {
		t.Fatalf("active processes after terminate %d, %v", n, err)
	}
}

// Closing the job handle (kill on close) ends the tree too.
func TestCloseKillsWholeTree(t *testing.T) {
	_, j, pids := startTree(t)
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	waitGone(t, 5*time.Second, pids...)
	if err := j.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if n, _ := j.ActiveProcesses(); n != 0 {
		t.Fatalf("a closed job reports %d processes", n)
	}
}

// A daemon terminated outright (crash, task end) closes its job handles,
// and every tree it contained dies.
func TestDaemonDeathKillsContainedTrees(t *testing.T) {
	out := filepath.Join(t.TempDir(), "daemon")
	d := helperCmd("daemon", out)
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Process.Kill(); _ = d.Wait() })
	lines := readLines(t, out)
	child := pidsOf(t, lines)
	if len(child) != 1 {
		t.Fatalf("daemon output %v", lines)
	}
	tree := pidsOf(t, readLines(t, out+".tree"))
	pids := append(child, tree...)
	if len(pids) != 3 {
		t.Fatalf("tree %v", pids)
	}
	if err := d.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = d.Wait()
	waitGone(t, 5*time.Second, pids...)
}

// An assign failure kills the child before it ever runs and fails Start.
func TestAssignFailureKillsChild(t *testing.T) {
	prev := assignProcess
	assignProcess = func(windows.Handle, windows.Handle) error { return errors.New("refused for the test") }
	t.Cleanup(func() { assignProcess = prev })
	c := helperCmd("sleep", "")
	if _, err := Start(c); err == nil || !strings.Contains(err.Error(), "refused for the test") {
		t.Fatalf("err %v", err)
	}
	if c.ProcessState == nil {
		t.Fatal("the child was not reaped")
	}
	if running(c.Process.Pid) {
		t.Fatal("the child survived a failed assign")
	}
}

// A resume failure kills the (assigned) child too.
func TestResumeFailureKillsChild(t *testing.T) {
	prev := resumeProcess
	resumeProcess = func(uint32) error { return fmt.Errorf("no resume for the test") }
	t.Cleanup(func() { resumeProcess = prev })
	c := helperCmd("sleep", "")
	if _, err := Start(c); err == nil || !strings.Contains(err.Error(), "no resume for the test") {
		t.Fatalf("err %v", err)
	}
	if c.ProcessState == nil || running(c.Process.Pid) {
		t.Fatal("the child survived a failed resume")
	}
}

// The contained child really runs (it was resumed) and exits normally.
func TestStartResumesChild(t *testing.T) {
	c := exec.Command(os.Getenv("ComSpec"), "/c", "exit", "7")
	j, err := Start(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 7 {
			t.Fatalf("exit %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the child never ran (still suspended?)")
	}
}
