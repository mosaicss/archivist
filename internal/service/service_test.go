package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeRunner records commands and answers from a script keyed on the
// joined command line (prefix match); unknown commands exit 0.
type fakeRunner struct {
	calls   []string
	answers map[string][]Result // consumed in order; the last one repeats
	missing map[string]bool     // binaries that are "not found"
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (Result, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	if f.missing[name] {
		return Result{}, errors.New("executable file not found in $PATH")
	}
	for prefix, rs := range f.answers {
		if strings.HasPrefix(line, prefix) {
			r := rs[0]
			if len(rs) > 1 {
				f.answers[prefix] = rs[1:]
			}
			return r, nil
		}
	}
	return Result{}, nil
}

func (f *fakeRunner) called(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func testOpts(t *testing.T, f *fakeRunner) Options {
	t.Helper()
	home := t.TempDir()
	return Options{
		Home:   home,
		Binary: testBinary,
		Env:    []EnvVar{{"PATH", "/usr/bin:/bin"}, {"HOME", home}},
		Run:    f.run,
		UID:    501,
		User:   "alice",
		Sleep:  func(time.Duration) {},
	}
}

// testBinary is an absolute binary path on every platform ("/opt/..." is
// not absolute on Windows); the rendered definitions below expect it.
var testBinary = func() string {
	if runtime.GOOS == "windows" {
		return `C:\opt\archivist\bin\archivist`
	}
	return "/opt/archivist/bin/archivist"
}()

// abs makes a slash path absolute on this platform (a drive on Windows).
func abs(p string) string {
	if runtime.GOOS == "windows" {
		return `C:` + filepath.FromSlash(p)
	}
	return p
}

func TestCaptureEnv(t *testing.T) {
	// CODEX_HOME and CLAUDE_CONFIG_DIR must be absolute on this platform.
	codexHome, claudeDir := abs("/c"), abs("/u/.claude-b")
	env := CaptureEnv([]string{
		"PATH=/a:/b", "HOME=/h", "LANG=en_US.UTF-8", "LC_ALL=C", "LC_CTYPE=x",
		"CODEX_HOME=" + codexHome, "CLAUDE_CONFIG_DIR=relative", "ARCHIVIST_TOKEN=ak_secret",
		"ARCHIVIST_BASE_URL=http://127.0.0.1:9", "ARCHIVIST_RELAY_URL=", "ANTHROPIC_API_KEY=k",
		"SHELL=/bin/zsh", "TERM=x", "USER=u", "BAD=a\nb", "CLAUDE_CODE_OAUTH_TOKEN=o",
	})
	var got []string
	for _, e := range env {
		got = append(got, e.Key+"="+e.Value)
	}
	want := "PATH=/a:/b HOME=/h LANG=en_US.UTF-8 LC_ALL=C CODEX_HOME=" + codexHome + " ARCHIVIST_BASE_URL=http://127.0.0.1:9"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v\nwant %s", got, want)
	}
	env = CaptureEnv([]string{"CLAUDE_CONFIG_DIR=" + claudeDir, "PATH=/x\n", "CODEX_HOME=rel"})
	if len(env) != 1 || env[0] != (EnvVar{"CLAUDE_CONFIG_DIR", claudeDir}) {
		t.Fatalf("got %v", env)
	}
}

func TestRenderSystemdUnit(t *testing.T) {
	unit := RenderSystemdUnit("/home/a b/bin/archivist%$", nil, []EnvVar{
		{"PATH", "/usr/bin:/bin"}, {"LANG", `we"ird\50%`}, {"HOME", "/home/$USER"},
	})
	for _, line := range []string{
		`ExecStart="/home/a b/bin/archivist%%$$" connect --service`,
		`Environment="PATH=/usr/bin:/bin"`,
		`Environment="LANG=we\"ird\\50%%"`,
		`Environment="HOME=/home/$USER"`,
		"Restart=on-failure", "RestartSec=10", "KillMode=mixed", "TimeoutStopSec=30",
		"WantedBy=default.target", "Type=simple",
	} {
		if !strings.Contains(unit, line+"\n") {
			t.Errorf("unit lacks %q:\n%s", line, unit)
		}
	}
	if strings.Contains(unit, "ARCHIVIST_TOKEN") {
		t.Fatal("unit carries ARCHIVIST_TOKEN")
	}
}

func TestRenderLaunchdPlist(t *testing.T) {
	p := RenderLaunchdPlist("/Users/a&b/bin/archivist", "/Users/a&b", "/Users/a&b/.archivist/connect/connect.log", nil,
		[]EnvVar{{"PATH", "/usr/bin:/opt/<x>"}})
	for _, s := range []string{
		"<string>" + Label + "</string>",
		"<string>/Users/a&amp;b/bin/archivist</string>\n\t\t<string>connect</string>\n\t\t<string>--service</string>",
		"<key>PATH</key>\n\t\t<string>/usr/bin:/opt/&lt;x&gt;</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>ThrottleInterval</key>\n\t<integer>10</integer>",
		"<key>StandardErrorPath</key>\n\t<string>/Users/a&amp;b/.archivist/connect/connect.log</string>",
		"<key>StandardOutPath</key>",
	} {
		if !strings.Contains(p, s) {
			t.Errorf("plist lacks %q:\n%s", s, p)
		}
	}
}

func TestSystemdInstallFreshAndRestart(t *testing.T) {
	f := &fakeRunner{answers: map[string][]Result{"systemctl --user is-active": {{ExitCode: 3}}}}
	opts := testOpts(t, f)
	m := newSystemd(opts)
	rep, err := m.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(opts.Home, ".config", "systemd", "user", UnitName)
	if rep.Path != wantPath || rep.Restarted || !rep.LingerOK || rep.Linger != "enabled" {
		t.Fatalf("report %+v", rep)
	}
	b, err := os.ReadFile(wantPath)
	if err != nil || !strings.Contains(string(b), `ExecStart="`+strings.ReplaceAll(testBinary, `\`, `\\`)+`" connect --service`) {
		t.Fatalf("unit %s %v", b, err)
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user is-active --quiet " + UnitName,
		"systemctl --user enable --now " + UnitName,
		"loginctl --no-ask-password enable-linger alice",
	}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v", f.calls)
	}
	assertLog(t, opts.Home)

	// Already active: restarted so a new binary or environment applies.
	f2 := &fakeRunner{answers: map[string][]Result{"loginctl": {{ExitCode: 1, Output: "Access denied"}}}}
	opts.Run = f2.run
	rep, err = newSystemd(opts).Install(context.Background())
	if err != nil || !rep.Restarted || rep.LingerOK || !strings.Contains(rep.Linger, "Access denied") {
		t.Fatalf("%+v %v", rep, err)
	}
	if f2.called("systemctl --user restart "+UnitName) != 1 {
		t.Fatalf("calls %v", f2.calls)
	}
}

func TestSystemdInstallFailureLeavesNothing(t *testing.T) {
	f := &fakeRunner{answers: map[string][]Result{"systemctl --user daemon-reload": {{ExitCode: 1, Output: "Failed to connect to bus"}}}}
	opts := testOpts(t, f)
	m := newSystemd(opts)
	_, err := m.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Fatalf("err %v", err)
	}
	if fileExists(m.Path()) {
		t.Fatal("unit left behind after a failed install")
	}
	f = &fakeRunner{missing: map[string]bool{"systemctl": true}}
	opts.Run = f.run
	if _, err := newSystemd(opts).Install(context.Background()); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("err %v", err)
	}
	if fileExists(m.Path()) {
		t.Fatal("unit left behind without systemctl")
	}
}

