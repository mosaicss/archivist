//go:build windows

package connect

import (
	"os/exec"
	"syscall"
	"time"

	"github.com/mosaicss/archivist/internal/winjob"
)

// Story 78.34: each harness child runs in its own Windows Job Object. It
// starts suspended (CREATE_SUSPENDED|CREATE_NO_WINDOW|CREATE_NEW_PROCESS_GROUP),
// is assigned to a kill on close job with no breakaway, and only then
// resumes, so its whole tree (new process groups and breakaway attempts
// included) stays in the job. Stop, kill, a failed proof and the overflow
// path terminate the job; the job handle closes after the child exits and,
// because it is never inherited, also when the daemon exits or crashes.
// Descendant snapshots and orphan sweeps are Unix mechanisms and no-ops here.

// Signal stand-ins: the job has one way to end its tree (terminate).
const (
	sigTerm = syscall.Signal(15)
	sigKill = syscall.Signal(9)
)

// Supported reports whether harness supervision works on this platform.
func Supported() bool { return true }

// setProcessGroup sets the contained start flags; the job is attached right
// after Start (attachProc).
func setProcessGroup(cmd *exec.Cmd) error {
	winjob.Prepare(cmd)
	return nil
}

// containProc puts a started, suspended child into its job and resumes it;
// a failure kills the child. A var so tests can make it fail.
var containProc = winjob.Contain

// procTree is the child's job.
type procTree struct{ job *winjob.Job }

// attachProc contains a child StartProc just started.
func attachProc(cmd *exec.Cmd) (procTree, error) {
	job, err := containProc(cmd)
	if err != nil {
		return procTree{}, err
	}
	return procTree{job: job}, nil
}

// alive reports whether any process remains in the job (a failed query
// counts as alive, so the stop path still terminates the job).
func (t procTree) alive(int) bool {
	if t.job == nil {
		return false
	}
	n, err := t.job.ActiveProcesses()
	return err != nil || n > 0
}

// signal terminates every process in the job: Windows has no group signal
// a console-less child would receive, and the stop grace (stdin closed,
// then a wait) already ran before the stop path gets here. TerminateJobObject
// returns before the processes are gone, so it then waits (bounded) until
// the job has no active process: like the Unix kill and reap, the tree is
// gone, and with it every handle it held (the session cwd included), before
// the stop path removes the session's files.
func (t procTree) signal(int, syscall.Signal) {
	if t.job == nil {
		return
	}
	_ = t.job.Terminate(1)
	deadline := time.Now().Add(treeGoneBudget)
	for time.Now().Before(deadline) {
		if n, err := t.job.ActiveProcesses(); err != nil || n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// treeGoneBudget bounds the wait for a terminated job to empty.
const treeGoneBudget = 5 * time.Second

// release ends any descendant still in the job once the child has exited
// (terminated and awaited like signal), then closes the job handle.
func (t procTree) release() {
	if t.job == nil {
		return
	}
	if t.alive(0) {
		t.signal(0, sigKill)
	}
	_ = t.job.Close()
}

// signalGroup and groupAlive serve the PTY sign-in child, which cannot
// start on Windows (no PTY), so they never act.
func signalGroup(int, syscall.Signal) {}

func groupAlive(int) bool { return false }

// procID names a process; Windows keeps no descendant snapshots, so it
// carries nothing.
type procID struct{}

func killPID(procID) {}

func descendants(int) []procID { return nil }

func liveIDs() map[procID]bool { return nil }
