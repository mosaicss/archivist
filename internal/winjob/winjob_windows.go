//go:build windows

package winjob

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CreationFlags start a contained child: suspended until it is in its job,
// without a console window (its console children share that hidden
// console), and in its own process group. Never DETACHED_PROCESS (each
// console grandchild would open a visible console) and never
// CREATE_BREAKAWAY_FROM_JOB.
const CreationFlags = windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP

// Prepare sets CreationFlags on cmd before Start, keeping its other
// SysProcAttr fields.
func Prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= CreationFlags
	cmd.SysProcAttr.HideWindow = true
}

// Job is one kill on close job object. Its methods are safe for concurrent
// use and become no-ops once it is closed.
type Job struct {
	mu     sync.Mutex
	h      windows.Handle
	closed bool
}

// basicAccounting is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION (x/sys does not
// define it): four LARGE_INTEGER times, then four DWORD counters.
type basicAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// Test seams: production values never change at runtime.
var (
	assignProcess = windows.AssignProcessToJobObject
	resumeProcess = resumeThreads
)

// New creates an unnamed job (nil security attributes: the handle is not
// inheritable) whose only limit is JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE: no
// breakaway flag, so a descendant asking for CREATE_BREAKAWAY_FROM_JOB is
// refused.
func New() (*Job, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("set the job object limits: %w", err)
	}
	return &Job{h: h}, nil
}

// Contain puts cmd, started with Prepare's flags (so still suspended), into
// a new job and resumes it. On any failure the child, which never ran, is
// killed and the error returned; the caller still calls cmd.Wait.
func Contain(cmd *exec.Cmd) (*Job, error) {
	if cmd.Process == nil {
		return nil, errors.New("winjob: the process was not started")
	}
	j, err := New()
	if err == nil {
		var aerr error
		if werr := cmd.Process.WithHandle(func(h uintptr) { aerr = assignProcess(j.h, windows.Handle(h)) }); werr != nil {
			aerr = werr
		}
		if aerr != nil {
			err = fmt.Errorf("assign the process to its job object: %w", aerr)
		}
	}
	if err == nil {
		if rerr := resumeProcess(uint32(cmd.Process.Pid)); rerr != nil {
			err = fmt.Errorf("resume the process: %w", rerr)
		}
	}
	if err != nil {
		if j != nil {
			_ = j.Terminate(1)
			_ = j.Close()
		}
		_ = cmd.Process.Kill()
		return nil, err
	}
	return j, nil
}

// Start is Prepare, cmd.Start and Contain. On a containment failure the
// child is killed and reaped (cmd.Wait) before the error returns.
func Start(cmd *exec.Cmd) (*Job, error) {
	Prepare(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	j, err := Contain(cmd)
	if err != nil {
		_ = cmd.Wait()
		return nil, err
	}
	return j, nil
}

// resumeThreads resumes every thread of a process started suspended. The
// primary thread handle CreateProcess returned is closed by os/exec, so
// the threads are found through a toolhelp snapshot.
func resumeThreads(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("thread snapshot: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	resumed := 0
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, oerr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if oerr != nil {
			return fmt.Errorf("open thread %d: %w", te.ThreadID, oerr)
		}
		_, rerr := windows.ResumeThread(th)
		_ = windows.CloseHandle(th)
		if rerr != nil {
			return fmt.Errorf("resume thread %d: %w", te.ThreadID, rerr)
		}
		resumed++
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("walk the thread snapshot: %w", err)
	}
	if resumed == 0 {
		return fmt.Errorf("no thread of process %d to resume", pid)
	}
	return nil
}

// Terminate ends every process in the job with exit code code.
func (j *Job) Terminate(code uint32) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	return windows.TerminateJobObject(j.h, code)
}

// ActiveProcesses is the number of live processes in the job (0 once closed).
func (j *Job) ActiveProcesses() (uint32, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return 0, nil
	}
	var info basicAccounting
	if err := windows.QueryInformationJobObject(j.h, windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return 0, err
	}
	return info.ActiveProcesses, nil
}

// Close closes the job handle, which kills whatever still runs in it. Safe
// to call twice.
func (j *Job) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	return windows.CloseHandle(j.h)
}
