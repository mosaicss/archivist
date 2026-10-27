package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTaskName(t *testing.T) {
	hash := func(u string) string {
		sum := sha256.Sum256([]byte(u))
		return hex.EncodeToString(sum[:4])
	}
	const sid = "S-1-5-21-1004336348-1177238915-682003330-1001"
	// The readable prefix comes from the user name, the hash from the SID.
	for in, base := range map[string]string{
		`DESKTOP-1\alice`: "MosaicArchivistConnect-alice",
		"bob.smith":       "MosaicArchivistConnect-bob.smith",
		`AD\José Ü`:       "MosaicArchivistConnect-Jos___",
		`x\`:              "MosaicArchivistConnect-user",
		"":                "MosaicArchivistConnect-user",
		"a/b c":           "MosaicArchivistConnect-b_c",
	} {
		if got, want := TaskName(in, sid), base+"-"+hash(sid); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
		// Without a SID the full user string is hashed.
		if got, want := TaskName(in, ""), base+"-"+hash(in); got != want {
			t.Errorf("%q without a SID: %q, want %q", in, got, want)
		}
	}
	// Renaming the PC or the account keeps the task (same SID).
	if TaskName(`OLDPC\bob`, sid) != TaskName(`NEWPC\bob`, sid) {
		t.Fatal("a PC rename changed the task name")
	}
	if a, b := TaskName(`PC\bob`, sid), TaskName(`PC\robert`, sid); a[len(a)-8:] != b[len(b)-8:] {
		t.Fatalf("an account rename changed the hash: %s %s", a, b)
	}
	// Accounts whose reduced names agree get distinct tasks.
	if TaskName(`PC\bob`, sid) == TaskName(`AD\bob`, sid+"2") {
		t.Fatal("two accounts share a task")
	}
	for _, pair := range [][2]string{{`PC\bob`, `AD\bob`}, {`PC\José`, `PC\Josi`}, {`PC\Ü`, `PC\Ö`}} {
		if TaskName(pair[0], "") == TaskName(pair[1], "") {
			t.Errorf("%q and %q share the task %s", pair[0], pair[1], TaskName(pair[0], ""))
		}
	}
	// Stable across calls and releases: a fixed expectation.
	if got := TaskName(`PC\bob`, ""); got != "MosaicArchivistConnect-bob-40439733" {
		t.Fatalf("task name %q", got)
	}
}

// The task XML is UTF-16LE with a BOM, declares UTF-16, carries every
// required setting, escapes XML, and its arguments read back exactly
// through ServiceArgs (Windows command line quoting included).
func TestRenderTaskXML(t *testing.T) {
	raw := RenderTaskXML(`PC\a&b`, `C:\Users\a b\AppData\Local\Programs\archivist\archivistw.exe`, `C:\Users\a b`, []string{"--max-permission", "full_auto"})
	if raw[0] != 0xFF || raw[1] != 0xFE {
		t.Fatalf("no UTF-16LE byte order mark: % x", raw[:4])
	}
	xml, ok := decodeUTF16LE(string(raw))
	if !ok {
		t.Fatal("decode failed")
	}
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-16"?>`,
		`<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">`,
		"<LogonTrigger>\r\n      <Enabled>true</Enabled>\r\n      <UserId>PC\\a&amp;b</UserId>",
		"<UserId>PC\\a&amp;b</UserId>\r\n      <LogonType>InteractiveToken</LogonType>\r\n      <RunLevel>LeastPrivilege</RunLevel>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<RestartOnFailure>\r\n      <Interval>PT1M</Interval>\r\n      <Count>3</Count>",
		`<Command>C:\Users\a b\AppData\Local\Programs\archivist\archivistw.exe</Command>`,
		"<Arguments>connect --service --max-permission full_auto</Arguments>",
		`<WorkingDirectory>C:\Users\a b</WorkingDirectory>`,
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("task XML lacks %q:\n%s", want, xml)
		}
	}
	// schtasks rejects a comment holding "--" ("incorrect comment syntax"),
	// so the definition carries no XML comment at all.
	if strings.Contains(xml, "<!--") {
		t.Errorf("task XML has a comment (schtasks rejects \"--\" inside one):\n%s", xml)
	}
	if got := ServiceArgs(string(raw)); strings.Join(got, "|") != "--max-permission|full_auto" {
		t.Fatalf("args %q", got)
	}
	for _, c := range [][]string{nil, {"--max-permission", "ask"}, {`we"ird 50%$\x`, "<&>", `trail\`, `sp ace\`, "", "\ttab"}} {
		raw := RenderTaskXML("u", `C:\p q\archivistw.exe`, `C:\h`, c)
		got := ServiceArgs(string(raw))
		if strings.Join(got, "|") != strings.Join(c, "|") || len(got) != len(c) {
			t.Errorf("%q: read back %q", c, got)
		}
	}
	// Not ours, or not a task: nothing.
	other := RenderTaskXML("u", `C:\x.exe`, `C:\h`, nil)
	xml, _ = decodeUTF16LE(string(other))
	if got := ServiceArgs(strings.Replace(xml, "<Arguments>connect --service", "<Arguments>other --service", 1)); got != nil {
		t.Fatalf("foreign task args %q", got)
	}
	if got := ServiceArgs("\xff\xfe"); got != nil {
		t.Fatalf("empty BOM file %q", got)
	}
}

// windowsArg and splitWindowsArgs agree with each other (and so with the
// Go runtime's own command line parsing of archivistw.exe).
func TestWindowsArgRoundTrip(t *testing.T) {
	args := []string{"a", "", "b c", `d"e`, `f\`, `g\\"h`, `i j\`, `\\server\share\x y`, `"`, `\`, "k\tl"}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = windowsArg(a)
	}
	got := splitWindowsArgs(strings.Join(quoted, " "))
	if strings.Join(got, "|") != strings.Join(args, "|") || len(got) != len(args) {
		t.Fatalf("got %q\nwant %q\nline %s", got, args, strings.Join(quoted, " "))
	}
	if windowsArg("plain") != "plain" || windowsArg("a b") != `"a b"` || windowsArg(`a"b`) != `a\"b` {
		t.Fatal("quoting changed")
	}
}