func TestSystemdStatusAndUninstall(t *testing.T) {
	f := &fakeRunner{answers: map[string][]Result{"systemctl --user is-active": {{ExitCode: 0}}}}
	opts := testOpts(t, f)
	opts.ConfigHome = filepath.Join(opts.Home, "xdg")
	m := newSystemd(opts)
	st, err := m.Status(context.Background())
	if err != nil || st.Installed || st.Running || len(f.calls) != 0 {
		t.Fatalf("not installed: %+v %v %v", st, err, f.calls)
	}
	if removed, err := m.Uninstall(context.Background()); removed || err != nil {
		t.Fatalf("uninstall nothing: %v %v", removed, err)
	}
	if _, err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(m.Path(), opts.ConfigHome) {
		t.Fatalf("XDG_CONFIG_HOME ignored: %s", m.Path())
	}
	st, err = m.Status(context.Background())
	if err != nil || !st.Installed || !st.Running || st.Manager != "systemd" {
		t.Fatalf("%+v %v", st, err)
	}
	removed, err := m.Uninstall(context.Background())
	if !removed || err != nil || fileExists(m.Path()) || f.called("systemctl --user disable --now "+UnitName) != 1 {
		t.Fatalf("uninstall: %v %v %v", removed, err, f.calls)
	}
}

