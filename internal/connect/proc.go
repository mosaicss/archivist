package connect

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// maxLine bounds one stdout frame from a harness (stream-json lines carry
// whole tool results).
const maxLine = 32 << 20

// ProcSpec describes one harness child. Bin and Args are fixed by the
// adapter; nothing in them comes from the relay.
type ProcSpec struct {
	Bin  string
	Args []string
	Env  []string
	Dir  string
	Log  *Logger
}

// Proc is a supervised harness child in its own process group.
type Proc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	log   *Logger

	wmu    sync.Mutex
	lines  chan []byte
	quit   chan struct{}
	qOnce  sync.Once
	done   chan struct{}
	exitMu sync.Mutex
	exit   error
	stopMu sync.Mutex
}

// ErrUnsupportedPlatform is returned where process groups are unavailable.
var ErrUnsupportedPlatform = errors.New("archivist connect does not supervise harness processes on this platform yet")

// StartProc spawns the child with its own process group, drains stderr on
// its own goroutine (scrubbed into the log) and reads stdout lines.
func StartProc(spec ProcSpec) (*Proc, error) {
	cmd := exec.Command(spec.Bin, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.Dir
	if err := setProcessGroup(cmd); err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Proc{cmd: cmd, stdin: stdin, log: spec.Log, lines: make(chan []byte, 64),
		quit: make(chan struct{}), done: make(chan struct{})}

	var drain sync.WaitGroup
	drain.Add(2)
	go func() {
		defer drain.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		logged := 0
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			if logged < 200 {
				p.log.Printf("harness stderr: %s", truncateString(line, 2000))
				logged++
			}
		}
		_, _ = io.Copy(io.Discard, stderr)
	}()
	go func() {
		defer drain.Done()
		defer close(p.lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1<<20), maxLine)
		for sc.Scan() {
			b := sc.Bytes()
			if len(strings.TrimSpace(string(b))) == 0 {
				continue
			}
			line := append([]byte(nil), b...)
			select {
			case p.lines <- line:
			case <-p.quit:
			}
		}
		if err := sc.Err(); err != nil {
			p.log.Printf("harness stdout reader stopped: %v", err)
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	go func() {
		drain.Wait()
		err := cmd.Wait()
		p.exitMu.Lock()
		p.exit = err
		p.exitMu.Unlock()
		close(p.done)
	}()
	return p, nil
}

// PID is the child pid, which is also its process group id.
func (p *Proc) PID() int { return p.cmd.Process.Pid }

// Lines yields stdout frames until EOF.
func (p *Proc) Lines() <-chan []byte { return p.lines }

// Done is closed after the child exits and its pipes are drained.
func (p *Proc) Done() <-chan struct{} { return p.done }

// ExitErr is the wait error once Done is closed.
func (p *Proc) ExitErr() error {
	p.exitMu.Lock()
	defer p.exitMu.Unlock()
	return p.exit
}

// WriteJSON writes v as one stdin line.
func (p *Proc) WriteJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// Stop closes stdin, waits grace for a clean exit, then SIGTERMs and
// SIGKILLs the group and any descendant that left it. Safe to call twice.
func (p *Proc) Stop(grace time.Duration) {
	p.stopMu.Lock()
	defer p.stopMu.Unlock()
	tracked := descendants(p.PID())
	p.qOnce.Do(func() { close(p.quit) })
	p.wmu.Lock()
	_ = p.stdin.Close()
	p.wmu.Unlock()
	select {
	case <-p.done:
	case <-time.After(grace):
	}
	p.killGroup(tracked)
}

// Kill skips the stdin grace period (interrupt timeout, failed proof).
func (p *Proc) Kill() {
	p.stopMu.Lock()
	defer p.stopMu.Unlock()
	tracked := descendants(p.PID())
	p.qOnce.Do(func() { close(p.quit) })
	p.killGroup(tracked)
}

func (p *Proc) killGroup(tracked []int) {
	pgid := p.PID()
	tracked = append(tracked, descendants(pgid)...)
	if groupAlive(pgid) {
		signalGroup(pgid, sigTerm)
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
		}
	}
	if groupAlive(pgid) {
		signalGroup(pgid, sigKill)
	}
	for _, pid := range tracked {
		killPID(pid)
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		p.log.Printf("harness pid %d did not report exit after SIGKILL", pgid)
	}
}

func (p *Proc) String() string { return fmt.Sprintf("pid %d", p.PID()) }