func TestEnvFileAndMerge(t *testing.T) {
	home := t.TempDir()
	if ReadEnvFile(home) != nil {
		t.Fatal("env from nothing")
	}
	env := []EnvVar{{"PATH", `C:\bin;C:\x`}, {"CODEX_HOME", `D:\codex`}, {"BAD", "a\nb"}}
	if err := WriteEnvFile(home, env); err != nil {
		t.Fatal(err)
	}
	got := ReadEnvFile(home)
	if len(got) != 2 || got[0] != env[0] || got[1] != env[1] {
		t.Fatalf("read back %v", got)
	}
	merged := MergeEnv([]string{"Path=C:\\old", "ARCHIVIST_TOKEN=ak_x", "archivist_token=ak_y", "Other=1", "=C:=C:\\"}, got, true, "ARCHIVIST_SERVICE_SUPERVISED=1")
	want := `Other=1|=C:=C:\|PATH=C:\bin;C:\x|CODEX_HOME=D:\codex|ARCHIVIST_SERVICE_SUPERVISED=1`
	if strings.Join(merged, "|") != want {
		t.Fatalf("merged %q", merged)
	}
	// Exact keys without fold: only an exact match is replaced.
	merged = MergeEnv([]string{"Path=a", "PATH=b"}, []EnvVar{{"PATH", "c"}}, false)
	if strings.Join(merged, "|") != "Path=a|PATH=c" {
		t.Fatalf("no fold %q", merged)
	}
}

// On Windows PATH is captured from Path (case-insensitive keys).
func TestCaptureEnvFoldsCase(t *testing.T) {
	codex := t.TempDir() // absolute on every platform
	env := captureEnv([]string{"Path=C:\\w", "Codex_Home=" + codex, "archivist_token=ak_x", "HOME=C:\\h"}, true)
	var got []string
	for _, e := range env {
		got = append(got, e.Key+"="+e.Value)
	}
	if strings.Join(got, " ") != `PATH=C:\w HOME=C:\h CODEX_HOME=`+codex {
		t.Fatalf("got %v", got)
	}
	if env := captureEnv([]string{"Path=x"}, false); len(env) != 0 {
		t.Fatalf("unfolded capture took Path: %v", env)
	}
}

func schtasksOpts(t *testing.T, f *fakeRunner) Options {
	o := testOpts(t, f)
	o.User = `PC\alice`
	o.Env = []EnvVar{{"PATH", `C:\bin`}}
	return o
}