func TestLaunchdInstallFresh(t *testing.T) {
	f := &fakeRunner{answers: map[string][]Result{"launchctl print": {{ExitCode: 113}}}}
	opts := testOpts(t, f)
	m := newLaunchd(opts)
	rep, err := m.Install(context.Background())
	if err != nil || rep.Restarted || rep.Linger != "" {
		t.Fatalf("%+v %v", rep, err)
	}
	if rep.Path != filepath.Join(opts.Home, "Library", "LaunchAgents", Label+".plist") || !fileExists(rep.Path) {
		t.Fatalf("plist path %s", rep.Path)
	}
	want := []string{
		"launchctl print gui/501/" + Label,
		"launchctl enable gui/501/" + Label,
		"launchctl bootstrap gui/501 " + rep.Path,
	}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v", f.calls)
	}
	assertLog(t, opts.Home)
}

// A loaded job is booted out, awaited until it leaves the domain, then
// bootstrapped again; a bootstrap racing the teardown (error 5) is retried.
func TestLaunchdReinstallWaitsAndRetries(t *testing.T) {
	f := &fakeRunner{answers: map[string][]Result{
		"launchctl print":     {{}, {}, {}, {ExitCode: 113}},
		"launchctl bootstrap": {{ExitCode: 5, Output: "Bootstrap failed: 5: Input/output error"}, {}},
	}}
	opts := testOpts(t, f)
	m := newLaunchd(opts)
	rep, err := m.Install(context.Background())
	if err != nil || !rep.Restarted {
		t.Fatalf("%+v %v %v", rep, err, f.calls)
	}
	if f.called("launchctl bootout gui/501/"+Label) != 1 || f.called("launchctl print") != 4 || f.called("launchctl bootstrap") != 2 {
		t.Fatalf("calls %v", f.calls)
	}
	bootout := indexOf(f.calls, "launchctl bootout")
	bootstrap := indexOf(f.calls, "launchctl bootstrap")
	if bootout < 0 || bootstrap < bootout {
		t.Fatalf("order %v", f.calls)
	}
}

func TestLaunchdBootstrapFailureRemovesNewPlist(t *testing.T) {
	f := &fakeRunner{answers: map[string][]Result{
		"launchctl print":     {{ExitCode: 113}},
		"launchctl bootstrap": {{ExitCode: 125, Output: "Domain does not support specified action"}},
	}}
	opts := testOpts(t, f)
	m := newLaunchd(opts)
	if _, err := m.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "Domain does not support") {
		t.Fatalf("err %v", err)
	}
	if fileExists(m.Path()) || f.called("launchctl bootstrap") != launchdBootstraps {
		t.Fatalf("plist kept or retries wrong: %v", f.calls)
	}
}

