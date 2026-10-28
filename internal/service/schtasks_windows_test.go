//go:build windows

package service

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
)

// The real schtasks.exe accepts the rendered XML (UTF-16LE, logon trigger
// and principal naming the current user, LeastPrivilege): /create /xml,
// /query, /delete of a harmless task that is never run. CI only
// (ARCHIVIST_REAL_SCHTASKS=1 on the Windows runner); never on a developer
// machine by accident.
func TestRealSchtasksAcceptsTaskXML(t *testing.T) {
	if os.Getenv("ARCHIVIST_REAL_SCHTASKS") != "1" {
		t.Skip("set ARCHIVIST_REAL_SCHTASKS=1 (CI) to register a real scheduled task")
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	name := TaskName("archivist-ci-"+strconv.Itoa(os.Getpid()), "")
	bin := filepath.Join(os.Getenv("SystemRoot"), "System32", "whoami.exe")
	xml := filepath.Join(t.TempDir(), "task.xml")
	if err := os.WriteFile(xml, RenderTaskXML(u.Username, bin, t.TempDir(), []string{"--max-permission", "ask"}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = ExecRunner(ctx, "schtasks", "/delete", "/tn", name, "/f") })
	r, err := ExecRunner(ctx, "schtasks", "/create", "/xml", xml, "/tn", name, "/f")
	if err != nil || !r.OK() {
		t.Fatalf("create: %v exit %d %s", err, r.ExitCode, r.Output)
	}
	if r, err := ExecRunner(ctx, "schtasks", "/query", "/tn", name); err != nil || !r.OK() {
		t.Fatalf("query after create: %v exit %d %s", err, r.ExitCode, r.Output)
	}
	if r, err := ExecRunner(ctx, "schtasks", "/delete", "/tn", name, "/f"); err != nil || !r.OK() {
		t.Fatalf("delete: %v exit %d %s", err, r.ExitCode, r.Output)
	}
	if r, err := ExecRunner(ctx, "schtasks", "/query", "/tn", name); err != nil || r.OK() {
		t.Fatalf("query after delete: %v exit %d", err, r.ExitCode)
	}
	// The manager's status reads the same exit codes.
	m := newSchtasks(Options{Home: t.TempDir(), User: "archivist-ci-" + strconv.Itoa(os.Getpid())})
	if st, err := m.Status(ctx); err != nil || st.Loaded || st.Installed {
		t.Fatalf("status of a deleted task: %+v %v", st, err)
	}
}

// The real Task Scheduler (Story 94.14): a probe of a missing program
// reports file not found (0x80070002), and one of `cmd.exe /c exit 0`
// reports 0, as does a GUI subsystem build of cmd/archivist
// (archivistw.exe) with `version`. CI only, like TestRealSchtasksAcceptsTaskXML; the probe task
// is deleted either way.
func TestRealSchtasksProbeLaunch(t *testing.T) {
	if os.Getenv("ARCHIVIST_REAL_SCHTASKS") != "1" {
		t.Skip("set ARCHIVIST_REAL_SCHTASKS=1 (CI) to run a real scheduled task")
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	opts := Options{Home: t.TempDir(), User: u.Username, SID: "archivist-ci-probe-" + strconv.Itoa(os.Getpid())}
	name := ProbeTaskName(opts.User, opts.SID)
	t.Cleanup(func() { _, _ = ExecRunner(ctx, "schtasks", "/delete", "/tn", name, "/f") })
	missing := filepath.Join(t.TempDir(), "archivistw.exe")
	res, err := ProbeLaunch(ctx, opts, missing, []string{"version"})
	if err != nil || res != (TaskResult{Code: ResultFileNotFound, Known: true}) {
		t.Fatalf("missing program: %+v %v", res, err)
	}
	cmdExe := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	res, err = ProbeLaunch(ctx, opts, cmdExe, []string{"/c", "exit", "0"})
	if err != nil || res != (TaskResult{Code: 0, Known: true}) {
		t.Fatalf("cmd.exe /c exit 0: %+v %v", res, err)
	}
	if r, err := ExecRunner(ctx, "schtasks", "/query", "/tn", name); err != nil || r.OK() {
		t.Fatalf("the probe task is still registered: %v exit %d", err, r.ExitCode)
	}
	// The real archivistw.exe (console-less, as released) answers the
	// installer's probe argv `version` with 0 under Task Scheduler: a
	// non-zero exit there would stop every install with a task before
	// pairing. `version` prints build info and reads at most the skill's
	// SKILL.md header; it needs no network, config or key.
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go is needed to build archivistw.exe: %v", err)
	}
	archivistw := filepath.Join(t.TempDir(), "archivistw.exe")
	build := exec.Command(goBin, "build", "-ldflags=-H=windowsgui", "-o", archivistw, "github.com/mosaicss/archivist/cmd/archivist")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build archivistw.exe: %v\n%s", err, out)
	}
	res, err = ProbeLaunch(ctx, opts, archivistw, []string{"version"})
	if err != nil || res != (TaskResult{Code: 0, Known: true}) {
		t.Fatalf("archivistw.exe version: %+v %v", res, err)
	}
}