var testTask = TaskName(`PC\alice`, "")

// A first install writes service.env and the XML, registers the task,
// runs it (nothing to end) and waits for the new daemon's pid.
func TestSchtasksInstallFresh(t *testing.T) {
	f := &fakeRunner{answers: map[string][]Result{"schtasks /query": {{ExitCode: 1}}}}
	opts := schtasksOpts(t, f)
	polls := 0
	opts.Alive = func(pid int) bool { return pid == 4242 }
	opts.Sleep = func(time.Duration) {
		polls++
		if polls == 2 {
			if _, err := WritePID(opts.Home, 4242); err != nil {
				t.Fatal(err)
			}
		}
	}
	m := newSchtasks(opts)
	rep, err := m.Install(context.Background())
	if err != nil || rep.Restarted || rep.Path != TaskXMLPath(opts.Home) {
		t.Fatalf("%+v %v", rep, err)
	}
	want := []string{
		"schtasks /query /tn " + testTask,
		"schtasks /create /xml " + rep.Path + " /tn " + testTask + " /f",
		"schtasks /run /tn " + testTask,
	}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v", f.calls)
	}
	if polls != 2 {
		t.Fatalf("polled %d times for the daemon pid", polls)
	}
	if got := ReadEnvFile(opts.Home); len(got) != 1 || got[0].Key != "PATH" {
		t.Fatalf("env file %v", got)
	}
	b, err := os.ReadFile(rep.Path)
	if err != nil {
		t.Fatal(err)
	}
	if args := ServiceArgs(string(b)); args != nil {
		t.Fatalf("args %q", args)
	}
	if xml, _ := decodeUTF16LE(string(b)); !strings.Contains(xml, "<Command>"+testBinary+"</Command>") {
		t.Fatalf("xml:\n%s", xml)
	}
	assertLog(t, opts.Home)
}

// A reinstall ends the previous run, waits for its daemon to exit, then
// runs the task again: "updated and restarted".
func TestSchtasksReinstallEndsAndAwaits(t *testing.T) {
	f := &fakeRunner{answers: map[string][]Result{"schtasks /end": {{ExitCode: 1, Output: "not running"}}}}
	opts := schtasksOpts(t, f)
	opts.Args = []string{"--max-permission", "read_only"}
	if _, err := WritePID(opts.Home, 100); err != nil {
		t.Fatal(err)
	}
	oldAlive := 3
	aliveAtCreate := false
	inner := opts.Run
	opts.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
		if len(args) > 0 && args[0] == "/create" && oldAlive > 0 {
			aliveAtCreate = true
		}
		return inner(ctx, name, args...)
	}
	opts.Alive = func(pid int) bool {
		switch pid {
		case 100:
			oldAlive--
			return oldAlive > 0
		case 200:
			return true
		}
		return false
	}
	opts.Sleep = func(time.Duration) {
		if oldAlive <= 0 {
			_, _ = WritePID(opts.Home, 200)
		}
	}
	m := newSchtasks(opts)
	rep, err := m.Install(context.Background())
	if err != nil || !rep.Restarted {
		t.Fatalf("%+v %v %v", rep, err, f.calls)
	}
	// The running instance ends, and its daemon exits, before the task is
	// registered again; then the new definition runs.
	end, create, run := indexOf(f.calls, "schtasks /end"), indexOf(f.calls, "schtasks /create"), indexOf(f.calls, "schtasks /run")
	if end < 0 || create < end || run < create {
		t.Fatalf("order %v", f.calls)
	}
	if oldAlive > 0 || aliveAtCreate {
		t.Fatalf("the old daemon was not awaited before /create (left %d, alive at create %v)", oldAlive, aliveAtCreate)
	}
	st, err := m.Status(context.Background())
	if err != nil || !st.Installed || !st.Loaded || !st.Running || strings.Join(st.Args, " ") != "--max-permission read_only" {
		t.Fatalf("status %+v %v", st, err)
	}
}

