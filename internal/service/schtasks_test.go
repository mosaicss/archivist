package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
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
	opts.Alive = func(int) bool { return alive }
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
	removed, err := m.Uninstall(context.Background())
	if !removed || err != nil {
		t.Fatalf("uninstall: %v %v", removed, err)
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
