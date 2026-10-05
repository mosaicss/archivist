//go:build windows

package service

import (
	"context"
	"os"
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