// A refused registration removes the files this call wrote and reports
// schtasks' output; schtasks missing reports that.
func TestSchtasksCreateFailureLeavesNothing(t *testing.T) {
	f := &fakeRunner{answers: map[string][]Result{
		"schtasks /query":  {{ExitCode: 1}},
		"schtasks /create": {{ExitCode: 1, Output: "ERROR: Access is denied."}},
	}}
	opts := schtasksOpts(t, f)
	m := newSchtasks(opts)
	if _, err := m.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "Access is denied") {
		t.Fatalf("err %v", err)
	}
	if fileExists(m.Path()) || fileExists(EnvFilePath(opts.Home)) || f.called("schtasks /run") != 0 {
		t.Fatalf("files kept or task run: %v", f.calls)
	}
	f = &fakeRunner{missing: map[string]bool{"schtasks": true}}
	opts.Run = f.run
	if _, err := newSchtasks(opts).Install(context.Background()); err == nil || !strings.Contains(err.Error(), "schtasks is not available") {
		t.Fatalf("err %v", err)
	}
	opts.User = ""
	if _, err := newSchtasks(opts).Install(context.Background()); err == nil {
		t.Fatal("installed without a user")
	}
}

// Status: XML absent is not installed; registered with a dead pid is
// loaded, not running; registered with a live pid is running. Uninstall
// ends and deletes the task, removes both files and keeps the key.
func TestSchtasksStatusAndUninstall(t *testing.T) {
	registered := false
	f := &fakeRunner{}
	opts := schtasksOpts(t, f)
	opts.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
		r, err := f.run(ctx, name, args...)
		if len(args) > 0 && args[0] == "/query" && !registered {
			r.ExitCode = 1
		}
		return r, err
	}
	alive := false
	isAlive := func() bool { return alive }
	opts.Alive = func(int) bool { return isAlive() }
	m := newSchtasks(opts)
	st, err := m.Status(context.Background())
	if err != nil || st.Installed || st.Loaded || st.Running || st.Manager != "Task Scheduler" {
		t.Fatalf("before: %+v %v", st, err)
	}
	if removed, err := m.Uninstall(context.Background()); removed || err != nil {
		t.Fatalf("uninstall nothing: %v %v", removed, err)
	}
	if _, err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	registered = true
	if _, err := WritePID(opts.Home, 77); err != nil {
		t.Fatal(err)
	}
	if st, _ = m.Status(context.Background()); !st.Installed || !st.Loaded || st.Running {
		t.Fatalf("dead pid: %+v", st)
	}
	alive = true
	if st, _ = m.Status(context.Background()); !st.Running {
		t.Fatalf("live pid: %+v", st)
	}
	key := filepath.Join(opts.Home, ".archivist", "credentials")
	if err := os.WriteFile(key, []byte("ak_kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	// The ended run's daemon exits a few polls after /end: uninstall waits
	// for it before removing the files it holds open.
	polls := 0
	isAlive = func() bool { polls++; return polls < 4 }
	removed, err := m.Uninstall(context.Background())
	if !removed || err != nil || polls < 4 {
		t.Fatalf("uninstall: %v %v (polls %d)", removed, err, polls)
	}
	if f.called("schtasks /end /tn "+testTask) != 1 || f.called("schtasks /delete /tn "+testTask+" /f") != 1 {
		t.Fatalf("calls %v", f.calls)
	}
	if fileExists(m.Path()) || fileExists(EnvFilePath(opts.Home)) || !fileExists(key) {
		t.Fatal("uninstall left the task files or removed the key")
	}
	// A refused delete keeps the files and reports the failure.
	if _, err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.answers = map[string][]Result{"schtasks /delete": {{ExitCode: 1, Output: "ERROR: denied"}}}
	if removed, err := m.Uninstall(context.Background()); removed || err == nil || !fileExists(m.Path()) {
		t.Fatalf("refused delete: %v %v", removed, err)
	}
	// A daemon still alive after the wait: the task and files go, and the
	// uninstall says so.
	f.answers = nil
	if _, err := WritePID(opts.Home, 77); err != nil {
		t.Fatal(err)
	}
	isAlive = func() bool { return true }
	removed, err = m.Uninstall(context.Background())
	if !removed || err == nil || !strings.Contains(err.Error(), "did not stop") || fileExists(m.Path()) {
		t.Fatalf("still alive: %v %v", removed, err)
	}
}

// A reinstall whose registration is refused puts the previous task XML and
// service.env back and runs the previous definition again.
func TestSchtasksRefusedReinstallRestores(t *testing.T) {
	f := &fakeRunner{}
	opts := schtasksOpts(t, f)
	opts.Alive = func(int) bool { return false }
	opts.Args = []string{"--max-permission", "ask"}
	m := newSchtasks(opts)
	if _, err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	prevXML, _ := os.ReadFile(m.Path())
	prevEnv, _ := os.ReadFile(EnvFilePath(opts.Home))
	f.calls = nil
	f.answers = map[string][]Result{"schtasks /create": {{ExitCode: 1, Output: "ERROR: Access is denied."}}}
	m.opts.Args = []string{"--max-permission", "full_auto"}
	m.opts.Env = []EnvVar{{"PATH", `C:\other`}}
	if _, err := m.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "Access is denied") {
		t.Fatalf("err %v", err)
	}
	if b, _ := os.ReadFile(m.Path()); string(b) != string(prevXML) {
		t.Fatal("the previous task XML was not restored")
	}
	if b, _ := os.ReadFile(EnvFilePath(opts.Home)); string(b) != string(prevEnv) {
		t.Fatal("the previous service.env was not restored")
	}
	end, create, rerun := indexOf(f.calls, "schtasks /end"), indexOf(f.calls, "schtasks /create"), indexOf(f.calls, "schtasks /run")
	if end < 0 || create < end || rerun < create {
		t.Fatalf("calls %v", f.calls)
	}
}

