//go:build windows

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/mosaicss/archivist/internal/service"
)

// Helper roles of this test binary, chosen by environment before the tests
// run: a long sleeper (a stand-in daemon that /end kills), and a supervised
// daemon child that records the arguments it received and exits 0.
func init() {
	if os.Getenv("ARCHIVIST_TEST_SLEEPER") == "1" {
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	}
	if p := os.Getenv("ARCHIVIST_TEST_SERVICE_CHILD_ARGS"); p != "" && os.Getenv(supervisedEnvKey) == "1" {
		_ = os.WriteFile(p, []byte(strings.Join(os.Args[1:], " ")), 0o600)
		os.Exit(0)
	}
}

// startSleeper starts this test binary as a stand-in daemon.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command(exe, "-test.run=^$")
	c.Env = append(os.Environ(), "ARCHIVIST_TEST_SLEEPER=1")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	t.Cleanup(func() { _ = c.Process.Kill(); <-done })
	return c
}

// taskCommand is the <Command> of a UTF-16LE task XML.
func taskCommand(t *testing.T, raw []byte) string {
	t.Helper()
	if len(raw) < 2 || raw[0] != 0xFF || raw[1] != 0xFE {
		t.Fatalf("task XML is not UTF-16LE with a BOM")
	}
	units := make([]uint16, 0, len(raw)/2)
	for i := 2; i+1 < len(raw); i += 2 {
		units = append(units, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	xml := string(utf16.Decode(units))
	_, rest, ok := strings.Cut(xml, "<Command>")
	cmd, _, ok2 := strings.Cut(rest, "</Command>")
	if !ok || !ok2 {
		t.Fatalf("no <Command> in:\n%s", xml)
	}
	return cmd
}

// windowsSandbox points both HOME and USERPROFILE (what os.UserHomeDir
// reads on Windows) at a temp dir.
func windowsSandbox(t *testing.T) string {
	t.Helper()
	home := sandbox(t)
	t.Setenv("USERPROFILE", home)
	return home
}

// Install, status and uninstall against a stub schtasks (Story 78.34):
// archivistw.exe must sit next to archivist.exe; the task XML and
// service.env are written, the task registered and run; status reads the
// exit codes and the daemon pid; uninstall ends, deletes and keeps the key.
func TestConnectInstallStatusUninstallWindows(t *testing.T) {
	home := windowsSandbox(t)
	saveKey(t, home, pairedKey)
	t.Setenv("ARCHIVIST_TOKEN", "ak_mustnotreachtask00000")
	registered := false
	var daemon *exec.Cmd // the stand-in daemon /end kills
	calls := stubRunner(t, func(line string) service.Result {
		switch {
		case strings.HasPrefix(line, "schtasks /query") && !registered:
			return service.Result{ExitCode: 1}
		case strings.HasPrefix(line, "schtasks /create"):
			registered = true
		case strings.HasPrefix(line, "schtasks /end") && daemon != nil:
			_ = daemon.Process.Kill()
		case strings.HasPrefix(line, "schtasks /delete"):
			registered = false
		}
		return service.Result{}
	})
	stable, err := stableBinary()
	if err != nil {
		t.Fatal(err)
	}
	companion := filepath.Join(filepath.Dir(stable), "archivistw.exe")
	_ = os.Remove(companion)

	out, code := runRoot(t, "connect", "--install")
	if code != ExitGenericError || !strings.Contains(out, "archivistw.exe") || !strings.Contains(out, "install.ps1") || len(*calls) != 0 {
		t.Fatalf("missing companion: exit %d calls %v\n%s", code, *calls, out)
	}
	if err := os.WriteFile(companion, []byte("stub"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(companion) })

	out, code = runRoot(t, "connect", "--install", "--max-permission", "ask")
	if code != 0 || !strings.Contains(out, "installed and started (Task Scheduler:") || !strings.Contains(out, "starts again at every login") {
		t.Fatalf("install: exit %d\n%s", code, out)
	}
	xmlPath := service.TaskXMLPath(home)
	b, err := os.ReadFile(xmlPath)
	if err != nil {
		t.Fatal(err)
	}
	if args := service.ServiceArgs(string(b)); strings.Join(args, " ") != "--max-permission ask" {
		t.Fatalf("task args %q", args)
	}
	// The task runs the console-less archivistw.exe next to archivist.exe.
	if got := taskCommand(t, b); got != companion {
		t.Fatalf("task <Command> %q, want %q", got, companion)
	}
	envFile, err := os.ReadFile(service.EnvFilePath(home))
	if err != nil || strings.Contains(string(envFile), "ARCHIVIST_TOKEN") || strings.Contains(string(envFile), pairedKey) {
		t.Fatalf("service.env %v:\n%s", err, envFile)
	}
	for _, c := range *calls {
		if strings.Contains(c, pairedKey) {
			t.Fatalf("a schtasks command carried the key: %s", c)
		}
	}

	// Registered, daemon alive (a sleeper stands in): running.
	daemon = startSleeper(t)
	if _, err := service.WritePID(home, daemon.Process.Pid); err != nil {
		t.Fatal(err)
	}
	out, code = runRoot(t, "connect", "--status")
	if code != 0 || !strings.Contains(out, "Service:  running (Task Scheduler: "+xmlPath+")") || !strings.Contains(out, "Ceiling:  ask") {
		t.Fatalf("status: exit %d\n%s", code, out)
	}
	// A reinstall without the flag keeps the ceiling: updated and restarted.
	// /end stops the stand-in daemon, which is awaited before /create.
	out, code = runRoot(t, "connect", "--install")
	if code != 0 || !strings.Contains(out, "updated and restarted") || !strings.Contains(out, "kept from the installed service") {
		t.Fatalf("reinstall: exit %d\n%s", code, out)
	}

	out, code = runRoot(t, "connect", "--uninstall")
	if code != 0 || !strings.Contains(out, "Background connect removed") {
		t.Fatalf("uninstall: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(xmlPath); err == nil {
		t.Fatal("task XML kept")
	}
	if _, err := os.Stat(filepath.Join(home, ".archivist", "credentials")); err != nil {
		t.Fatalf("key removed: %v", err)
	}
	out, code = runRoot(t, "connect", "--status")
	if code != ExitGenericError || !strings.Contains(out, "not installed") {
		t.Fatalf("status after uninstall: exit %d\n%s", code, out)
	}
	out, code = runRoot(t, "connect", "--uninstall")
	if code != 0 || !strings.Contains(out, "Background connect is not installed.") {
		t.Fatalf("second uninstall: exit %d\n%s", code, out)
	}
}

// The supervisor runs the daemon child with service.env applied and the
// supervised marker, restarts it after a non-zero exit and stops when it
// exits 0. The child is this test binary in a helper role.
func TestWindowsSupervisorRestartsThenStops(t *testing.T) {
	home := windowsSandbox(t)
	// sandbox() marks this process supervised; unset it so the child's
	// marker can only come from startSupervisedDaemon (the cleanup of
	// sandbox's t.Setenv restores the earlier value).
	if err := os.Unsetenv(supervisedEnvKey); err != nil {
		t.Fatal(err)
	}
	if err := service.WriteEnvFile(home, []service.EnvVar{{Key: "CODEX_HOME", Value: `C:\captured`}}); err != nil {
		t.Fatal(err)
	}
	prev := supervisorRestartDelay
	supervisorRestartDelay = 10 * time.Millisecond
	t.Cleanup(func() { supervisorRestartDelay = prev })
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(home, "runs")
	t.Setenv("ARCHIVIST_TEST_SUPERVISED_CHILD", marker)
	t.Setenv("ARCHIVIST_TOKEN", "ak_mustnotreachdaemon000")
	starts := 0
	err = superviseDaemon(t.Context(), testLogger(), func() (func() error, error) {
		starts++
		return startSupervisedDaemon(exe, []string{"-test.run=^TestSupervisedChildHelper$"}, home)
	})
	if err != nil || starts != 2 {
		t.Fatalf("supervise: %v after %d starts", err, starts)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	want := "1 C:\\captured  \n1 C:\\captured  \n"
	if string(b) != want {
		t.Fatalf("child runs %q, want %q", b, want)
	}
}

// TestSupervisedChildHelper is the daemon stand-in: it records the marker,
// the captured CODEX_HOME and ARCHIVIST_TOKEN, then fails its first run
// and succeeds its second.
func TestSupervisedChildHelper(t *testing.T) {
	path := os.Getenv("ARCHIVIST_TEST_SUPERVISED_CHILD")
	if path == "" || os.Getenv(supervisedEnvKey) != "1" {
		t.Skip("helper process only")
	}
	prior, _ := os.ReadFile(path)
	line := os.Getenv(supervisedEnvKey) + " " + os.Getenv("CODEX_HOME") + " " + os.Getenv("ARCHIVIST_TOKEN") + " \n"
	if err := os.WriteFile(path, append(prior, line...), 0o600); err != nil {
		os.Exit(3)
	}
	if len(prior) == 0 {
		os.Exit(1)
	}
	os.Exit(0)
}

// The real command path: `connect --service --max-permission ask` with the
// supervised marker unset runs the supervisor, which starts this binary as
// the daemon child with exactly those arguments; a child exiting 0 ends
// supervision with exit 0.
func TestWindowsServiceModeRunsSupervisedChild(t *testing.T) {
	home := windowsSandbox(t)
	if err := os.Unsetenv(supervisedEnvKey); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(home, "child-args")
	t.Setenv("ARCHIVIST_TEST_SERVICE_CHILD_ARGS", argsFile)
	out, code := runRoot(t, "connect", "--service", "--max-permission", "ask")
	if code != 0 || out != "" {
		t.Fatalf("exit %d, output %q", code, out)
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("the daemon child never ran: %v", err)
	}
	if string(b) != "connect --service --max-permission ask" {
		t.Fatalf("child args %q", b)
	}
	log, _ := os.ReadFile(service.LogPath(home))
	if !strings.Contains(string(log), "supervisor: the daemon stopped (exit 0)") {
		t.Fatalf("log:\n%s", log)
	}
}

// connect --check resolves harnesses through HarnessLookPath: an npm
// claude.cmd without its native exe is reported (naming the native
// installer) and never run; with the npm package's claude.exe present, that
// exe is used.
func TestWindowsConnectCheckResolvesNpmShim(t *testing.T) {
	home := windowsSandbox(t)
	prefix := t.TempDir()
	ran := filepath.Join(prefix, "shim-ran.txt")
	shim := "@echo off\r\necho ran > \"" + ran + "\"\r\n"
	if err := os.WriteFile(filepath.Join(prefix, "claude.cmd"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	// The native stand-in is built first (go needs the real PATH) and moved
	// into the npm layout later.
	built := filepath.Join(t.TempDir(), "claude.exe")
	if b, err := exec.Command("go", "build", "-o", built, "../connect/testdata/fakeclaude").CombinedOutput(); err != nil {
		t.Fatalf("go build fakeclaude: %v\n%s", err, b)
	}
	t.Setenv("PATH", prefix)
	t.Setenv("CODEX_HOME", filepath.Join(home, "no-codex"))

	out, code := runRoot(t, "connect", "--check")
	if code != ExitNotFound || !strings.Contains(out, "install the native Claude Code") {
		t.Fatalf("shim only: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the batch shim was executed")
	}

	native := filepath.Join(prefix, "node_modules", "@anthropic-ai", "claude-code", "bin", "claude.exe")
	if err := os.MkdirAll(filepath.Dir(native), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(built, native); err != nil {
		t.Fatal(err)
	}
	out, code = runRoot(t, "connect", "--check")
	if code != 0 || !strings.Contains(out, "path:     "+native) || !strings.Contains(out, "status:   usable") {
		t.Fatalf("native exe: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the batch shim was executed")
	}
}
