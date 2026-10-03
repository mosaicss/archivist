//go:build linux

package connect

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// enableSubreaper makes the daemon the reaper of orphaned descendants
// (PR_SET_CHILD_SUBREAPER): a command Codex detaches with setsid, whose
// parent then exits, is reparented to the daemon instead of init, so the
// stop sweep can still find, kill and reap it.
func enableSubreaper() error {
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}

// psEntry is one row of a full process table snapshot.
type psEntry struct {
	pid, ppid, pgid int
	stat            string
	id              procID
}

// psTable snapshots every process with its parent, group, state and start
// time (the procID identity used against pid reuse). Errors yield nil.
func psTable() []psEntry {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,pgid=,stat=,lstart=").Output()
	if err != nil {
		return nil
	}
	var res []psEntry
	for _, line := range bytes.Split(out, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) < 5 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		pgid, err3 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		res = append(res, psEntry{pid: pid, ppid: ppid, pgid: pgid, stat: f[3],
			id: procID{pid: pid, start: strings.Join(f[4:], " ")}})
	}
	return res
}

// orphanSweepBudget bounds one sweep (kill, wait for exit, reap).
const orphanSweepBudget = 3 * time.Second

// sweepOrphans kills and reaps processes reparented to the daemon that
// belong to owner: tracked descendants, members of owner's process group,
// or any orphan when owner is the only live harness (or all is set, at
// daemon exit). Only children of the daemon outside its own process group
// and outside the harness registry qualify, so os/exec's own children
// (harnesses, detection commands, ps) are never killed or reaped here.
func sweepOrphans(owner *Proc, all bool) {
	self := os.Getpid()
	selfPG := syscall.Getpgrp()
	procRegistry.mu.Lock()
	defer procRegistry.mu.Unlock()
	takeAll := all
	if owner != nil && !takeAll {
		takeAll = true
		for pid := range procRegistry.live {
			if pid != owner.PID() {
				takeAll = false
				break
			}
		}
	}
	deadline := time.Now().Add(orphanSweepBudget)
	for {
		table := psTable()
		children := map[int][]psEntry{}
		for _, e := range table {
			children[e.ppid] = append(children[e.ppid], e)
		}
		var roots []psEntry
		for _, e := range children[self] {
			if e.pgid == selfPG || procRegistry.live[e.pid] != nil {
				continue
			}
			if strings.HasPrefix(e.stat, "Z") {
				// A dead orphan: reaping it harms no session, whoever owned it.
				var ws syscall.WaitStatus
				_, _ = syscall.Wait4(e.pid, &ws, syscall.WNOHANG, nil)
				continue
			}
			if !takeAll && !owner.owns(e) {
				continue
			}
			roots = append(roots, e)
		}
		if len(roots) == 0 {
			return
		}
		for _, r := range roots {
			for _, e := range subtree(r, children) {
				if owner != nil {
					owner.track([]procID{e.id})
				}
				if strings.HasPrefix(e.stat, "Z") {
					continue
				}
				if e.ppid == self {
					// Our child: nobody else can reap it, so the pid is still its own.
					_ = syscall.Kill(e.pid, syscall.SIGKILL)
				} else {
					killPID(e.id)
				}
			}
		}
		for _, r := range roots {
			var ws syscall.WaitStatus
			_, _ = syscall.Wait4(r.pid, &ws, syscall.WNOHANG, nil)
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// owns reports whether an orphan is attributable to p: a descendant seen
// in a snapshot, or a member of p's process group or of a group led by a
// process p tracked.
func (p *Proc) owns(e psEntry) bool {
	return p != nil && (p.isTracked(e.id) || p.ownsGroup(e.pgid))
}

// subtree returns root and every descendant in the snapshot.
func subtree(root psEntry, children map[int][]psEntry) []psEntry {
	out := []psEntry{root}
	seen := map[int]bool{root.pid: true}
	for i := 0; i < len(out); i++ {
		for _, c := range children[out[i].pid] {
			if !seen[c.pid] {
				seen[c.pid] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// SweepAllOrphans kills and reaps every orphan reparented to the daemon
// (daemon exit, after every session stopped).
func SweepAllOrphans() { sweepOrphans(nil, true) }

// reapZombies reaps dead orphans adopted by the daemon (outside its own
// process group and the harness registry), whoever they belonged to.
func reapZombies() {
	self, selfPG := os.Getpid(), syscall.Getpgrp()
	procRegistry.mu.Lock()
	defer procRegistry.mu.Unlock()
	for _, e := range psTable() {
		if e.ppid == self && e.pgid != selfPG && procRegistry.live[e.pid] == nil && strings.HasPrefix(e.stat, "Z") {
			var ws syscall.WaitStatus
			_, _ = syscall.Wait4(e.pid, &ws, syscall.WNOHANG, nil)
		}
	}
}