// A previous run that does not stop after /end fails the reinstall with
// nothing changed: the /run would be a no-op (IgnoreNew) while the old
// daemon runs on.
func TestSchtasksReinstallRefusesWhenOldRunSurvives(t *testing.T) {
	f := &fakeRunner{}
	opts := schtasksOpts(t, f)
	opts.Alive = func(int) bool { return false }
	m := newSchtasks(opts)
	if _, err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	prevXML, _ := os.ReadFile(m.Path())
	if _, err := WritePID(opts.Home, 4321); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	m.opts.Alive = func(pid int) bool { return pid == 4321 }
	m.opts.Args = []string{"--max-permission", "full_auto"}
	rep, err := m.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not stop") || rep.Restarted {
		t.Fatalf("%+v %v", rep, err)
	}
	if f.called("schtasks /create") != 0 || f.called("schtasks /run") != 0 || f.called("schtasks /end") != 1 {
		t.Fatalf("calls %v", f.calls)
	}
	if b, _ := os.ReadFile(m.Path()); string(b) != string(prevXML) {
		t.Fatal("the task XML changed")
	}
	if readPID(opts.Home) != 4321 {
		t.Fatal("the live daemon's pid file was removed")
	}
}

// A stale service.pid (a hard killed run) is removed before /run on
// install and by uninstall, so a reused pid never reads as the daemon.
func TestSchtasksRemovesStalePID(t *testing.T) {
	f := &fakeRunner{answers: map[string][]Result{"schtasks /query": {{ExitCode: 1}}}}
	opts := schtasksOpts(t, f)
	opts.Alive = func(int) bool { return false }
	if _, err := WritePID(opts.Home, 999); err != nil {
		t.Fatal(err)
	}
	pidAtRun := -1
	inner := opts.Run
	opts.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
		if len(args) > 0 && args[0] == "/run" {
			pidAtRun = readPID(opts.Home)
		}
		return inner(ctx, name, args...)
	}
	m := newSchtasks(opts)
	if _, err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pidAtRun != 0 {
		t.Fatalf("service.pid at /run holds %d, want none", pidAtRun)
	}
	if _, err := WritePID(opts.Home, 998); err != nil {
		t.Fatal(err)
	}
	f.answers = nil
	if removed, err := m.Uninstall(context.Background()); !removed || err != nil {
		t.Fatalf("uninstall %v %v", removed, err)
	}
	if _, err := os.Stat(PIDPath(opts.Home)); !os.IsNotExist(err) {
		t.Fatalf("service.pid kept after uninstall: %v", err)
	}
}

// A daemon still running while its task is gone (deleted in Task
// Scheduler) refuses the install with nothing changed: a /run would start a
// second, untracked daemon.
func TestSchtasksInstallRefusesUntrackedDaemon(t *testing.T) {
	f := &fakeRunner{answers: map[string][]Result{"schtasks /query": {{ExitCode: 1}}}}
	opts := schtasksOpts(t, f)
	opts.Alive = func(pid int) bool { return pid == 5555 }
	if _, err := WritePID(opts.Home, 5555); err != nil {
		t.Fatal(err)
	}
	m := newSchtasks(opts)
	_, err := m.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pid 5555") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err %v", err)
	}
	if f.called("schtasks /create") != 0 || f.called("schtasks /run") != 0 || f.called("schtasks /end") != 0 {
		t.Fatalf("calls %v", f.calls)
	}
	if fileExists(m.Path()) || fileExists(EnvFilePath(opts.Home)) || readPID(opts.Home) != 5555 {
		t.Fatal("files changed")
	}
}