func TestLaunchdStatusAndUninstall(t *testing.T) {
	f := &fakeRunner{answers: map[string][]Result{"launchctl print": {{ExitCode: 113}}}}
	opts := testOpts(t, f)
	m := newLaunchd(opts)
	if removed, err := m.Uninstall(context.Background()); removed || err != nil {
		t.Fatalf("%v %v", removed, err)
	}
	if _, err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.answers["launchctl print"] = []Result{{}}
	// Loaded but no daemon pid (it exited 0: superseded, no harness,
	// revoked key): loaded, not running.
	st, err := m.Status(context.Background())
	if err != nil || !st.Installed || !st.Loaded || st.Running || st.Manager != "launchd" {
		t.Fatalf("no pid: %+v %v", st, err)
	}
	// The live daemon's pid (this test process, signal 0): running.
	remove, err := WritePID(opts.Home, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(PIDPath(opts.Home)); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("pid file mode %v", info.Mode().Perm())
	}
	if st, err = m.Status(context.Background()); err != nil || !st.Loaded || !st.Running {
		t.Fatalf("live pid: %+v %v", st, err)
	}
	// A stale pid (the process is gone): not running.
	m.opts.Alive = func(int) bool { return false }
	if st, _ = m.Status(context.Background()); !st.Loaded || st.Running {
		t.Fatalf("stale pid: %+v", st)
	}
	m.opts.Alive = nil
	remove()
	if _, err := os.Stat(PIDPath(opts.Home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pid file kept: %v", err)
	}
	// Not loaded: never running, whatever the pid file says.
	if _, err := WritePID(opts.Home, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	f.answers["launchctl print"] = []Result{{ExitCode: 113}}
	if st, _ = m.Status(context.Background()); st.Loaded || st.Running {
		t.Fatalf("unloaded: %+v", st)
	}
	f.answers["launchctl print"] = []Result{{}}
	removed, err := m.Uninstall(context.Background())
	if !removed || err != nil || fileExists(m.Path()) || f.called("launchctl bootout gui/501/"+Label) != 1 {
		t.Fatalf("%v %v %v", removed, err, f.calls)
	}
}

// A just bootstrapped daemon gets about 5 s to write its pid before the
// loaded job is reported not running; the sleep is injected, nothing waits.
func TestLaunchdStatusWaitsForStartingDaemon(t *testing.T) {
	f := &fakeRunner{answers: map[string][]Result{"launchctl print": {{}}}}
	opts := testOpts(t, f)
	var slept time.Duration
	polls := 0
	opts.Sleep = func(d time.Duration) {
		slept += d
		polls++
		if polls == 3 {
			if _, err := WritePID(opts.Home, os.Getpid()); err != nil {
				t.Fatal(err)
			}
		}
	}
	m := newLaunchd(opts)
	st, err := m.Status(context.Background())
	if err != nil || !st.Loaded || !st.Running || polls != 3 {
		t.Fatalf("pid appears while polling: %+v %v polls=%d", st, err, polls)
	}
	if err := os.Remove(PIDPath(opts.Home)); err != nil {
		t.Fatal(err)
	}
	slept, polls = 0, 0
	m.opts.Sleep = func(d time.Duration) { slept += d; polls++ }
	st, err = m.Status(context.Background())
	if err != nil || !st.Loaded || st.Running {
		t.Fatalf("pid never appears: %+v %v", st, err)
	}
	if slept < 4*time.Second || slept > 6*time.Second {
		t.Fatalf("waited %v in %d polls, want about 5 s", slept, polls)
	}
}

// WritePID's remover leaves a pid file another daemon has since replaced.
func TestWritePIDRemoverKeepsNewerDaemon(t *testing.T) {
	home := t.TempDir()
	remove, err := WritePID(home, 111)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WritePID(home, 222); err != nil {
		t.Fatal(err)
	}
	remove()
	if readPID(home) != 222 {
		t.Fatalf("pid %d", readPID(home))
	}
	if processAlive(0) || processAlive(-1) {
		t.Fatal("non-positive pid alive")
	}
}

func TestOpenLogRotatesAndKeepsMode(t *testing.T) {
	home := t.TempDir()
	path := LogPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, maxLogBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenLog(home)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("12:00:00.000 connect: first\n12:00:01.000 connect: last line\n\n")
	_ = f.Close()
	if !fileExists(path + ".1") {
		t.Fatal("not rotated")
	}
	assertLog(t, home)
	if got := LastLogLine(home); got != "12:00:01.000 connect: last line" {
		t.Fatalf("last line %q", got)
	}
	if LastLogLine(t.TempDir()) != "" {
		t.Fatal("missing log has a last line")
	}
}

func TestNewRequiresAbsoluteHome(t *testing.T) {
	if _, err := New(Options{Home: "relative"}); err == nil {
		t.Fatal("relative home accepted")
	}
	m, err := New(Options{Home: t.TempDir()})
	switch runtime.GOOS {
	case "linux":
		if err != nil || m.Name() != "systemd" {
			t.Fatalf("%v %v", m, err)
		}
	case "darwin":
		if err != nil || m.Name() != "launchd" {
			t.Fatalf("%v %v", m, err)
		}
	case "windows":
		if err != nil || m.Name() != "Task Scheduler" {
			t.Fatalf("%v %v", m, err)
		}
	default:
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%v", err)
		}
	}
}

func assertLog(t *testing.T, home string) {
	t.Helper()
	info, err := os.Stat(LogPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode %v", info.Mode().Perm())
	}
	dir, _ := os.Stat(filepath.Dir(LogPath(home)))
	if runtime.GOOS != "windows" && dir.Mode().Perm() != 0o700 && dir.Mode().Perm() != 0o755 {
		t.Fatalf("log dir mode %v", dir.Mode().Perm())
	}
}

func indexOf(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// Story 78.32: the install's daemon flags (--max-permission <mode>) follow
// `connect --service` in both service definitions, and ServiceArgs reads
// them back, quoting included.
func TestRenderServiceArgsRoundTrip(t *testing.T) {
	args := []string{"--max-permission", "full_auto"}
	unit := RenderSystemdUnit("/home/a b/bin/archivist%$", args, nil)
	if !strings.Contains(unit, `ExecStart="/home/a b/bin/archivist%%$$" connect --service --max-permission full_auto`+"\n") {
		t.Fatalf("unit:\n%s", unit)
	}
	plist := RenderLaunchdPlist("/Users/a&b/bin/archivist", "/Users/a&b", "/Users/a&b/l.log", args, nil)
	if !strings.Contains(plist, "<string>--service</string>\n\t\t<string>--max-permission</string>\n\t\t<string>full_auto</string>\n\t</array>") {
		t.Fatalf("plist:\n%s", plist)
	}
	for _, c := range [][]string{nil, args, {`we"ird 50%$\x`, "<&>", "plain"}} {
		for name, content := range map[string]string{
			"systemd": RenderSystemdUnit("/home/a b/bin/archivist%$", c, []EnvVar{{"HOME", "/h"}}),
			"launchd": RenderLaunchdPlist("/Users/a&b/bin/archivist", "/Users/a&b", "/l", c, []EnvVar{{"HOME", "/h"}}),
		} {
			if got := ServiceArgs(content); strings.Join(got, "|") != strings.Join(c, "|") || len(got) != len(c) {
				t.Errorf("%s %q: read back %q", name, c, got)
			}
		}
	}
	for _, content := range []string{"", "ExecStart=/bin/true\n", "ExecStart=/x other --service --max-permission ask\n", "<plist><dict></dict></plist>"} {
		if got := ServiceArgs(content); got != nil {
			t.Errorf("%q: %q", content, got)
		}
	}
}

// The installed flags are written and Status reports them (systemd and
// launchd).
func TestInstallWritesArgsAndStatusReadsThem(t *testing.T) {
	f := &fakeRunner{answers: map[string][]Result{"launchctl print": {{ExitCode: 113}}}}
	for _, m := range []Manager{
		func() Manager {
			o := testOpts(t, f)
			o.Args = []string{"--max-permission", "read_only"}
			return newSystemd(o)
		}(),
		func() Manager {
			o := testOpts(t, f)
			o.Args = []string{"--max-permission", "read_only"}
			return newLaunchd(o)
		}(),
	} {
		if _, err := m.Install(context.Background()); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(m.Path())
		if err != nil || !strings.Contains(string(b), "read_only") {
			t.Fatalf("%s: %v\n%s", m.Name(), err, b)
		}
		st, err := m.Status(context.Background())
		if err != nil || strings.Join(st.Args, " ") != "--max-permission read_only" {
			t.Fatalf("%s: status args %q, %v", m.Name(), st.Args, err)
		}
	}
	// Without flags the definition is unchanged and Status reports none.
	o := testOpts(t, f)
	m := newSystemd(o)
	if _, err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.Status(context.Background()); st.Args != nil {
		t.Fatalf("args %q", st.Args)
	}
}
