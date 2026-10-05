// Command schtasks is the stub schtasks.exe scripts/test-install.ps1 puts
// first on PATH (Story 78.34). It logs every call to STUB_LOG and keeps the
// registration in STUB_STATE: /create registers (or fails with
// STUB_FAIL_CREATE=1), /query exits 0 only while registered, /run starts a
// sleeping stand-in daemon and writes its pid to the service.pid under
// USERPROFILE (as the real supervised daemon would), /end kills it,
// /delete unregisters. `schtasks sleep` is the stand-in itself.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "sleep" {
		time.Sleep(2 * time.Minute)
		return
	}
	logLine("schtasks " + strings.Join(args, " "))
	state := os.Getenv("STUB_STATE")
	pidFile := filepath.Join(os.Getenv("USERPROFILE"), ".archivist", "connect", "service.pid")
	if len(args) == 0 {
		os.Exit(2)
	}
	switch args[0] {
	case "/create":
		if os.Getenv("STUB_FAIL_CREATE") == "1" {
			fmt.Fprintln(os.Stderr, "ERROR: Access is denied. (stub refusal)")
			os.Exit(1)
		}
		_ = os.WriteFile(state, []byte("registered"), 0o600)
	case "/query":
		if _, err := os.Stat(state); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR: The system cannot find the file specified.")
			os.Exit(1)
		}
	case "/run":
		c := exec.Command(os.Args[0], "sleep")
		if err := c.Start(); err != nil {
			os.Exit(1)
		}
		_ = os.MkdirAll(filepath.Dir(pidFile), 0o700)
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(c.Process.Pid)+"\n"), 0o600)
		logLine("daemon " + strconv.Itoa(c.Process.Pid))
	case "/end":
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Kill()
				}
			}
		}
	case "/delete":
		_ = os.Remove(state)
	}
}

func logLine(s string) {
	f, err := os.OpenFile(os.Getenv("STUB_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintln(f, s)
}