// Story 94.14: generalising the renderer for the probe leaves the service
// task's definition byte for byte as it was (SHA-256 of v0.2.35's output).
func TestRenderTaskXMLUnchanged(t *testing.T) {
	for _, c := range []struct {
		user, bin, home, want string
		args                  []string
	}{
		{`PC\a&b`, `C:\Users\a b\.archivist\bin\archivistw.exe`, `C:\Users\a b`,
			"4360435e8eb88a535aa8125a908b586572bb9577e18443256a4ba3de1b6b0f42", []string{"--max-permission", "full_auto"}},
		{"u", `C:\p q\archivistw.exe`, `C:\h`, "71a5327489dd919507906e1f9967b5c2233891b534e047d1be9f49bbaf566bfe", nil},
	} {
		sum := sha256.Sum256(RenderTaskXML(c.user, c.bin, c.home, c.args))
		if got := hex.EncodeToString(sum[:]); got != c.want {
			t.Errorf("%s: the service task XML changed (sha256 %s)", c.bin, got)
		}
	}
}

// The probe has the service task's principal but no trigger, no restart,
// a one minute limit and its own arguments.
func TestRenderProbeXML(t *testing.T) {
	xml, ok := decodeUTF16LE(string(RenderProbeXML(`PC\alice`, `C:\Users\alice\.archivist\bin\archivistw.exe`, `C:\Users\alice`, []string{"version"})))
	if !ok {
		t.Fatal("not UTF-16LE")
	}
	for _, want := range []string{
		"<UserId>PC\\alice</UserId>\r\n      <LogonType>InteractiveToken</LogonType>\r\n      <RunLevel>LeastPrivilege</RunLevel>",
		"<ExecutionTimeLimit>PT1M</ExecutionTimeLimit>",
		`<Command>C:\Users\alice\.archivist\bin\archivistw.exe</Command>`,
		"<Arguments>version</Arguments>",
		`<WorkingDirectory>C:\Users\alice</WorkingDirectory>`,
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("probe XML lacks %q:\n%s", want, xml)
		}
	}
	for _, unwanted := range []string{"<Triggers>", "<LogonTrigger>", "<RestartOnFailure>", "connect --service", "<!--"} {
		if strings.Contains(xml, unwanted) {
			t.Errorf("probe XML has %q:\n%s", unwanted, xml)
		}
	}
	if ServiceArgs(xml) != nil {
		t.Fatal("the probe reads as a service definition")
	}
}

func TestProbeTaskName(t *testing.T) {
	if got, want := ProbeTaskName(`PC\bob`, ""), "MosaicArchivistProbe-bob-40439733"; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	if ProbeTaskName(`PC\bob`, "S-1") == TaskName(`PC\bob`, "S-1") {
		t.Fatal("the probe shares the service task's name")
	}
}

// probeRow is a schtasks /query /v /fo csv /nh row with result in column 6.
func probeRow(result string) string {
	return `"PC","\` + ProbeTaskName(`PC\alice`, "") + `","N/A","Ready","Interactive only","10/27/2026 10:00:00","` + result + `","PC\alice","C:\x.exe version"` + "\r\n"
}

