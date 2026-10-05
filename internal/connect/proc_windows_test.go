//go:build windows

package connect

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mosaicss/archivist/internal/winjob"
	"golang.org/x/sys/windows"
)

// buildTreeHelper builds testdata/treehelper into a temp dir.
func buildTreeHelper(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "treehelper.exe")
	if out, err := exec.Command("go", "build", "-o", bin, "./testdata/treehelper").CombinedOutput(); err != nil {
		t.Fatalf("go build treehelper: %v\n%s", err, out)
	}
	return bin
}

func winRunning(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == 259
}

// treePIDs waits for the helper's report: every pid, and a refused breakaway.
func treePIDs(t *testing.T, out string) []int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		b, err := os.ReadFile(out)
		if err == nil {
			var pids []int
			text := string(b)
			if !strings.Contains(text, "breakaway denied") {
				t.Fatalf("a breakaway grandchild escaped the job:\n%s", text)
			}
			for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
				if v, ok := strings.CutPrefix(l, "pid "); ok {
					pid, _ := strconv.Atoi(v)
					pids = append(pids, pid)
				}
			}
			if len(pids) != 3 {
				t.Fatalf("want the harness and two grandchildren:\n%s", text)
			}
			return pids
		}
		if time.Now().After(deadline) {
			t.Fatal("the tree helper never reported")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func assertGone(t *testing.T, pids []int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		var left []int
		for _, p := range pids {
			if winRunning(p) {
				left = append(left, p)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still running after %s: %v", budget, left)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Kill and Stop end the harness, a grandchild and one in a new process
// group (the breakaway attempt fails inside the job), within the budget.
func TestWindowsProcJobKillsTree(t *testing.T) {
	bin := buildTreeHelper(t)
	for name, end := range map[string]func(*Proc){
		"kill": func(p *Proc) { p.Kill() },
		"stop": func(p *Proc) { p.Stop(200 * time.Millisecond) },
	} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "tree")
			p, err := StartProc(ProcSpec{Bin: bin, Args: []string{out}, Env: os.Environ(), Log: NewLogger(io.Discard)})
			if err != nil {
				t.Fatal(err)
			}
			pids := treePIDs(t, out)
			if !p.tree.alive(p.PID()) {
				t.Fatal("the job reports no live process")
			}
			start := time.Now()
			end(p)
			select {
			case <-p.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("Done not closed")
			}
			assertGone(t, pids, 5*time.Second)
			t.Logf("tree gone %s after the end call", time.Since(start))
			if p.tree.alive(p.PID()) {
				t.Fatal("a released job still reports processes")
			}
		})
	}
}

// The harness exiting on its own releases the job, which kills the
// grandchildren it left behind.
func TestWindowsProcExitReleasesJob(t *testing.T) {
	bin := buildTreeHelper(t)
	out := filepath.Join(t.TempDir(), "tree")
	p, err := StartProc(ProcSpec{Bin: bin, Args: []string{out}, Env: os.Environ(), Log: NewLogger(io.Discard)})
	if err != nil {
		t.Fatal(err)
	}
	pids := treePIDs(t, out)
	// Kill only the harness process itself, as a crash would.
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Its grandchildren hold no pipe, so the reader ends and Wait returns.
	select {
	case <-p.Done():
	case <-time.After(15 * time.Second):
		p.Kill()
		t.Fatal("Done not closed after the harness died")
	}
	assertGone(t, pids, 5*time.Second)
}

// A containment failure kills the child before it runs: StartProc fails
// and leaves nothing registered or running.
func TestWindowsProcContainFailure(t *testing.T) {
	bin := buildTreeHelper(t)
	prev := containProc
	var pid int
	containProc = func(cmd *exec.Cmd) (*winjob.Job, error) {
		pid = cmd.Process.Pid
		_ = cmd.Process.Kill()
		return nil, errors.New("assign refused for the test")
	}
	t.Cleanup(func() { containProc = prev })
	_, err := StartProc(ProcSpec{Bin: bin, Args: []string{"sleep"}, Env: os.Environ(), Log: NewLogger(io.Discard)})
	if err == nil || !strings.Contains(err.Error(), "assign refused") {
		t.Fatalf("err %v", err)
	}
	procRegistry.mu.Lock()
	registered := procRegistry.live[pid] != nil
	procRegistry.mu.Unlock()
	if registered || winRunning(pid) {
		t.Fatalf("child %d left registered=%v running=%v", pid, registered, winRunning(pid))
	}
}
