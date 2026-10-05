//go:build windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Story 78.34: Windows stand-ins for the Unix sleep, setsid and sh helpers.
// The fake re-runs itself in a helper role chosen by environment:
// FAKE_SLEEP=<seconds> sleeps; FAKE_ORPHAN_PIDFILE=<path> starts a sleeper
// (FAKE_ORPHAN_SECONDS), writes its pid and exits at once, leaving it an
// orphan.
func helperMode() bool {
	if s := os.Getenv("FAKE_SLEEP"); s != "" {
		f, _ := strconv.ParseFloat(s, 64)
		time.Sleep(time.Duration(f * float64(time.Second)))
		return true
	}
	if p := os.Getenv("FAKE_ORPHAN_PIDFILE"); p != "" {
		c := sleeper(os.Getenv("FAKE_ORPHAN_SECONDS"), windows.CREATE_NEW_PROCESS_GROUP)
		if c.Start() == nil {
			_ = os.WriteFile(p, []byte(strconv.Itoa(c.Process.Pid)), 0o600)
		}
		return true
	}
	return false
}

func sleeper(seconds string, flags uint32) *exec.Cmd {
	c := exec.Command(os.Args[0])
	c.Env = append(cleanEnv(), "FAKE_SLEEP="+seconds)
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
	return c
}

// cleanEnv is this environment without the helper role keys.
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if len(kv) >= 5 && kv[:5] == "FAKE_" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// detachedSleeper is a 300 s sleeper in a new process group (the nearest
// Windows analogue of setsid).
func detachedSleeper() *exec.Cmd { return sleeper("300", windows.CREATE_NEW_PROCESS_GROUP) }

// groupSleeper is a 300 s sleeper leading its own process group.
func groupSleeper() *exec.Cmd { return sleeper("300", windows.CREATE_NEW_PROCESS_GROUP) }

// spawnOrphan starts a sleeper through an intermediate that exits at once.
func spawnOrphan(pidFile, seconds string) {
	c := exec.Command(os.Args[0])
	c.Env = append(cleanEnv(), "FAKE_ORPHAN_PIDFILE="+pidFile, "FAKE_ORPHAN_SECONDS="+seconds)
	_ = c.Run()
}

func killPID(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