// The last result is read by column position: signed or unsigned decimal
// or hex; anything else is unknown.
func TestParseLastResult(t *testing.T) {
	for in, want := range map[string]TaskResult{
		probeRow("-2147024894"):              {ResultFileNotFound, true},
		probeRow("2147942402"):               {ResultFileNotFound, true},
		probeRow("0x80070002"):               {ResultFileNotFound, true},
		probeRow("0X41301"):                  {0x41301, true},
		probeRow("0"):                        {0, true},
		probeRow("1"):                        {1, true},
		"\r\n" + probeRow("267011"):          {0x41303, true},
		probeRow("267011") + probeRow("1"):   {0x41303, true},
		probeRow("Ready"):                    {},
		probeRow(""):                         {},
		probeRow("4294967296"):               {},
		probeRow("-2147483649"):              {},
		probeRow("0x1FFFFFFFF"):              {},
		`"PC","\task","N/A","Ready","x","y"`: {},
		"":                                   {},
		"ERROR: The system cannot find the file specified.": {},
	} {
		if got := parseLastResult(in); got != want {
			t.Errorf("%q: %+v, want %+v", in, got, want)
		}
	}
	f := &fakeRunner{answers: map[string][]Result{"schtasks /query": {{ExitCode: 1, Output: probeRow("0")}}}}
	if got := LastTaskResult(context.Background(), schtasksOpts(t, f), "x"); got.Known {
		t.Fatalf("a failed query read %+v", got)
	}
	if f.calls[0] != "schtasks /query /tn x /v /fo csv /nh" {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestTaskResultText(t *testing.T) {
	for code, want := range map[uint32]string{
		0x80070002: "the program file was not found (0x80070002)",
		0x80070003: "the program file was not found (0x80070003)",
		0x80070005: "access was denied (0x80070005)",
		0x8007010B: "the start folder is not valid (0x8007010B)",
		0x800700C1: "Windows cannot run the program file (0x800700C1)",
		0x800704DD: "the user is not logged on (0x800704DD)",
		0x800710E0: "an administrator or policy refused the request (0x800710E0)",
		0x8004131F: "an instance is already running (0x8004131F)",
		0x41301:    "the task is still running (0x00041301)",
		0x41303:    "the task has not run yet (0x00041303)",
		0x41306:    "the task was stopped (0x00041306)",
		0xC000013A: "the task was stopped (0xC000013A)",
		1:          "the program exited with code 1 (0x00000001)",
		0xFFFF:     "the program exited with code 65535 (0x0000FFFF)",
		0x10000:    "result 0x00010000",
		0x80004005: "result 0x80004005",
	} {
		if got := TaskResultText(code); got != want {
			t.Errorf("0x%08X: %q, want %q", code, got, want)
		}
	}
}

var testProbe = ProbeTaskName(`PC\alice`, "")

// The probe registers and runs a one-shot task, polls its last result past
// "has not run" and "running", and always deletes the task and its XML.
func TestProbeLaunch(t *testing.T) {
	bin := `C:\Users\alice\.archivist\bin\archivistw.exe`
	if runtime.GOOS != "windows" {
		bin = "/home/alice/.archivist/bin/archivistw.exe"
	}
	query := "schtasks /query /tn " + testProbe
	for name, c := range map[string]struct {
		answers []Result
		want    TaskResult
	}{
		"success":        {[]Result{{Output: probeRow("267011")}, {Output: probeRow("267009")}, {Output: probeRow("0")}}, TaskResult{0, true}},
		"file not found": {[]Result{{Output: probeRow("-2147024894")}}, TaskResult{ResultFileNotFound, true}},
		"exit code":      {[]Result{{Output: probeRow("267009")}, {Output: probeRow("3")}}, TaskResult{3, true}},
		"timeout":        {[]Result{{Output: probeRow("267009")}}, TaskResult{}},
		"unreadable":     {[]Result{{Output: "garbage"}}, TaskResult{}},
		"query refused":  {[]Result{{ExitCode: 1}}, TaskResult{}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeRunner{answers: map[string][]Result{query: c.answers}}
			opts := schtasksOpts(t, f)
			polls := 0
			opts.Sleep = func(d time.Duration) {
				if d != 250*time.Millisecond {
					t.Errorf("polled every %v", d)
				}
				polls++
			}
			var xmlAtCreate []byte
			inner := opts.Run
			opts.Run = func(ctx context.Context, n string, args ...string) (Result, error) {
				if len(args) > 2 && args[0] == "/create" {
					xmlAtCreate, _ = os.ReadFile(args[2])
				}
				return inner(ctx, n, args...)
			}
			got, err := newSchtasks(opts).ProbeLaunch(context.Background(), bin, []string{"version"})
			if err != nil || got != c.want {
				t.Fatalf("%+v %v, want %+v (calls %v)", got, err, c.want, f.calls)
			}
			xmlPath := filepath.Join(opts.Home, ".archivist", "connect", "MosaicArchivistProbe.xml")
			want := []string{
				"schtasks /create /xml " + xmlPath + " /tn " + testProbe + " /f",
				"schtasks /run /tn " + testProbe,
			}
			if strings.Join(f.calls[:2], "|") != strings.Join(want, "|") {
				t.Fatalf("calls %v", f.calls)
			}
			if last := f.calls[len(f.calls)-1]; last != "schtasks /delete /tn "+testProbe+" /f" {
				t.Fatalf("the probe task was not deleted last: %v", f.calls)
			}
			if xml, _ := decodeUTF16LE(string(xmlAtCreate)); !strings.Contains(xml, "<Command>"+bin+"</Command>") || !strings.Contains(xml, "<Arguments>version</Arguments>") {
				t.Fatalf("probe XML:\n%s", xml)
			}
			if fileExists(xmlPath) {
				t.Fatal("the probe XML was kept")
			}
			if info, err := os.Stat(filepath.Dir(xmlPath)); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
				t.Fatalf("connect dir %v %v", info, err)
			}
			if !c.want.Known && polls != 60 {
				t.Fatalf("an unknown result after %d polls, want 60 (15 s)", polls)
			}
		})
	}
}

