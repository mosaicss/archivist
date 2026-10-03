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
// whole tool results). A var so tests can shorten it.
var maxLine = 32 << 20

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

	inMu     sync.Mutex
	inClosed bool

	trackMu sync.Mutex
	tracked map[procID]bool // every descendant seen (snapshots), for the stop sweep
}

// procRegistry holds the live harness children. A child is registered under
// the lock while it starts and leaves only after os/exec reaped it, so the
// orphan sweep never mistakes a harness for an orphan it may reap.
var procRegistry = struct {
	mu   sync.Mutex
	live map[int]*Proc
}{live: map[int]*Proc{}}

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
	procRegistry.mu.Lock()
	if err := cmd.Start(); err != nil {
		procRegistry.mu.Unlock()
		return nil, err
	}
	p := &Proc{cmd: cmd, stdin: stdin, log: spec.Log, lines: make(chan []byte, 64),
		quit: make(chan struct{}), done: make(chan struct{}), tracked: map[procID]bool{}}
	procRegistry.live[cmd.Process.Pid] = p
	procRegistry.mu.Unlock()

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
		sc.Buffer(make([]byte, 0, min(1<<20, maxLine)), maxLine)
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
			// A frame over maxLine (or a read error) ends the stream; the child
			// would otherwise run on unread, so end its group and let the exit
			// path settle the session.
			p.log.Printf("harness stdout reader stopped: %v; terminating claude", err)
			// Snapshot first: descendants in other groups could keep the pipe
			// open (Done would never close) and are reparented after the kill.
			tracked := descendants(p.PID())
			signalGroup(p.PID(), sigKill)
			for _, id := range tracked {
				killPID(id)
			}
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	go func() {
		drain.Wait()
		err := cmd.Wait()
		procRegistry.mu.Lock()
		delete(procRegistry.live, cmd.Process.Pid)
		procRegistry.mu.Unlock()
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

// CloseStdin closes the child's stdin (end of input); safe to call twice.
// Codex's app-server treats EOF as a graceful shutdown request.
func (p *Proc) CloseStdin() {
	p.inMu.Lock()
	defer p.inMu.Unlock()
	if p.inClosed {
		return
	}
	p.inClosed = true
	p.wmu.Lock()
	_ = p.stdin.Close()
	p.wmu.Unlock()
}

// Snapshot records every current descendant (any process group), so a
// later stop can kill those that left the tree or were orphaned. Without a
// subreaper (macOS) these snapshots are the only record of detached
// grandchildren.
func (p *Proc) Snapshot() {
	p.track(descendants(p.PID()))
}

func (p *Proc) track(ids []procID) {
	p.trackMu.Lock()
	defer p.trackMu.Unlock()
	for _, id := range ids {
		p.tracked[id] = true
	}
}

func (p *Proc) isTracked(id procID) bool {
	p.trackMu.Lock()
	defer p.trackMu.Unlock()
	return p.tracked[id]
}

func (p *Proc) trackedIDs() []procID {
	p.trackMu.Lock()
	defer p.trackMu.Unlock()
	out := make([]procID, 0, len(p.tracked))
	for id := range p.tracked {
		out = append(out, id)
	}
	return out
}

// SweepOrphans kills and reaps processes of this harness that were
// reparented to the daemon (Linux subreaper); a no-op elsewhere.
func (p *Proc) SweepOrphans() { sweepOrphans(p, false) }

// Stop closes stdin, waits grace for a clean exit, then SIGTERMs and
// SIGKILLs the group and any descendant that left it. Safe to call twice.
func (p *Proc) Stop(grace time.Duration) {
	p.stopMu.Lock()
	defer p.stopMu.Unlock()
	tracked := descendants(p.PID())
	p.qOnce.Do(func() { close(p.quit) })
	p.CloseStdin()
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

func (p *Proc) killGroup(tracked []procID) {
	pgid := p.PID()
	tracked = append(tracked, descendants(pgid)...)
	p.track(tracked)
	tracked = p.trackedIDs()
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
	for _, id := range tracked {
		killPID(id)
	}
	// Orphans reparented to the daemon (Linux subreaper) are killed and
	// reaped; tracked ones that died after the snapshot are reaped too.
	sweepOrphans(p, false)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		p.log.Printf("harness pid %d did not report exit after SIGKILL", pgid)
	}
}

func (p *Proc) String() string { return fmt.Sprintf("pid %d", p.PID()) }
