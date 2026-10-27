// Command schtasks is the stub schtasks.exe scripts/test-install.ps1 puts
// first on PATH (Story 78.34). It logs every call to STUB_LOG and keeps the
// registration in STUB_STATE: /create registers (or fails with
// STUB_FAIL_CREATE=1), /query exits 0 only while registered, /run starts a
// sleeping stand-in daemon and writes its pid to the service.pid under
// USERPROFILE (as the real supervised daemon would), /end kills it,
// /delete unregisters. `schtasks sleep` is the stand-in itself.
//
// Story 94.14: each task records a last result, which
// `/query /tn X /v /fo csv /nh` prints in CSV column 6 (signed decimal, as
// schtasks does; the other columns hold placeholders). A registered task
// starts as "has not run" (0x41303). The launch probe's tasks
// (MosaicArchivistProbe-*) keep their own state (STUB_STATE.probe, holding
// the <Command> of the XML they were created from): /run records
// STUB_PROBE_RESULT when it is set, else 0 when that file exists, else
// 0x80070002 (file not found), and starts nothing. The service task's /run
// with STUB_RUN_FAIL=<code> starts no daemon and records that code; without
// it a started daemon records 0x41301 (running).
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

const probePrefix = "MosaicArchivistProbe-"

const (
	resultRunning   = 0x41301
	resultHasNotRun = 0x41303
	resultNotFound  = 0x80070002
)

func main() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "sleep" {
		time.Sleep(2 * time.Minute)
		return
	}
	logLine("schtasks " + strings.Join(args, " "))
	if len(args) == 0 {
		os.Exit(2)
	}
	name := flagValue(args, "/tn")
	state := os.Getenv("STUB_STATE")
	probe := strings.HasPrefix(name, probePrefix)
	if probe {
		state += ".probe"
	}
	resultFile := state + ".result"
	pidFile := filepath.Join(os.Getenv("USERPROFILE"), ".archivist", "connect", "service.pid")
	switch args[0] {
	case "/create":
		if !probe && os.Getenv("STUB_FAIL_CREATE") == "1" {
			fmt.Fprintln(os.Stderr, "ERROR: Access is denied. (stub refusal)")
			os.Exit(1)
		}
		content := "registered"
		if probe {
			content = taskCommand(flagValue(args, "/xml"))
		}
		_ = os.WriteFile(state, []byte(content), 0o600)
		writeResult(resultFile, resultHasNotRun)
	case "/query":
		if _, err := os.Stat(state); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR: The system cannot find the file specified.")
			os.Exit(1)
		}
		if hasFlag(args, "/v") {
			result := resultHasNotRun
			if b, err := os.ReadFile(resultFile); err == nil {
				if v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32); err == nil {
					result = int(v)
				}
			}
			// schtasks prints a failure HRESULT as a negative decimal.
			fmt.Printf("\"STUBPC\",\"\\%s\",\"N/A\",\"Ready\",\"Interactive only\",\"10/27/2026 10:00:00 AM\",\"%d\",\"STUBPC\\user\",\"stub\"\r\n",
				name, int32(uint32(result)))
		}
	case "/run":
		if probe {
			result := resultNotFound
			if b, err := os.ReadFile(state); err == nil {
				if _, err := os.Stat(strings.TrimSpace(string(b))); err == nil {
					result = 0
				}
			}
			if v := os.Getenv("STUB_PROBE_RESULT"); v != "" {
				if n, err := strconv.ParseUint(v, 10, 32); err == nil {
					result = int(n)
				}
			}
			writeResult(resultFile, result)
			return
		}
		if v := os.Getenv("STUB_RUN_FAIL"); v != "" {
			if n, err := strconv.ParseUint(v, 10, 32); err == nil {
				writeResult(resultFile, int(n))
				return
			}
		}
		c := exec.Command(os.Args[0], "sleep")
		if err := c.Start(); err != nil {
			os.Exit(1)
		}
		_ = os.MkdirAll(filepath.Dir(pidFile), 0o700)
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(c.Process.Pid)+"\n"), 0o600)
		writeResult(resultFile, resultRunning)
		logLine("daemon " + strconv.Itoa(c.Process.Pid))
	case "/end":
		if probe {
			return
		}
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Kill()
				}
			}
		}
	case "/delete":
		_ = os.Remove(state)
		_ = os.Remove(resultFile)
	}
}

// flagValue is the argument after flag, or "".
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if strings.EqualFold(a, flag) && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if strings.EqualFold(a, flag) {
			return true
		}
	}
	return false
}

func writeResult(path string, result int) {
	_ = os.WriteFile(path, []byte(strconv.Itoa(result)), 0o600)
}

// taskCommand is the <Command> of a UTF-16LE task XML file, or "".
func taskCommand(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) < 2 || raw[0] != 0xFF || raw[1] != 0xFE {
		return ""
	}
	units := make([]uint16, 0, len(raw)/2)
	for i := 2; i+1 < len(raw); i += 2 {
		units = append(units, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	xml := string(utf16.Decode(units))
	_, rest, ok := strings.Cut(xml, "<Command>")
	cmd, _, ok2 := strings.Cut(rest, "</Command>")
	if !ok || !ok2 {
		return ""
	}
	// The renderer escapes &, < and >; paths here have none of them.
	return cmd
}

func logLine(s string) {
	f, err := os.OpenFile(os.Getenv("STUB_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintln(f, s)
}