// A refused registration or run is an error, and the probe task is still
// deleted; nothing runs without a user or an absolute binary.
func TestProbeLaunchFailures(t *testing.T) {
	bin := testBinary
	for _, refused := range []string{"schtasks /create", "schtasks /run"} {
		f := &fakeRunner{answers: map[string][]Result{refused: {{ExitCode: 1, Output: "ERROR: Access is denied."}}}}
		opts := schtasksOpts(t, f)
		if _, err := ProbeLaunch(context.Background(), opts, bin, []string{"version"}); err == nil || !strings.Contains(err.Error(), "Access is denied") {
			t.Fatalf("%s refused: %v", refused, err)
		}
		if f.called("schtasks /delete /tn "+testProbe+" /f") != 1 || f.called("schtasks /query") != 0 {
			t.Fatalf("%s refused: calls %v", refused, f.calls)
		}
		if fileExists(filepath.Join(opts.Home, ".archivist", "connect", "MosaicArchivistProbe.xml")) {
			t.Fatal("the probe XML was kept")
		}
	}
	f := &fakeRunner{}
	opts := schtasksOpts(t, f)
	if _, err := ProbeLaunch(context.Background(), opts, "relative.exe", nil); err == nil || len(f.calls) != 0 {
		t.Fatalf("relative binary: %v %v", err, f.calls)
	}
	opts.User = ""
	if _, err := ProbeLaunch(context.Background(), opts, bin, nil); err == nil || len(f.calls) != 0 {
		t.Fatalf("no user: %v %v", err, f.calls)
	}
}

// A registered task whose daemon is not running reports its last result;
// a running one does not ask.
func TestSchtasksStatusReadsLastResult(t *testing.T) {
	f := &fakeRunner{answers: map[string][]Result{
		"schtasks /query /tn " + testTask + " /v": {{Output: strings.Replace(probeRow("-2147024894"), testProbe, testTask, 1)}},
	}}
	opts := schtasksOpts(t, f)
	opts.Alive = func(int) bool { return false }
	m := newSchtasks(opts)
	if err := os.MkdirAll(filepath.Dir(m.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.Path(), RenderTaskXML(opts.User, testBinary, opts.Home, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := m.Status(context.Background())
	if err != nil || !st.Loaded || st.Running || st.LastResult != (TaskResult{ResultFileNotFound, true}) || !st.LastResult.NotFound() {
		t.Fatalf("%+v %v", st, err)
	}
	f.calls = nil
	m.opts.Alive = func(int) bool { return true }
	if _, err := WritePID(opts.Home, 9); err != nil {
		t.Fatal(err)
	}
	if st, _ = m.Status(context.Background()); !st.Running || st.LastResult.Known || f.called("schtasks /query /tn "+testTask+" /v") != 0 {
		t.Fatalf("running: %+v %v", st, f.calls)
	}
}

func TestTaskResultFailed(t *testing.T) {
	for r, want := range map[TaskResult]bool{
		{}:                          false,
		{0, true}:                   false,
		{0x41301, true}:             false,
		{0x41303, true}:             true,
		{1, true}:                   true,
		{ResultFileNotFound, true}:  true,
		{ResultFileNotFound, false}: false,
	} {
		if r.Failed() != want {
			t.Errorf("%+v failed %v", r, !want)
		}
	}
}
