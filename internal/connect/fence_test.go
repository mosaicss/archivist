package connect

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Story 94.5: the fence. One locked path list per launch drives the Claude
// Code settings, the Codex permission profiles and the daemon backstop.

// fenceFixture is a home with the state tree and a few entries.
type fenceFixture struct {
	home, state, work, cwd, other string
}

func newFenceFixture(t *testing.T) fenceFixture {
	t.Helper()
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	st, err := OpenStore(filepath.Join(home, ".archivist", "connect"))
	if err != nil {
		t.Fatal(err)
	}
	f := fenceFixture{home: home, state: st.Dir(), work: st.WorkDir(),
		cwd: filepath.Join(st.WorkDir(), cwdPrefix+"own"), other: filepath.Join(st.WorkDir(), cwdPrefix+"other")}
	for _, d := range []string{f.cwd, f.other, filepath.Join(home, ".archivist", "cache")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{filepath.Join(f.state, "MosaicArchivistConnect.xml"), filepath.Join(f.state, "connect.log.1")} {
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// j0 joins a path under a base.
func j0(base string, parts ...string) string {
	return filepath.Join(append([]string{base}, parts...)...)
}

func TestFenceListShape(t *testing.T) {
	f := newFenceFixture(t)
	secrets := filepath.Join(t.TempDir(), "secrets")
	if r, err := filepath.EvalSymlinks(filepath.Dir(secrets)); err == nil {
		secrets = filepath.Join(r, "secrets")
	}
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	linked := runtime.GOOS != "windows"
	if linked {
		if err := os.Symlink(secrets, filepath.Join(f.home, ".gnupg")); err != nil {
			t.Fatal(err)
		}
	}
	xdg := filepath.Join(f.home, "xdg")
	claudeDir := filepath.Join(f.home, "claude-b")
	codexDir := filepath.Join(f.home, "codex-owner")
	// Claude Code config folders: this session's own projects entry (as
	// written; the fixture has no links) stays readable, everything else
	// in them is locked entry by entry.
	own := claudeProjectName(f.cwd)
	for _, d := range []string{claudeDir, j0(f.home, ".claude")} {
		for _, sub := range []string{"projects/" + own + "/sess/tool-results", "projects/-other-project", "plans", "sessions"} {
			if err := os.MkdirAll(filepath.Join(d, filepath.FromSlash(sub)), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(d, "settings.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"XDG_CONFIG_HOME=" + xdg, "CLAUDE_CONFIG_DIR=" + claudeDir, "CODEX_HOME=" + codexDir}
	fe, err := newFence(fenceSpec{Home: f.home, Env: env, StateDir: f.state, Cwd: f.cwd})
	if err != nil {
		t.Fatal(err)
	}
	j := func(parts ...string) string { return filepath.Join(append([]string{f.home}, parts...)...) }
	want := []string{
		j(".ssh"), j(".aws"), j(".config", "gcloud"), j(".azure"), j(".kube"), j(".docker", "config.json"),
		j(".netrc"), j(".npmrc"), j(".pypirc"), j(".claude.json"), j(".codex"),
		filepath.Join(xdg, "gcloud"), codexDir,
		// ~/.archivist: every entry but connect, plus the fixed names even
		// when absent.
		j(".archivist", "credentials"), j(".archivist", "install-channel"), j(".archivist", "keychain"), j(".archivist", "cache"),
		// connect: every entry but work, plus its fixed names.
		filepath.Join(f.state, "sessions"), filepath.Join(f.state, "run"), filepath.Join(f.state, "codex-home"),
		filepath.Join(f.state, "connect.log"), filepath.Join(f.state, "service.pid"), filepath.Join(f.state, "service.env"),
		filepath.Join(f.state, BrowsersFile), filepath.Join(f.state, "paused"),
		filepath.Join(f.state, "connect.log.1"), filepath.Join(f.state, "MosaicArchivistConnect.xml"),
		// work: every session folder but this one.
		f.other,
	}
	for _, rel := range fenceHomeNames(runtime.GOOS)[12:] {
		want = append(want, j(filepath.FromSlash(rel)))
		// Linux browser and password names also under $XDG_CONFIG_HOME.
		if rest, ok := strings.CutPrefix(rel, ".config/"); ok {
			want = append(want, filepath.Join(xdg, filepath.FromSlash(rest)))
		}
	}
	for _, d := range []string{claudeDir, j(".claude")} {
		want = append(want, filepath.Join(d, ".credentials.json"), filepath.Join(d, "settings.json"),
			filepath.Join(d, "plans"), filepath.Join(d, "sessions"))
		for _, open := range []string{d, filepath.Join(d, "projects"), filepath.Join(d, "projects", own),
			filepath.Join(d, "projects", own, "sess", "tool-results")} {
			if slices.Contains(fe.paths, open) || slices.Contains(fe.real, open) || slices.Contains(fe.ruleOnly, open) {
				t.Errorf("locked %s, the session's own Claude Code data", open)
			}
		}
		// Other projects entries get only Read and Edit rules; the sandbox
		// denies projects and reopens the session's own entry.
		if other := filepath.Join(d, "projects", "-other-project"); !slices.Contains(fe.ruleOnly, other) || slices.Contains(fe.paths, other) {
			t.Errorf("projects entry %s: ruleOnly %v", other, fe.ruleOnly)
		}
		if !slices.Contains(fe.work, filepath.Join(d, "projects")) || !slices.Contains(fe.own, filepath.Join(d, "projects", own)) {
			t.Errorf("sandbox projects deny %v, reopen %v", fe.work, fe.own)
		}
	}
	for _, p := range want {
		if !slices.Contains(fe.paths, p) {
			t.Errorf("paths lack %s", p)
		}
		if !slices.Contains(fe.real, resolvePath(p)) {
			t.Errorf("real paths lack %s", resolvePath(p))
		}
	}
	for _, p := range []string{f.cwd, f.work, f.state, j(".archivist"), f.home} {
		if slices.Contains(fe.paths, p) || slices.Contains(fe.real, p) {
			t.Errorf("locked %s, which holds or is the session folder", p)
		}
	}
	if linked {
		// The symlinked folder is listed as written and resolved; Codex
		// gets only the resolved one.
		if !slices.Contains(fe.paths, j(".gnupg")) || !slices.Contains(fe.paths, secrets) {
			t.Errorf("paths lack a form of the linked .gnupg: %v", fe.paths)
		}
		if slices.Contains(fe.real, j(".gnupg")) || !slices.Contains(fe.real, secrets) {
			t.Errorf("real paths %v", fe.real)
		}
	}
	for _, p := range append(append([]string(nil), fe.paths...), fe.real...) {
		if !filepath.IsAbs(p) {
			t.Errorf("relative entry %s", p)
		}
	}

	// An entry holding the session folder is never listed (it would hide
	// the folder from the session).
	fe2, err := newFence(fenceSpec{Home: f.home, Env: []string{"CLAUDE_CONFIG_DIR=" + f.work}, StateDir: f.state, Cwd: f.cwd})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(fe2.paths, f.work) {
		t.Errorf("an entry holding the session folder is listed")
	}

	// A session folder from before Story 94.5 (in the temp directory):
	// every work entry is locked.
	legacy := filepath.Join(t.TempDir(), cwdPrefix+"legacy")
	fe3, err := newFence(fenceSpec{Home: f.home, StateDir: f.state, Cwd: legacy})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(fe3.paths, f.cwd) || !slices.Contains(fe3.paths, f.other) {
		t.Errorf("legacy session: work entries %v", fe3.paths)
	}
	// A harness binary inside a locked folder (the standalone Codex install
	// under ~/.codex/packages) stays reachable: that folder is locked entry
	// by entry, without the entry holding the binary.
	bin := filepath.Join(f.home, ".codex", "packages", "standalone", "releases", "1", "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{bin, filepath.Join(f.home, ".codex", "auth.json"), filepath.Join(f.home, ".codex", "config.toml")} {
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(f.home, "codex-link")
	if runtime.GOOS != "windows" {
		if err := os.Symlink(bin, link); err != nil {
			t.Fatal(err)
		}
	} else {
		link = bin
	}
	fe4, err := newFence(fenceSpec{Home: f.home, StateDir: f.state, Cwd: f.cwd, Keep: []string{link}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{j(".codex"), j(".codex", "packages")} {
		if slices.Contains(fe4.paths, p) || slices.Contains(fe4.real, p) {
			t.Errorf("locked %s, which holds the harness binary", p)
		}
	}
	for _, p := range []string{j(".codex", "auth.json"), j(".codex", "config.toml"), j(".ssh")} {
		if !slices.Contains(fe4.paths, p) {
			t.Errorf("paths lack %s next to the harness binary", p)
		}
	}
	if _, err := newFence(fenceSpec{Home: "", StateDir: f.state, Cwd: f.cwd}); err == nil {
		t.Error("a fence without a home was built")
	}
	if _, err := newFence(fenceSpec{Home: f.home, StateDir: f.state, Cwd: ""}); err == nil {
		t.Error("a fence without a session folder was built")
	}
}

func TestFenceHomeNamesPerOS(t *testing.T) {
	common := []string{".ssh", ".aws", ".config/gcloud", ".azure", ".kube", ".docker/config.json", ".gnupg",
		".netrc", ".npmrc", ".pypirc", ".claude.json", ".codex"}
	for goos, extra := range map[string][]string{
		"darwin": {"Library/Application Support/Google/Chrome", "Library/Application Support/Firefox",
			"Library/Application Support/1Password", "Library/Application Support/Bitwarden", "Library/Keychains",
			"Library/Cookies", "Library/Safari", "Library/Group Containers/2BUA8C4S2C.com.1password"},
		"linux": {".mozilla", ".config/google-chrome", ".config/chromium", ".config/BraveSoftware", ".config/microsoft-edge",
			".config/1Password", ".config/Bitwarden", ".config/keepassxc", ".password-store", ".local/share/keyrings"},
		"windows": {"AppData/Local/Google/Chrome/User Data", "AppData/Local/Microsoft/Edge/User Data", "AppData/Local/BraveSoftware",
			"AppData/Roaming/Mozilla/Firefox", "AppData/Local/1Password", "AppData/Roaming/Bitwarden", "AppData/Roaming/gcloud"},
	} {
		names := fenceHomeNames(goos)
		if !slices.Equal(names[:len(common)], common) {
			t.Errorf("%s: common names %v", goos, names[:len(common)])
		}
		for _, n := range extra {
			if !slices.Contains(names, n) {
				t.Errorf("%s lacks %s", goos, n)
			}
		}
	}
}

func TestFenceClaudeSettingsPerOS(t *testing.T) {
	paths := []string{"/home/u/.ssh", "/home/u/.archivist/credentials"}
	work := []string{"/home/u/.archivist/connect/work"}
	own := []string{"/home/u/.archivist/connect/work/archivist-connect-own"}
	for _, goos := range []string{"linux", "darwin"} {
		raw := claudeSettings(goos, paths, nil, work, own)
		if !strings.HasPrefix(raw, `{"useAutoModeDuringPlan":false,"plansDirectory":".plans","sandbox":{"enabled":true,"failIfUnavailable":true,`+
			`"allowUnsandboxedCommands":false,"autoAllowBashIfSandboxed":false,`) {
			t.Fatalf("%s: settings %s", goos, raw)
		}
		var s struct {
			Sandbox struct {
				Filesystem  map[string][]string `json:"filesystem"`
				Credentials struct {
					Files []map[string]string `json:"files"`
				} `json:"credentials"`
				Network map[string][]string `json:"network"`
			} `json:"sandbox"`
			Permissions map[string][]string `json:"permissions"`
		}
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			t.Fatal(err)
		}
		// The sandbox also denies the work folder and reopens the session's
		// own (folders created later stay closed to shell commands); the
		// work folder is never a credentials entry or a deny rule.
		if !slices.Equal(s.Sandbox.Filesystem["denyRead"], append(append([]string(nil), paths...), work...)) ||
			!slices.Equal(s.Sandbox.Filesystem["allowRead"], own) || len(s.Sandbox.Filesystem) != 2 {
			t.Errorf("%s: filesystem %v", goos, s.Sandbox.Filesystem)
		}
		if strings.Contains(raw, `"Read(//home/u/.archivist/connect/work)"`) || strings.Count(raw, work[0]+`"`) != 1 {
			t.Errorf("%s: the work folder outside the sandbox read list: %s", goos, raw)
		}
		if len(s.Sandbox.Credentials.Files) != 2 || s.Sandbox.Credentials.Files[1]["path"] != paths[1] ||
			s.Sandbox.Credentials.Files[1]["mode"] != "deny" {
			t.Errorf("%s: credentials %v", goos, s.Sandbox.Credentials.Files)
		}
		if !slices.Equal(s.Sandbox.Network["allowedDomains"], []string{"*"}) {
			t.Errorf("%s: network %v", goos, s.Sandbox.Network)
		}
		want := []string{"Read(//home/u/.ssh)", "Read(//home/u/.ssh/**)", "Edit(//home/u/.ssh)", "Edit(//home/u/.ssh/**)",
			"Read(//home/u/.archivist/credentials)", "Read(//home/u/.archivist/credentials/**)",
			"Edit(//home/u/.archivist/credentials)", "Edit(//home/u/.archivist/credentials/**)"}
		if !slices.Equal(s.Permissions["deny"], want) || len(s.Permissions) != 1 {
			t.Errorf("%s: permissions %v", goos, s.Permissions)
		}
		for _, banned := range []string{"allowNestedUserNamespaces", "excludedCommands", "WebFetch"} {
			if strings.Contains(raw, banned) {
				t.Errorf("%s: settings carry %s", goos, banned)
			}
		}
	}
	// Native Windows: no shell sandbox; the deny rules in POSIX form, and
	// Bash and PowerShell always ask.
	raw := claudeSettings("windows", []string{`C:\Users\u\.ssh`}, nil, []string{`C:\Users\u\w`}, []string{`C:\Users\u\w\s`})
	want := `{"useAutoModeDuringPlan":false,"plansDirectory":".plans","permissions":{"deny":["Read(//c/Users/u/.ssh)","Read(//c/Users/u/.ssh/**)",` +
		`"Edit(//c/Users/u/.ssh)","Edit(//c/Users/u/.ssh/**)"],"ask":["Bash","PowerShell"]}}`
	if raw != want {
		t.Fatalf("windows settings\n%s\nwant\n%s", raw, want)
	}
}

func TestFenceCodexProfiles(t *testing.T) {
	real := []string{"/home/u/.ssh", "/home/u/.archivist/credentials"}
	set := map[string]string{}
	for _, kv := range codexProfileConfig(real) {
		set[kv[0]] = kv[1]
	}
	deny := `"/home/u/.ssh"="deny", "/home/u/.archivist/credentials"="deny"`
	want := map[string]string{
		"default_permissions":                         `"archivist-read"`,
		"permissions.archivist-read.extends":          `":read-only"`,
		"permissions.archivist-read.filesystem":       "{" + deny + "}",
		"permissions.archivist-read.network.enabled":  "false",
		"permissions.archivist-write.extends":         `":workspace"`,
		"permissions.archivist-write.filesystem":      "{" + deny + `, ":slash_tmp"="read"}`,
		"permissions.archivist-write.network.enabled": "false",
		"permissions.archivist-full.extends":          `":workspace"`,
		"permissions.archivist-full.filesystem":       "{" + deny + `, ":slash_tmp"="read"}`,
		"permissions.archivist-full.network.enabled":  "true",
	}
	if len(set) != len(want) {
		t.Errorf("pairs %v", set)
	}
	for k, v := range want {
		if set[k] != v {
			t.Errorf("-c %s=%s, want %s", k, set[k], v)
		}
	}
	for _, m := range Modes {
		id := codexProfile(m)
		if _, ok := set["permissions."+id+".extends"]; !ok {
			t.Errorf("%s: profile %s is not defined", m, id)
		}
	}
}

func TestFenceBackstopForms(t *testing.T) {
	f := &fence{goos: "linux", home: "/home/u", paths: []string{"/home/u/.ssh", "/home/u/.archivist/credentials",
		"/home/u/.archivist/connect/work/archivist-connect-other", "/srv/claude-b"}}
	refuse := func(text string) bool {
		b, _ := json.Marshal(map[string]any{"command": text, "nested": []any{map[string]any{"n": 1}}})
		return f.fenceRefusal(b)
	}
	for _, in := range []string{
		"cat /home/u/.ssh/id_rsa", "cat ~/.ssh/id_rsa", "cat $HOME/.ssh/id_rsa", "cat ${HOME}/.ssh/id_rsa",
		`cat "$HOME"/.ssh/id_rsa`, "cat ~/'.ssh'/id_rsa", "cat /home/u//.ssh/./id_rsa", "ls ~/.ssh", "ls ~/.ssh*",
		"cat ~/.archivist/credentials;echo", "cat /srv/claude-b/.credentials.json",
		"ls ~/.archivist/connect/work/archivist-connect-other/notes.md",
	} {
		if !refuse(in) {
			t.Errorf("%q not refused", in)
		}
	}
	for _, in := range []string{
		"cat ~/.sshx", "cat ~/.ssh.bak/key", "cat ~/.ssh-old", "ls ~/.archivist/credentials2",
		"cat /home/u/.archivist/connect/work/archivist-connect-own/notes.md", "ls ~/.archivist/connect/work/archivist-connect-othe",
		"cat /home/u/docs/report.md", "echo ssh", "cat /home/user/.ssh/id_rsa",
	} {
		if refuse(in) {
			t.Errorf("%q refused", in)
		}
	}
	// Every string argument counts (a file path field), plain text too.
	if !f.fenceRefusal(json.RawMessage(`{"file_path":"/home/u/.ssh/config","limit":10}`)) {
		t.Error("Read file_path not refused")
	}
	if !f.fenceRefusal(json.RawMessage(`[{"path":"/home/u/.archivist/credentials"}]`)) {
		t.Error("nested change path not refused")
	}
	if !f.fenceRefusal(json.RawMessage(`not json ~/.ssh/id_rsa`)) {
		t.Error("plain text not refused")
	}
	if f.fenceRefusal(nil) || (*fence)(nil).fenceRefusal(json.RawMessage(`"~/.ssh"`)) {
		t.Error("empty input or no fence refused")
	}

	// Windows: any case, either slash, %USERPROFILE%.
	w := &fence{goos: "windows", home: `C:\Users\U`, paths: []string{`C:\Users\U\.ssh`, `C:\Users\U\.archivist\credentials`}}
	wr := func(text string) bool {
		b, _ := json.Marshal(map[string]string{"command": text})
		return w.fenceRefusal(b)
	}
	for _, in := range []string{
		`type C:\Users\U\.ssh\id_rsa`, `type c:\users\u\.SSH\id_rsa`, `cat C:/Users/U/.ssh/id_rsa`,
		`type %USERPROFILE%\.ssh\id_rsa`, `type %userprofile%\.archivist\credentials`, `cat ~\.ssh\id_rsa`,
		`Get-Content $HOME\.ssh\id_rsa`, `Get-Content ${HOME}/.ssh/id_rsa`, `Get-Content $env:USERPROFILE\.ssh\id_rsa`,
	} {
		if !wr(in) {
			t.Errorf("windows %q not refused", in)
		}
	}
	for _, in := range []string{`type C:\Users\U\.sshx`, `type C:\Users\U\Documents\a.txt`, `type %USERPROFILE%\.ssh2`} {
		if wr(in) {
			t.Errorf("windows %q refused", in)
		}
	}
}

func TestFenceInitExtensions(t *testing.T) {
	builtin := `[{"name":"cc-plugin-agents-md","path":"builtin","source":"cc-plugin-agents-md@builtin"},` +
		`{"name":"cc-plugin-telemetry","path":"builtin","source":"cc-plugin-telemetry@builtin"}]`
	archivist := `[{"name":"archivist","status":"connected"}]`
	for name, c := range map[string]struct {
		servers, plugins string
		want             string
	}{
		"builtins and archivist":     {archivist, builtin, ""},
		"none":                       {`[]`, `[]`, ""},
		"absent":                     {``, `null`, ""},
		"pending archivist":          {`[{"name":"archivist","status":"pending"}]`, builtin, ""},
		"second server":              {`[{"name":"archivist","status":"connected"},{"name":"other","status":"connected"}]`, builtin, `MCP server "other"`},
		"user plugin":                {archivist, `[{"name":"helper","path":"/home/u/.claude/plugins/helper","source":"helper@market"}]`, `plugin "helper"`},
		"builtin path, other source": {archivist, `[{"name":"x","path":"builtin","source":"x@market"}]`, `plugin "x"`},
		"builtin source, other path": {archivist, `[{"name":"y","path":"/tmp/y","source":"y@builtin"}]`, `plugin "y"`},
		"unreadable servers":         {`{"archivist":{}}`, builtin, "MCP server list is unreadable"},
		"unreadable plugins":         {archivist, `"x"`, "plugin list is unreadable"},
	} {
		p := initExtensionsProblem(json.RawMessage(c.servers), json.RawMessage(c.plugins))
		if (c.want == "") != (p == "") || !strings.Contains(p, c.want) {
			t.Errorf("%s: %q, want %q", name, p, c.want)
		}
	}
}

func TestFenceSessionFolderChecks(t *testing.T) {
	skipOnWindows(t, "owner and mode checks are Unix only")
	st, err := OpenStore(filepath.Join(t.TempDir(), "connect"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{st.WorkDir(), filepath.Join(st.Dir(), codexHomeDir)} {
		if info, err := os.Stat(d); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, info, err)
		}
	}
	good := filepath.Join(st.WorkDir(), cwdPrefix+"good")
	if err := st.checkSessionCwd(good); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing folder: %v", err)
	}
	if err := st.ensureSessionCwd(good); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(good); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("created folder %v %v", info, err)
	}
	target := t.TempDir()
	link := filepath.Join(st.WorkDir(), cwdPrefix+"link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(st.WorkDir(), cwdPrefix+"loose")
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(st.WorkDir(), cwdPrefix+"file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "nested", cwdPrefix+"x")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"symlink": link, "mode 0755": loose, "file": file, "wrong parent": elsewhere,
		"no prefix": filepath.Join(st.WorkDir(), "projects")} {
		if err := st.checkSessionCwd(dir); !errors.Is(err, errCwdUnusable) {
			t.Errorf("%s: %v", name, err)
		}
		if err := st.ensureSessionCwd(dir); !errors.Is(err, errCwdUnusable) {
			t.Errorf("%s ensured: %v", name, err)
		}
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Error("the link target was written")
	}
}

// ─── daemon rows ────────────────────────────────────────────────────────────

// fakeSettings is the --settings JSON of the n-th fake Claude run.
func (h *harness) fakeSettings(n int) map[string]any {
	h.t.Helper()
	runs := fakeRuns(h.t, h.home)
	if len(runs) <= n {
		h.t.Fatalf("%d runs", len(runs))
	}
	// The settings travel as a file in the session's private run folder.
	args := runs[n].Args
	i := slices.Index(args, "--settings")
	if i < 0 || filepath.Base(args[i+1]) != claudeSettingsFileName ||
		filepath.Dir(args[i+1]) != filepath.Join(h.home, ".archivist", "connect", "run", filepath.Base(filepath.Dir(args[i+1]))) {
		h.t.Fatalf("--settings in %v", args)
	}
	var s map[string]any
	if json.Unmarshal([]byte(runs[n].Settings), &s) != nil {
		h.t.Fatalf("settings %q", runs[n].Settings)
	}
	return s
}

func TestFenceClaudeLaunchSettings(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo hi")
	h.waitFinishes(sid, 1)
	rec := h.record(sid)
	work := filepath.Join(h.home, ".archivist", "connect", "work")
	if rw, err := filepath.EvalSymlinks(work); err == nil {
		work = rw
	}
	if filepath.Dir(rec.Cwd) != work || !strings.HasPrefix(filepath.Base(rec.Cwd), cwdPrefix) {
		t.Fatalf("session folder %s, not in %s", rec.Cwd, work)
	}
	if info, err := os.Lstat(rec.Cwd); err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
		t.Fatalf("session folder %v %v", info, err)
	}
	s := h.fakeSettings(0)
	if s["useAutoModeDuringPlan"] != false {
		t.Fatalf("settings %v", s)
	}
	perms, _ := s["permissions"].(map[string]any)
	deny, _ := perms["deny"].([]any)
	cred := claudeRulePath(runtime.GOOS, filepath.Join(h.home, ".archivist", "credentials"))
	if !slices.Contains(deny, any("Read("+cred+")")) || !slices.Contains(deny, any("Edit("+cred+"/**)")) {
		t.Fatalf("deny rules %v", deny)
	}
	for _, r := range deny {
		if strings.Contains(r.(string), filepath.Base(rec.Cwd)) {
			t.Fatalf("the session folder is denied: %s", r)
		}
	}
	sb, hasSandbox := s["sandbox"].(map[string]any)
	if runtime.GOOS == "windows" {
		if hasSandbox || !slices.Equal(perms["ask"].([]any), []any{"Bash", "PowerShell"}) {
			t.Fatalf("windows settings %v", s)
		}
		return
	}
	if !hasSandbox || sb["enabled"] != true || sb["failIfUnavailable"] != true || sb["allowUnsandboxedCommands"] != false ||
		sb["autoAllowBashIfSandboxed"] != false {
		t.Fatalf("sandbox %v", sb)
	}
	denyRead := sb["filesystem"].(map[string]any)["denyRead"].([]any)
	for _, want := range []string{filepath.Join(h.home, ".ssh"), filepath.Join(h.home, ".archivist", "credentials"),
		filepath.Join(h.home, ".archivist", "connect", "run")} {
		if !slices.Contains(denyRead, any(want)) {
			t.Errorf("denyRead lacks %s", want)
		}
	}
	// The work folder is denied to shell reads with the session's own
	// folder reopened; no other entry holds the session folder, and no
	// credentials entry does.
	allowRead, _ := sb["filesystem"].(map[string]any)["allowRead"].([]any)
	if !slices.Contains(denyRead, any(work)) || !slices.Contains(allowRead, any(rec.Cwd)) {
		t.Errorf("work deny %v, allowRead %v", denyRead, allowRead)
	}
	for _, p := range denyRead {
		if p != work && (pathInside(rec.Cwd, p.(string)) || p == rec.Cwd) {
			t.Errorf("denyRead holds the session folder: %s", p)
		}
	}
	for _, c := range sb["credentials"].(map[string]any)["files"].([]any) {
		if p := c.(map[string]any)["path"].(string); p == work || pathInside(rec.Cwd, p) || p == rec.Cwd {
			t.Errorf("a credentials entry holds the session folder: %s", p)
		}
	}
	if s["plansDirectory"] != ".plans" {
		t.Errorf("plansDirectory %v", s["plansDirectory"])
	}
}

func TestFenceClaudeBackstopDeniesWithoutCard(t *testing.T) {
	h := newHarness(t, nil)
	h.maxMode = ModeFullAuto
	h.start()
	for _, mode := range Modes {
		sid := h.startWith("claude", "bash cat $HOME/.ssh/id_rsa", map[string]any{"mode": string(mode)})
		h.waitFinishes(sid, 1)
		h.message(sid, "edit "+filepath.Join(h.home, ".archivist", "credentials"))
		h.waitFinishes(sid, 2)
		if h.cards(sid) != 0 || strings.Count(h.relay.text(sid), "denied: "+fenceDenied) != 2 {
			t.Fatalf("%s: cards %d, text %q", mode, h.cards(sid), h.relay.text(sid))
		}
		// Work in the session folder is unchanged: a card in ask.
		if mode == ModeAsk {
			h.message(sid, "bash ls")
			h.approve(sid, 1, "allow", "user", "allow_once")
			h.waitFinishes(sid, 3)
		}
	}
}

func TestFenceCodexBackstopDeclinesWithoutCard(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("cmd cat ~/.ssh/id_rsa")
	h.waitFinishes(sid, 1)
	h.message(sid, "cmd cat "+filepath.Join(h.home, ".archivist", "credentials"))
	h.waitFinishes(sid, 2)
	if h.cards(sid) != 0 || strings.Count(h.relay.text(sid), "declined cat") != 2 {
		t.Fatalf("cards %d, text %q", h.cards(sid), h.relay.text(sid))
	}
	h.message(sid, "cmd ls")
	h.approve(sid, 1, "allow", "user", "allow_once")
	h.waitFinishes(sid, 3)

	// A file change whose path is only in the cached change list (the
	// request names none) is declined too, in auto_edits as in ask.
	for _, mode := range []Mode{ModeAsk, ModeAutoEdits} {
		sid := h.startWith("codex", "file ../../../credentials", map[string]any{"mode": string(mode)})
		h.waitFinishes(sid, 1)
		if h.cards(sid) != 0 || !strings.Contains(h.relay.text(sid), "file declined") {
			t.Fatalf("%s: cards %d, text %q", mode, h.cards(sid), h.relay.text(sid))
		}
		if _, err := os.Stat(filepath.Join(h.home, ".archivist", "credentials")); !os.IsNotExist(err) {
			t.Fatalf("%s: the change was applied: %v", mode, err)
		}
	}
}

func TestFenceCodexPermissionProfiles(t *testing.T) {
	if codexLegacySandbox {
		t.Skip("Windows keeps the legacy sandbox policy")
	}
	h := newCodexHarness(t, nil)
	h.maxMode = ModeFullAuto
	h.start()
	sid := h.startCodexSession("echo one")
	h.waitFinishes(sid, 1)
	th := h.threadStart()
	if th["permissions"] != codexProfileWrite {
		t.Fatalf("thread/start %v", th)
	}
	if _, ok := th["sandbox"]; ok {
		t.Fatalf("thread/start sends a sandbox: %v", th)
	}
	h.setMode(sid, ModeFullAuto)
	h.message(sid, "echo two")
	h.waitFinishes(sid, 2)
	h.setMode(sid, ModeReadOnly)
	h.message(sid, "echo three")
	h.waitFinishes(sid, 3)
	var got []string
	for _, p := range h.turnStarts() {
		if _, ok := p["sandboxPolicy"]; ok {
			t.Fatalf("turn/start sends a sandbox policy: %v", p)
		}
		got = append(got, p["permissions"].(string))
	}
	if !slices.Equal(got, []string{codexProfileWrite, codexProfileFull, codexProfileRead}) {
		t.Fatalf("turn/start profiles %v", got)
	}
	runs := codexRunArgs(t, h.home)
	if !slices.Contains(runs, `default_permissions="archivist-read"`) || slices.ContainsFunc(runs, func(a string) bool {
		return strings.HasPrefix(a, "sandbox_workspace_write.")
	}) {
		t.Fatalf("argv %v", runs)
	}

	// A thread in another profile fails the proof.
	h2 := newCodexHarness(t, map[string]any{"activeProfile": ":workspace"})
	h2.start()
	sid2 := h2.startCodexSession("echo x")
	h2.relay.waitStatus(t, sid2, "failed", 1)
	if errs := h2.errorTexts(sid2); len(errs) != 1 || !strings.Contains(errs[0], `permission profile ":workspace", not "archivist-write"`) {
		t.Fatalf("errors %v", errs)
	}
}

// codexRunArgs is the argv of a fake Codex session app-server run (not a
// detection or sign-in run).
func codexRunArgs(t *testing.T, home string) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(home, ".fakecodex", "runs"))
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(home, ".fakecodex", "runs", e.Name()))
		if err != nil {
			continue
		}
		var run struct {
			Args []string `json:"args"`
		}
		if json.Unmarshal(b, &run) == nil && slices.Contains(run.Args, "project_root_markers=[]") {
			return run.Args
		}
	}
	t.Fatal("no app-server run")
	return nil
}

func TestFenceInitFrameExtensionsFailClosed(t *testing.T) {
	for name, cfg := range map[string]map[string]any{
		"plugin": {"extraInitPlugins": []any{map[string]any{"name": "helper", "path": "/p/helper", "source": "helper@market"}}},
		"server": {"extraInitMCPServers": []any{"other"}},
	} {
		h := newHarness(t, cfg)
		h.start()
		sid := h.startSession("echo should not reach the relay")
		h.relay.waitStatus(t, sid, "failed", 1)
		time.Sleep(200 * time.Millisecond)
		if strings.Contains(h.relay.text(sid), "should not reach") || slices.Contains(h.relay.statuses(sid), "running") {
			t.Fatalf("%s: the session ran: %q %v", name, h.relay.text(sid), h.relay.statuses(sid))
		}
		if errs := h.errorTexts(sid); len(errs) != 1 || !strings.Contains(errs[0], "extensions archivist connect allows") {
			t.Fatalf("%s: errors %v", name, errs)
		}
		if h.record(sid).Status != "failed" {
			t.Fatalf("%s: record %s", name, h.record(sid).Status)
		}
	}
}

// A record whose session folder is a link, not a private directory, is never run in.
func TestFenceResumeRefusesNonPrivateFolder(t *testing.T) {
	skipOnWindows(t, "symbolic links need privileges on Windows")
	h := newHarness(t, nil)
	sess := h.api.addSession("claude")
	st, err := OpenStore(filepath.Join(h.home, ".archivist", "connect"))
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	cwd := filepath.Join(st.WorkDir(), cwdPrefix+"linked")
	if err := os.Symlink(target, cwd); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(&SessionRecord{SessionID: sess.SessionID, Agent: "claude", StartCorrelationID: "old-start",
		ClaudeSessionID: randomUUID(), Cwd: cwd, ExpiresAt: sess.ExpiresAt, Status: "active", CreatedAt: time.Now().UnixMilli(),
		Mode: string(ModeAsk)}); err != nil {
		t.Fatal(err)
	}
	h.start()
	waitFor(t, 10*time.Second, "session reattach", func() bool { return h.relay.dialCount(sess.SessionID) >= 1 })
	h.message(sess.SessionID, "echo resumed")
	h.relay.waitStatus(t, sess.SessionID, "failed", 1)
	if len(fakeRuns(t, h.home)) != 0 {
		t.Fatal("claude ran in a folder that is not a private directory")
	}
	if errs := h.errorTexts(sess.SessionID); len(errs) != 1 || !strings.Contains(errs[0], "no usable working directory") {
		t.Fatalf("errors %v", errs)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatal("the link target was written")
	}
}

// The sandbox resume (--resume-from) of a record whose folder is not this
// user's private directory starts fresh.
func TestFenceResumeSourceRefusesNonPrivateFolder(t *testing.T) {
	skipOnWindows(t, "owner and mode checks are Unix only")
	st, err := OpenStore(filepath.Join(t.TempDir(), "connect"))
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{store: st}
	save := func(cwd string) string {
		id := randomUUID()
		if err := st.Save(&SessionRecord{SessionID: id, Agent: "claude", ClaudeSessionID: randomUUID(), Cwd: cwd,
			Status: "active", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	link := filepath.Join(st.WorkDir(), cwdPrefix+"link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(st.WorkDir(), cwdPrefix+"loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(loose, 0o755)
	for name, cwd := range map[string]string{"symlink": link, "mode": loose,
		"outside": filepath.Join(t.TempDir(), "x", cwdPrefix+"y")} {
		if rec, why := d.resumeSource(save(cwd), "claude"); rec != nil || why != "it has no usable working directory" {
			t.Errorf("%s: %v %q", name, rec, why)
		}
	}
	// A missing folder (a fresh sandbox) and a private one resume.
	private := filepath.Join(st.WorkDir(), cwdPrefix+"private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{private, filepath.Join(st.WorkDir(), cwdPrefix+"gone")} {
		if rec, why := d.resumeSource(save(cwd), "claude"); rec == nil {
			t.Errorf("%s: %q", cwd, why)
		}
	}
}

// threadIsolationIs reports thread/start (or resume) params carrying the
// mode's permission profile and no sandbox, or on Windows the legacy
// sandbox mode and no profile.
func threadIsolationIs(p map[string]any, m Mode) bool {
	_, sandbox := p["sandbox"]
	_, perms := p["permissions"]
	if codexLegacySandbox {
		return p["sandbox"] == codexSandboxMode(m) && !perms
	}
	return p["permissions"] == codexProfile(m) && !sandbox
}

// turnIsolationIs is threadIsolationIs for turn/start params.
func turnIsolationIs(p map[string]any, m Mode) bool {
	_, perms := p["permissions"]
	policy, hasPolicy := p["sandboxPolicy"].(map[string]any)
	if codexLegacySandbox {
		return hasPolicy && policy["type"] == codexSandboxType(m) && !perms
	}
	_, anyPolicy := p["sandboxPolicy"]
	return p["permissions"] == codexProfile(m) && !anyPolicy
}

// Amendment 1: a projects/ name longer than 200 characters keeps every
// entry starting with its first 200 (Claude Code shortens long names).
func TestFenceClaudeProjectNames(t *testing.T) {
	if got := claudeProjectName("/home/a/.archivist/connect/work/s1"); got != "-home-a--archivist-connect-work-s1" {
		t.Fatalf("name %s", got)
	}
	dir := t.TempDir()
	long := "/" + strings.Repeat("deep/", 50) + "archivist-connect-x"
	name := claudeProjectName(long)
	for _, n := range []string{name[:200] + "-1a2b3c", "-other", name[:150]} {
		if err := os.MkdirAll(filepath.Join(dir, "projects", n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	locked, others, ownEntries := claudeConfigEntries(runtime.GOOS, dir, long)
	got := append(append([]string(nil), locked...), others...)
	if slices.Contains(got, filepath.Join(dir, "projects", name[:200]+"-1a2b3c")) ||
		!slices.Contains(ownEntries, filepath.Join(dir, "projects", name[:200]+"-1a2b3c")) {
		t.Errorf("the session's shortened projects entry is locked: %v (own %v)", got, ownEntries)
	}
	for _, want := range []string{filepath.Join(dir, "projects", "-other"), filepath.Join(dir, "projects", name[:150])} {
		if !slices.Contains(others, want) {
			t.Errorf("others lack %s: %v", want, others)
		}
	}
	if !slices.Contains(locked, filepath.Join(dir, ".credentials.json")) {
		t.Errorf("locked lacks .credentials.json: %v", locked)
	}
	if slices.Contains(got, filepath.Join(dir, "projects")) || slices.Contains(got, dir) {
		t.Errorf("projects or the folder itself is locked: %v", got)
	}
}

// Amendment 1: the backstop folds case on macOS as on Windows.
func TestFenceBackstopCaseOnMacOS(t *testing.T) {
	m := &fence{goos: "darwin", home: "/Users/u", paths: []string{"/Users/u/.ssh", "/Users/u/Library/Keychains"}}
	for _, in := range []string{"cat ~/.SSH/id_rsa", "cat /users/U/.ssh/id_rsa", "ls $home/library/keychains", "ls ${HOME}/Library/KEYCHAINS"} {
		b, _ := json.Marshal(map[string]string{"command": in})
		if !m.fenceRefusal(b) {
			t.Errorf("darwin %q not refused", in)
		}
	}
	b, _ := json.Marshal(map[string]string{"command": "cat ~/.SSHX"})
	if m.fenceRefusal(b) {
		t.Error("darwin sibling refused")
	}
	l := &fence{goos: "linux", home: "/home/u", paths: []string{"/home/u/.ssh"}}
	b, _ = json.Marshal(map[string]string{"command": "cat ~/.SSH/id_rsa"})
	if l.fenceRefusal(b) {
		t.Error("linux matched another case")
	}
}

// Amendment 1: a session folder must be exactly a directory: a named pipe
// (like a Windows junction, an irregular entry) is refused, and a link in
// its place is never followed or chmodded.
func TestFenceSessionFolderPlainDirectoryOnly(t *testing.T) {
	skipOnWindows(t, "named pipes and links are Unix fixtures here")
	st, err := OpenStore(filepath.Join(t.TempDir(), "connect"))
	if err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(st.WorkDir(), cwdPrefix+"pipe")
	if err := syscallMkfifo(pipe); err != nil {
		t.Skip("no mkfifo: ", err)
	}
	if err := st.checkSessionCwd(pipe); !errors.Is(err, errCwdUnusable) {
		t.Fatalf("named pipe: %v", err)
	}
	target := t.TempDir()
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(os.TempDir(), cwdPrefix+"link-"+randomUUID())
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(link) }()
	if err := st.ensureSessionCwd(link); !errors.Is(err, errCwdUnusable) {
		t.Fatalf("legacy link: %v", err)
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o755 {
		t.Fatalf("the link target was chmodded: %v", info.Mode())
	}
}

// Amendment 1: at startup, session folders no active record names are
// removed; an active record's folder stays.
func TestFenceStartupSweepsWorkFolders(t *testing.T) {
	h := newHarness(t, nil)
	sess := h.api.addSession("claude")
	st, err := OpenStore(filepath.Join(h.home, ".archivist", "connect"))
	if err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(st.WorkDir(), cwdPrefix+"active")
	gone := filepath.Join(st.WorkDir(), cwdPrefix+"leftover")
	other := filepath.Join(st.WorkDir(), "notes")
	for _, d := range []string{keep, gone, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Save(&SessionRecord{SessionID: sess.SessionID, Agent: "claude", StartCorrelationID: "old",
		ClaudeSessionID: randomUUID(), Cwd: keep, ExpiresAt: sess.ExpiresAt, Status: "active",
		CreatedAt: time.Now().UnixMilli(), Mode: string(ModeAsk)}); err != nil {
		t.Fatal(err)
	}
	h.start()
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Errorf("leftover folder kept: %v", err)
	}
	for _, d := range []string{keep, other} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("%s removed: %v", d, err)
		}
	}
}

// A Codex launch locks each Claude Code config folder whole (it never uses
// Claude Code's project data); a Claude Code launch keeps the split around
// its own projects entry.
func TestFenceCodexLocksClaudeConfigWhole(t *testing.T) {
	f := newFenceFixture(t)
	claudeDir := filepath.Join(f.home, "claude-b")
	own := claudeProjectName(f.cwd)
	for _, d := range []string{claudeDir, filepath.Join(f.home, ".claude")} {
		for _, sub := range []string{"projects/" + own, "projects/-other-project", "plans"} {
			if err := os.MkdirAll(filepath.Join(d, filepath.FromSlash(sub)), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	env := []string{"CLAUDE_CONFIG_DIR=" + claudeDir}
	cx, err := newFence(fenceSpec{Home: f.home, Env: env, StateDir: f.state, Cwd: f.cwd, Codex: true})
	if err != nil {
		t.Fatal(err)
	}
	cl, err := newFence(fenceSpec{Home: f.home, Env: env, StateDir: f.state, Cwd: f.cwd})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{claudeDir, filepath.Join(f.home, ".claude")} {
		if !slices.Contains(cx.paths, d) || !slices.Contains(cx.real, resolvePath(d)) {
			t.Errorf("codex fence lacks %s", d)
		}
		for _, p := range append(append([]string(nil), cx.paths...), cx.real...) {
			if strings.HasPrefix(p, d+string(filepath.Separator)) {
				t.Errorf("codex fence lists %s inside %s", p, d)
			}
		}
		if slices.Contains(cl.paths, d) || !slices.Contains(cl.ruleOnly, filepath.Join(d, "projects", "-other-project")) ||
			slices.Contains(cl.ruleOnly, filepath.Join(d, "projects", own)) {
			t.Errorf("claude fence split of %s: %v", d, cl.paths)
		}
	}
	if !slices.Contains(cx.paths, filepath.Join(f.home, ".claude.json")) || len(cx.ruleOnly) != 0 {
		t.Errorf("codex fence: ~/.claude.json or project rules %v", cx.ruleOnly)
	}
}

// Other Claude Code projects entries get only the Read and Edit rules of
// their contents (the sandbox's projects deny covers shell commands), and
// settings past the size bound fail the launch with a plain message.
func TestFenceClaudeProjectRulesAndSizeBound(t *testing.T) {
	kid := "/home/u/.claude/projects/-other"
	raw := claudeSettings("linux", []string{"/home/u/.ssh"}, []string{kid}, []string{"/home/u/.claude/projects"},
		[]string{"/home/u/.claude/projects/-own"})
	if !strings.Contains(raw, `"Read(//home/u/.claude/projects/-other/**)","Edit(//home/u/.claude/projects/-other/**)"`) ||
		strings.Contains(raw, `"Read(//home/u/.claude/projects/-other)"`) || strings.Count(raw, kid) != 2 {
		t.Fatalf("project rules: %s", raw)
	}
	if !strings.Contains(raw, `"denyRead":["/home/u/.ssh","/home/u/.claude/projects"],"allowRead":["/home/u/.claude/projects/-own"]`) {
		t.Fatalf("sandbox projects deny: %s", raw)
	}
	var kids []string
	for i := range 40 {
		kids = append(kids, "/home/u/.claude/projects/-p"+strings.Repeat("x", 40)+string(rune('a'+i%26))+strings.Repeat("y", i))
	}
	f := &fence{goos: "linux", home: "/home/u", paths: []string{"/home/u/.ssh"}, ruleOnly: kids,
		projectsDir: "/home/u/.claude/projects", projectCount: len(kids)}
	if _, err := f.claudeSettings(); err != nil {
		t.Fatal(err)
	}
	old := claudeSettingsMax
	claudeSettingsMax = 2000
	defer func() { claudeSettingsMax = old }()
	if _, err := f.claudeSettings(); err == nil ||
		err.Error() != "Claude Code's settings for this session are too large: 40 project folders in /home/u/.claude/projects" {
		t.Fatalf("size bound: %v", err)
	}
}

// A path that walks back into a locked folder with ".." is refused; one
// that walks out of it is not.
func TestFenceBackstopDotSegments(t *testing.T) {
	home := "/home/" + "u"
	own := home + "/.archivist/connect/work/archivist-connect-own"
	f := &fence{goos: "linux", home: home, paths: []string{home + "/.ssh", home + "/.archivist/credentials"}}
	for _, in := range []string{
		"cat " + own + "/../../../credentials",
		"cat ~/docs/../.ssh/id_rsa",
		"cat $HOME/./docs/../.archivist/credentials;echo",
		"python3 -c open('" + home + "/docs/x/../../.ssh/id_rsa')",
	} {
		b, _ := json.Marshal(map[string]string{"command": in})
		if !f.fenceRefusal(b) {
			t.Errorf("%q not refused", in)
		}
	}
	for _, in := range []string{"cat ~/.ssh/../docs/a.txt", "cat " + own + "/notes/../a.md", "ls ../x"} {
		b, _ := json.Marshal(map[string]string{"command": in})
		if f.fenceRefusal(b) {
			t.Errorf("%q refused", in)
		}
	}
	w := &fence{goos: "windows", home: `C:\Users\U`, paths: []string{`C:\Users\U\.ssh`}}
	b, _ := json.Marshal(map[string]string{"command": `type C:\Users\U\Documents\..\.SSH\id_rsa`})
	if !w.fenceRefusal(b) {
		t.Error("windows dot segments not refused")
	}
}

// The backstop reads home forms as the home folder and relative paths
// against the session folder before resolving "." and "..": a path that
// climbs above the home form, or one relative to the session folder, into
// a locked folder is refused; a relative path to an open one is not.
func TestFenceBackstopHomeAndRelativeForms(t *testing.T) {
	user := "u" + "x"
	home := "/home/" + user
	own := home + "/.archivist/connect/work/" + cwdPrefix + "own"
	f := &fence{goos: "linux", home: home, cwd: own,
		paths: []string{home + "/.ssh", home + "/.archivist/credentials", home + "/.archivist/connect/work/" + cwdPrefix + "other"}}
	refuse := func(in string) bool {
		b, _ := json.Marshal(map[string]string{"command": in})
		return f.fenceRefusal(b)
	}
	for _, in := range []string{
		"cat $HOME/../" + user + "/.ssh/id_rsa",
		"cat ~/../" + user + "/.archivist/credentials",
		"cat ${HOME}/../../home/" + user + "/.ssh/config",
		"cat ../../../credentials",
		"cat ./../../../credentials",
		"ls ../" + cwdPrefix + "other/notes.md",
		"tail -n 5 docs/../../../../../.ssh/id_rsa",
	} {
		if !refuse(in) {
			t.Errorf("%q not refused", in)
		}
	}
	for _, in := range []string{"cat notes/a.md", "cat ./report.md", "ls ../", "cat ../../../../docs/a.txt", "cat $HOME/docs/a.txt"} {
		if refuse(in) {
			t.Errorf("%q refused", in)
		}
	}
	w := &fence{goos: "windows", home: `C:\Users\` + user, cwd: `C:\Users\` + user + `\.archivist\connect\work\` + cwdPrefix + "own",
		paths: []string{`C:\Users\` + user + `\.archivist\credentials`}}
	for _, in := range []string{`type %USERPROFILE%\..\` + user + `\.archivist\credentials`, `type ..\..\..\CREDENTIALS`,
		`Get-Content $env:USERPROFILE/../` + user + `/.archivist/credentials`} {
		b, _ := json.Marshal(map[string]string{"command": in})
		if !w.fenceRefusal(b) {
			t.Errorf("windows %q not refused", in)
		}
	}
}

// Session folders an earlier release left in the OS temp directory are
// locked, except the session's own.
func TestFenceLocksLegacyTempFolders(t *testing.T) {
	f := newFenceFixture(t)
	other, err := os.MkdirTemp(os.TempDir(), cwdPrefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(other) }()
	ownLegacy, err := os.MkdirTemp(os.TempDir(), cwdPrefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(ownLegacy) }()
	fe, err := newFence(fenceSpec{Home: f.home, StateDir: f.state, Cwd: f.cwd})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{other, ownLegacy} {
		if !slices.Contains(fe.paths, p) || !slices.Contains(fe.real, resolvePath(p)) {
			t.Errorf("legacy folder %s not locked", p)
		}
	}
	fe2, err := newFence(fenceSpec{Home: f.home, StateDir: f.state, Cwd: ownLegacy})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(fe2.paths, other) || slices.Contains(fe2.paths, ownLegacy) || slices.Contains(fe2.real, resolvePath(ownLegacy)) {
		t.Errorf("legacy session: own %s or other %s wrong in %v", ownLegacy, other, fe2.paths)
	}
}

// A record from an earlier release whose folder is a private 0700 folder
// in the OS temp directory still resumes.
func TestFenceLegacyTempFolderResumes(t *testing.T) {
	skipOnWindows(t, "owner and mode checks are Unix only")
	st, err := OpenStore(filepath.Join(t.TempDir(), "connect"))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(os.TempDir(), cwdPrefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	cwd, err := filepath.EvalSymlinks(dir) // as makeCwd records it
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ensureSessionCwd(cwd); err != nil {
		t.Fatalf("ensureSessionCwd: %v", err)
	}
	id := randomUUID()
	if err := st.Save(&SessionRecord{SessionID: id, Agent: "claude", ClaudeSessionID: randomUUID(), Cwd: cwd,
		Status: "active", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if rec, why := (&Daemon{store: st}).resumeSource(id, "claude"); rec == nil || rec.Cwd != cwd {
		t.Fatalf("legacy record not resumable: %q", why)
	}
}

// Codex never starts without the launch's fence.
func TestFenceCodexNeedsFence(t *testing.T) {
	s := &session{d: &Daemon{codex: CodexConfig{Bin: "/nonexistent/codex"}}}
	if err := s.spawnCodex(context.Background(), "", ""); err == nil || !strings.Contains(err.Error(), "locked paths") {
		t.Fatalf("spawn without a fence: %v", err)
	}
}
