package connect

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// The fence (Story 94.5): every harness launch (spawn and resume) computes
// one list of locked paths from the session's folder, and that list drives
// three layers, each the harness's own:
//
//   - Claude Code: its sandbox (sandbox.filesystem.denyRead and
//     sandbox.credentials deny) and Read and Edit deny rules, all through
//     --settings (the setting sources stay empty, so --settings is the only
//     layer that carries them);
//   - Codex: three permission profiles defined with -c, whose filesystem
//     entries deny each locked path, selected per thread and turn;
//   - the daemon: an approval request whose string arguments name a locked
//     path is refused before any card or mode decision (fenceRefusal).
//
// A session can still read its own folder, the rest of the computer outside
// the list, and run the Mosaic tools; the MCP server and the harnesses
// themselves run outside the sandbox and keep reading their own files.

// fenceHomeNames are the home-relative locked entries (slash separated) of
// each OS: credential files, the harness logins and the browser profile
// and password manager folders. The Claude Code config folder (~/.claude)
// is not among them: claudeConfigEntries locks it entry by entry.
func fenceHomeNames(goos string) []string {
	names := []string{
		".ssh", ".aws", ".config/gcloud", ".azure", ".kube", ".docker/config.json", ".gnupg",
		".netrc", ".npmrc", ".pypirc", ".claude.json", ".codex",
	}
	switch goos {
	case "darwin":
		for _, app := range []string{"Google/Chrome", "Chromium", "BraveSoftware", "Microsoft Edge", "Arc",
			"Firefox", "1Password", "Bitwarden"} {
			names = append(names, "Library/Application Support/"+app)
		}
		names = append(names, "Library/Keychains", "Library/Cookies", "Library/Safari",
			"Library/Group Containers/2BUA8C4S2C.com.1password")
	case "windows":
		names = append(names, "AppData/Local/Google/Chrome/User Data", "AppData/Local/Microsoft/Edge/User Data",
			"AppData/Local/BraveSoftware", "AppData/Roaming/Mozilla/Firefox", "AppData/Local/1Password",
			"AppData/Roaming/Bitwarden", "AppData/Roaming/gcloud")
	default:
		names = append(names, ".mozilla", ".config/google-chrome", ".config/chromium", ".config/BraveSoftware",
			".config/microsoft-edge", ".config/1Password", ".config/Bitwarden", ".config/keepassxc",
			".password-store", ".local/share/keyrings")
	}
	return names
}

// Entries locked even when absent: inside ~/.archivist (besides every
// entry but connect) and inside the state directory (besides every entry
// but work).
var (
	fenceArchivistNames = []string{"credentials", "install-channel"}
	fenceStateNames     = []string{"sessions", "run", codexHomeDir, "connect.log", "service.pid", "service.env"}
)

// fenceSpec is the input of one fence computation.
type fenceSpec struct {
	Home string
	// Env is the daemon environment (XDG_CONFIG_HOME, CLAUDE_CONFIG_DIR,
	// CODEX_HOME).
	Env []string
	// StateDir is the connect state directory (~/.archivist/connect).
	StateDir string
	// Cwd is this session's folder: never locked, and the one work entry
	// left out.
	Cwd string
	// CodexHome is the owner's Codex home ("" = OwnerCodexHome of Env).
	CodexHome string
	// Keep are the harness binaries the launch runs. Codex runs its own
	// binary again inside its sandbox (the standalone install keeps it in
	// ~/.codex/packages), so an entry holding one is locked entry by entry,
	// leaving out the entry that holds the binary.
	Keep []string
	// Codex marks a Codex launch: it never uses Claude Code's project data,
	// so each Claude Code config folder is locked whole (one entry) rather
	// than entry by entry.
	Codex bool
}

// fence is one launch's locked path list.
type fence struct {
	goos string
	home string
	// paths lists every entry as written, each followed by its symlink
	// resolved form when that differs (Claude Code, the backstop).
	paths []string
	// real lists the resolved forms only (Codex refuses a symlinked deny
	// path).
	real []string
	// work and own are the folders Claude Code's shell sandbox also denies
	// and the ones it reopens inside them (as written and resolved): the
	// work folder and this session's folder, and each Claude Code config
	// folder's projects and this session's projects entry, so folders
	// created after the launch stay closed to shell commands too. Never a
	// credentials entry or a deny rule (a rule cannot reopen a child), and
	// not for Codex (which cannot either).
	work, own []string
	// ruleOnly are the projects entries of other working directories (as
	// written and resolved): Read and Edit deny rules only, since the
	// sandbox's projects deny covers shell commands (Claude launches).
	ruleOnly []string
	// cwd is this session's folder: the backstop reads relative paths
	// against it.
	cwd string
	// projectsDir and projectCount name the projects folder with the most
	// entries, for the settings size error.
	projectsDir  string
	projectCount int
}

// newFence computes the locked list for a launch on this OS.
func newFence(spec fenceSpec) (*fence, error) {
	goos := runtime.GOOS
	if spec.Home == "" || !filepath.IsAbs(spec.Home) {
		return nil, errors.New("the home directory is unknown")
	}
	if spec.Cwd == "" || !filepath.IsAbs(spec.Cwd) {
		return nil, errors.New("the session has no working directory")
	}
	home := filepath.Clean(spec.Home)
	var written []string
	add := func(p string) {
		if p != "" && filepath.IsAbs(p) {
			written = append(written, filepath.Clean(p))
		}
	}
	for _, rel := range fenceHomeNames(goos) {
		add(filepath.Join(home, filepath.FromSlash(rel)))
	}
	if xdg := envLookup(spec.Env, "XDG_CONFIG_HOME", goos); filepath.IsAbs(xdg) {
		// The .config names also under $XDG_CONFIG_HOME (gcloud, and on
		// Linux the browsers and password managers).
		for _, rel := range fenceHomeNames(goos) {
			if rest, ok := strings.CutPrefix(rel, ".config/"); ok {
				add(filepath.Join(xdg, filepath.FromSlash(rest)))
			}
		}
	}
	// The Claude Code config folders: every entry but this session's own
	// data folder (projects/<escaped cwd>), where Claude Code keeps large
	// tool results it reads back.
	claudeDirs := []string{filepath.Join(home, ".claude")}
	if dir := envLookup(spec.Env, ClaudeConfigDirKey, goos); filepath.IsAbs(dir) {
		claudeDirs = append(claudeDirs, dir)
	}
	var kids, projectsDirs, projectsOwn []string
	var mostDir string
	most := -1
	for _, dir := range claudeDirs {
		if spec.Codex {
			add(dir)
			continue
		}
		locked, others, own := claudeConfigEntries(goos, filepath.Clean(dir), spec.Cwd)
		for _, p := range locked {
			add(p)
		}
		kids = append(kids, others...)
		projects := filepath.Join(filepath.Clean(dir), "projects")
		projectsDirs = append(projectsDirs, projects)
		projectsOwn = append(projectsOwn, own...)
		if len(others) > most {
			most, mostDir = len(others), projects
		}
	}
	codexHome := spec.CodexHome
	if codexHome == "" {
		codexHome = OwnerCodexHome(spec.Env, home)
	}
	add(codexHome)

	// ~/.archivist: every entry but the state directory, plus the fixed
	// names; the state directory: every entry but work, plus its fixed
	// names; work: every session folder but this one.
	state := filepath.Clean(spec.StateDir)
	archivist := filepath.Join(home, ".archivist")
	for _, name := range entryNames(archivist, fenceArchivistNames) {
		if p := filepath.Join(archivist, name); !fencePathEqual(goos, p, state) {
			add(p)
		}
	}
	if spec.StateDir != "" && filepath.IsAbs(state) {
		for _, name := range entryNames(state, fenceStateNames) {
			if !fenceNameEqual(goos, name, workDirName) {
				add(filepath.Join(state, name))
			}
		}
		work := filepath.Join(state, workDirName)
		for _, name := range entryNames(work, nil) {
			if p := filepath.Join(work, name); !fencePathEqual(goos, p, spec.Cwd) {
				add(p)
			}
		}
	}

	// Session folders of earlier releases in the OS temp directory: every
	// one but this session's own.
	tmp := os.TempDir()
	for _, name := range entryNames(tmp, nil) {
		if p := filepath.Join(tmp, name); strings.HasPrefix(name, cwdPrefix) && !fencePathEqual(goos, p, spec.Cwd) {
			add(p)
		}
	}

	written = splitAroundKeep(goos, written, spec.Keep)
	cwd := filepath.Clean(spec.Cwd)
	realCwd := resolvePath(cwd)
	f := &fence{goos: goos, home: home, cwd: cwd}
	for _, p := range written {
		r := resolvePath(p)
		// Never an entry that holds the session folder: it would hide the
		// folder from the session.
		if fenceWithin(goos, cwd, p) || fenceWithin(goos, realCwd, r) || fenceWithin(goos, realCwd, p) {
			continue
		}
		f.paths = appendNew(goos, f.paths, p)
		f.paths = appendNew(goos, f.paths, r)
		f.real = appendNew(goos, f.real, r)
	}
	if spec.StateDir != "" && filepath.IsAbs(state) {
		work := filepath.Join(state, workDirName)
		f.work = appendNew(goos, appendNew(goos, nil, work), resolvePath(work))
		f.own = appendNew(goos, appendNew(goos, nil, cwd), realCwd)
	}
	for _, p := range projectsDirs {
		f.work = appendNew(goos, appendNew(goos, f.work, p), resolvePath(p))
	}
	for _, p := range projectsOwn {
		f.own = appendNew(goos, appendNew(goos, f.own, p), resolvePath(p))
	}
	for _, p := range kids {
		f.ruleOnly = appendNew(goos, appendNew(goos, f.ruleOnly, p), resolvePath(p))
	}
	f.projectsDir, f.projectCount = mostDir, most
	return f, nil
}

// claudeProjectName is Claude Code's projects/ folder name for a working
// directory: every character outside [A-Za-z0-9] replaced by '-'.
func claudeProjectName(cwd string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, cwd)
}

// claudeProjectKept reports a projects/ entry name of the session's own
// data: the cwd's name as written or resolved, and for a name longer than
// 200 characters any entry starting with its first 200.
func claudeProjectKept(goos, name string, cwds []string) bool {
	for _, c := range cwds {
		want := claudeProjectName(c)
		if fenceNameEqual(goos, name, want) {
			return true
		}
		if len(want) > 200 && len(name) >= 200 && fenceNameEqual(goos, name[:200], want[:200]) {
			return true
		}
	}
	return false
}

// claudeConfigEntries locks a Claude Code config folder entry by entry:
// locked is every entry but projects, plus .credentials.json even when
// absent; others is every projects entry but the session's own; own is the
// session's own projects entries (the exact name, and any present entry
// the 200 character rule keeps).
func claudeConfigEntries(goos, dir, cwd string) (locked, others, own []string) {
	cwds := []string{filepath.Clean(cwd), resolvePath(cwd)}
	for _, name := range entryNames(dir, []string{".credentials.json"}) {
		if !fenceNameEqual(goos, name, "projects") {
			locked = append(locked, filepath.Join(dir, name))
		}
	}
	projects := filepath.Join(dir, "projects")
	for _, c := range cwds {
		own = appendNew(goos, own, filepath.Join(projects, claudeProjectName(c)))
	}
	for _, name := range entryNames(projects, nil) {
		if claudeProjectKept(goos, name, cwds) {
			own = appendNew(goos, own, filepath.Join(projects, name))
		} else {
			others = append(others, filepath.Join(projects, name))
		}
	}
	return locked, others, own
}

// splitAroundKeep replaces each entry that holds a kept binary (as
// written or resolved) by its own entries, leaving out the ones that hold
// it: the binary's folder stays readable, its siblings stay locked.
func splitAroundKeep(goos string, written, keep []string) []string {
	var keeps []string
	for _, k := range keep {
		if k != "" && filepath.IsAbs(k) {
			keeps = append(keeps, filepath.Clean(k), resolvePath(k))
		}
	}
	holds := func(p string) bool {
		for _, k := range keeps {
			if fenceWithin(goos, k, p) || fenceWithin(goos, k, resolvePath(p)) {
				return true
			}
		}
		return false
	}
	var out []string
	for _, p := range written {
		if !holds(p) {
			out = append(out, p)
			continue
		}
		for _, name := range entryNames(p, nil) {
			if child := filepath.Join(p, name); !holds(child) {
				out = append(out, child)
			}
		}
	}
	return out
}

// entryNames lists dir's entries plus the fixed names (each once, sorted).
func entryNames(dir string, fixed []string) []string {
	names := append([]string(nil), fixed...)
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// appendNew appends p unless list already holds it.
func appendNew(goos string, list []string, p string) []string {
	for _, q := range list {
		if fenceNameEqual(goos, q, p) {
			return list
		}
	}
	return append(list, p)
}

// fenceNameEqual compares names and paths, ignoring case on Windows.
func fenceNameEqual(goos, a, b string) bool {
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// fencePathEqual reports the same path, lexically or once resolved.
func fencePathEqual(goos, a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return fenceNameEqual(goos, a, b) || fenceNameEqual(goos, resolvePath(a), resolvePath(b))
}

// fenceWithin reports path equal to dir or inside it (lexically).
func fenceWithin(goos, path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if goos == "windows" {
		rel, err = filepath.Rel(strings.ToLower(dir), strings.ToLower(path))
	}
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolvePath resolves symbolic links in p through its nearest existing
// ancestor (p itself when it exists); a dangling link is followed by its
// target text. Unresolvable parts are kept as written.
func resolvePath(p string) string {
	return resolveDepth(filepath.Clean(p), 0)
}

func resolveDepth(p string, depth int) string {
	if depth > 16 {
		return p
	}
	existing, rest := p, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return p
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return p
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		// A dangling link: follow its target text.
		target, lerr := os.Readlink(existing)
		if lerr != nil {
			return p
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(existing), target)
		}
		return resolveDepth(filepath.Join(target, rest), depth+1)
	}
	return filepath.Join(resolved, rest)
}

// envLookup returns key's value in env (case-insensitive key on Windows).
func envLookup(env []string, key, goos string) string {
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && fenceNameEqual(goos, k, key) {
			return v
		}
	}
	return ""
}

// ─── Claude Code ────────────────────────────────────────────────────────────

type claudeCredentialFile struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

type claudeSandboxSettings struct {
	Enabled                  bool `json:"enabled"`
	FailIfUnavailable        bool `json:"failIfUnavailable"`
	AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
	AutoAllowBashIfSandboxed bool `json:"autoAllowBashIfSandboxed"`
	Filesystem               struct {
		DenyRead  []string `json:"denyRead"`
		AllowRead []string `json:"allowRead,omitempty"`
	} `json:"filesystem"`
	Credentials struct {
		Files []claudeCredentialFile `json:"files"`
	} `json:"credentials"`
	Network struct {
		AllowedDomains []string `json:"allowedDomains"`
	} `json:"network"`
}

type claudePermissionSettings struct {
	Deny []string `json:"deny"`
	Ask  []string `json:"ask,omitempty"`
}

type claudeFlagSettingsJSON struct {
	// UseAutoModeDuringPlan stays false (Story 78.32): plan mode never lets
	// the auto mode classifier approve shell commands.
	UseAutoModeDuringPlan bool `json:"useAutoModeDuringPlan"`
	// PlansDirectory keeps plan files (plan mode) in the session folder.
	PlansDirectory string                   `json:"plansDirectory"`
	Sandbox        *claudeSandboxSettings   `json:"sandbox,omitempty"`
	Permissions    claudePermissionSettings `json:"permissions"`
}

// claudePlansDirectory is the plan file folder, relative to the session
// folder.
const claudePlansDirectory = ".plans"

// claudeSettings is the --settings JSON of a launch on goos. Linux and
// macOS: the sandbox for shell commands (required, no unsandboxed retry,
// Bash still asks per mode, network as before) with the list denied to
// reads and as credentials, plus Read and Edit deny rules; the sandbox
// also denies reading work and reopens own (fence.work, fence.own).
// ruleOnly paths get only the Read and Edit deny rules of their contents.
// Native Windows has no shell sandbox: the deny rules, and Bash and
// PowerShell always ask. Plan files go to the session folder on every OS.
func claudeSettings(goos string, paths, ruleOnly, work, own []string) string {
	s := claudeFlagSettingsJSON{PlansDirectory: claudePlansDirectory, Permissions: claudePermissionSettings{Deny: []string{}}}
	for _, p := range paths {
		r := claudeRulePath(goos, p)
		s.Permissions.Deny = append(s.Permissions.Deny, "Read("+r+")", "Read("+r+"/**)", "Edit("+r+")", "Edit("+r+"/**)")
	}
	for _, p := range ruleOnly {
		r := claudeRulePath(goos, p)
		s.Permissions.Deny = append(s.Permissions.Deny, "Read("+r+"/**)", "Edit("+r+"/**)")
	}
	if goos == "windows" {
		s.Permissions.Ask = []string{"Bash", "PowerShell"}
	} else {
		sb := &claudeSandboxSettings{Enabled: true, FailIfUnavailable: true}
		sb.Filesystem.DenyRead = append(append([]string{}, paths...), work...)
		if len(work) > 0 {
			sb.Filesystem.AllowRead = append([]string{}, own...)
		}
		sb.Credentials.Files = []claudeCredentialFile{}
		for _, p := range paths {
			sb.Credentials.Files = append(sb.Credentials.Files, claudeCredentialFile{Path: p, Mode: "deny"})
		}
		sb.Network.AllowedDomains = []string{"*"}
		s.Sandbox = sb
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// claudeRulePath is an absolute path in permission rule form: "//" then
// the path; on Windows the POSIX form Claude Code matches (C:\Users\a is
// //c/Users/a).
func claudeRulePath(goos, p string) string {
	if goos == "windows" {
		p = strings.ReplaceAll(p, `\`, "/")
		if len(p) >= 2 && p[1] == ':' {
			p = "/" + strings.ToLower(p[:1]) + p[2:]
		}
	}
	return "/" + p
}

// claudeSettingsMax bounds the settings file (Claude Code refuses one over
// 2 MiB).
var claudeSettingsMax = 3 << 19 // 1.5 MiB

// claudeSettings is this launch's --settings JSON; one over
// claudeSettingsMax fails the launch and the session (a lostError).
func (f *fence) claudeSettings() (string, error) {
	out := claudeSettings(f.goos, f.paths, f.ruleOnly, f.work, f.own)
	if len(out) > claudeSettingsMax {
		// The session cannot start this way: lostError ends it.
		return "", &lostError{fmt.Sprintf("Claude Code's settings for this session are too large: %d project folders in %s",
			f.projectCount, f.projectsDir)}
	}
	return out, nil
}

// claudeSettingsFileName is the launch's --settings file in the session's
// private run directory.
const claudeSettingsFileName = "claude-settings.json"

// writeClaudeSettings writes this launch's settings (0600) and returns the
// path passed to --settings: the list grows with the Claude Code projects
// folder, past what one command line argument may hold (Linux: 128 KiB;
// Claude Code reads a settings file up to 2 MiB).
func (s *session) writeClaudeSettings() (string, error) {
	settings, err := s.fence.claudeSettings()
	if err != nil {
		return "", err
	}
	dir, err := s.d.store.RunDir(s.id)
	if err != nil {
		return "", fmt.Errorf("claude settings: %w", err)
	}
	path := filepath.Join(dir, claudeSettingsFileName)
	if err := writeFileAtomic(path, []byte(settings)); err != nil {
		return "", fmt.Errorf("claude settings: %w", err)
	}
	return path, nil
}

// initExtensionsProblem checks the init frame's MCP servers and plugins
// (Story 94.5): archivist is the only MCP server, and every plugin is one
// of the harness's built-ins (path "builtin", source "<name>@builtin").
func initExtensionsProblem(mcpServers, plugins json.RawMessage) string {
	var problems []string
	var servers []struct {
		Name string `json:"name"`
	}
	if len(mcpServers) > 0 && string(mcpServers) != "null" {
		if json.Unmarshal(mcpServers, &servers) != nil {
			problems = append(problems, "its MCP server list is unreadable")
		}
	}
	for _, s := range servers {
		if s.Name != "archivist" {
			problems = append(problems, fmt.Sprintf("it reports the MCP server %q", truncateString(s.Name, 64)))
		}
	}
	var list []struct {
		Name   string `json:"name"`
		Path   string `json:"path"`
		Source string `json:"source"`
	}
	if len(plugins) > 0 && string(plugins) != "null" {
		if json.Unmarshal(plugins, &list) != nil {
			problems = append(problems, "its plugin list is unreadable")
		}
	}
	for _, p := range list {
		if p.Path != "builtin" || !strings.HasSuffix(p.Source, "@builtin") {
			problems = append(problems, fmt.Sprintf("it reports the plugin %q", truncateString(p.Name, 64)))
		}
	}
	return strings.Join(problems, "; ")
}

// ─── Codex ──────────────────────────────────────────────────────────────────

// Codex permission profiles (macOS and Linux): read only for read_only,
// the workspace (the session folder, /tmp read only) without network for
// ask and auto_edits, and the same workspace with network for full_auto.
// Each denies every locked path.
const (
	codexProfileRead  = "archivist-read"
	codexProfileWrite = "archivist-write"
	codexProfileFull  = "archivist-full"
)

// codexProfile is the permission profile id of a mode.
func codexProfile(m Mode) string {
	switch m {
	case ModeAsk, ModeAutoEdits:
		return codexProfileWrite
	case ModeFullAuto:
		return codexProfileFull
	}
	return codexProfileRead
}

// codexProfileExtends is the built-in profile each one extends.
func codexProfileExtends(id string) string {
	if id == codexProfileRead {
		return ":read-only"
	}
	return ":workspace"
}

// codexProfileConfig is the -c pairs defining the profiles over real (the
// resolved locked paths), with archivist-read the default.
func codexProfileConfig(real []string) [][2]string {
	deny := func(extra ...string) string {
		parts := make([]string, 0, len(real)+len(extra))
		for _, p := range real {
			parts = append(parts, tomlValue(p)+`="deny"`)
		}
		return "{" + strings.Join(append(parts, extra...), ", ") + "}"
	}
	pairs := [][2]string{{"default_permissions", tomlValue(codexProfileRead)}}
	for _, p := range []struct {
		id      string
		fs      string
		network bool
	}{
		{codexProfileRead, deny(), false},
		{codexProfileWrite, deny(`":slash_tmp"="read"`), false},
		{codexProfileFull, deny(`":slash_tmp"="read"`), true},
	} {
		pairs = append(pairs,
			[2]string{"permissions." + p.id + ".extends", tomlValue(codexProfileExtends(p.id))},
			[2]string{"permissions." + p.id + ".filesystem", p.fs},
			[2]string{"permissions." + p.id + ".network.enabled", fmt.Sprint(p.network)})
	}
	return pairs
}

// ─── daemon backstop ────────────────────────────────────────────────────────

// fenceDenied is the message of a refused approval.
const fenceDenied = "Denied: archivist connect sessions do not open this location."

// fenceRefusal reports an approval input (a JSON value, or plain text)
// whose string arguments name a locked path: absolute, as ~/rel, $HOME/rel
// or ${HOME}/rel, relative to the session folder, through "." and ".."
// segments, and on Windows also %USERPROFILE%\rel (either slash). On
// Windows and macOS, whose file systems ignore case by default, any case
// matches. A longer sibling name (~/.sshx) is not a match.
func (f *fence) fenceRefusal(input json.RawMessage) bool {
	if f == nil || len(input) == 0 {
		return false
	}
	needles := f.needles()
	var v any
	if json.Unmarshal(input, &v) != nil {
		return f.fenceMatch(string(input), needles)
	}
	found := false
	walkStrings(v, func(s string) {
		found = found || f.fenceMatch(s, needles)
	})
	return found
}

// walkStrings calls fn for every string in a decoded JSON value.
func walkStrings(v any, fn func(string)) {
	switch x := v.(type) {
	case string:
		fn(x)
	case []any:
		for _, e := range x {
			walkStrings(e, fn)
		}
	case map[string]any:
		for _, e := range x {
			walkStrings(e, fn)
		}
	}
}

// needles are the normalized forms of every locked path.
func (f *fence) needles() []string {
	var out []string
	for _, p := range append(append([]string(nil), f.paths...), f.ruleOnly...) {
		out = append(out, fenceNormalize(f.goos, p))
	}
	return out
}

// homeForms are the spellings of the home folder the backstop expands.
func homeForms(goos string) []string {
	forms := []string{"~", "$HOME", "${HOME}"}
	if goos == "windows" {
		forms = append(forms, "%USERPROFILE%", "$env:USERPROFILE")
	}
	return forms
}

// fenceNormalize prepares text for matching: quotes (and shell escapes)
// dropped, repeated slashes and "/./" collapsed; on Windows backslashes as
// slashes; on Windows and macOS any case (case folded for matching only).
func fenceNormalize(goos, s string) string {
	if goos == "windows" {
		s = strings.ReplaceAll(s, `\`, "/")
		s = strings.NewReplacer(`"`, "", `'`, "", "`", "").Replace(s)
	} else {
		s = strings.NewReplacer(`"`, "", `'`, "", `\`, "").Replace(s)
	}
	if goos == "windows" || goos == "darwin" {
		s = strings.ToLower(s)
	}
	for {
		next := strings.ReplaceAll(strings.ReplaceAll(s, "//", "/"), "/./", "/")
		if next == s {
			return s
		}
		s = next
	}
}

// fenceMatch reports a needle in text that ends at a name boundary, after
// each path-like token is lexically cleaned ("." and ".." segments
// resolved).
func (f *fence) fenceMatch(text string, needles []string) bool {
	return fenceMatchNormalized(f.cleanTokens(fenceNormalize(f.goos, text)), needles)
}

// cleanTokens makes every path-like token absolute and clean: a leading
// home form (~, $HOME, ${HOME}, on Windows %USERPROFILE% and
// $env:USERPROFILE) becomes the home folder, a relative path (one with a
// "/" or starting with ".") is read against the session folder, and "."
// and ".." segments are resolved lexically (path.Clean). Tokens end at
// whitespace and shell delimiters (not braces: ${HOME}); text is
// normalized.
func (f *fence) cleanTokens(text string) string {
	home := strings.TrimSuffix(fenceNormalize(f.goos, f.home), "/")
	cwd := strings.TrimSuffix(fenceNormalize(f.goos, f.cwd), "/")
	var forms []string
	for _, h := range homeForms(f.goos) {
		forms = append(forms, fenceNormalize(f.goos, h))
	}
	abs := func(tok string) bool {
		return strings.HasPrefix(tok, "/") || len(tok) >= 3 && tok[1] == ':' && tok[2] == '/'
	}
	var b strings.Builder
	start := 0
	flush := func(end int) {
		tok := text[start:end]
		expanded := false
		if f.home != "" {
			for _, h := range forms {
				if rest, ok := strings.CutPrefix(tok, h); ok && (rest == "" || rest[0] == '/') {
					tok, expanded = home+rest, true
					break
				}
			}
		}
		if !expanded && cwd != "" && tok != "" && !abs(tok) && (strings.Contains(tok, "/") || tok[0] == '.') {
			tok = cwd + "/" + tok
		}
		if strings.Contains(tok, "/") {
			tok = path.Clean(tok)
		}
		b.WriteString(tok)
	}
	for i := 0; i < len(text); i++ {
		if strings.IndexByte(" \t\n\r;|&<>()=,[]", text[i]) >= 0 {
			flush(i)
			b.WriteByte(text[i])
			start = i + 1
		}
	}
	flush(len(text))
	return b.String()
}

// fenceMatchNormalized is fenceMatch on normalized text.
func fenceMatchNormalized(text string, needles []string) bool {
	for _, n := range needles {
		for i := 0; ; {
			j := strings.Index(text[i:], n)
			if j < 0 {
				break
			}
			end := i + j + len(n)
			if end == len(text) || !fenceNameByte(text[end]) {
				return true
			}
			i += j + 1
		}
	}
	return false
}

// fenceNameByte reports a byte that continues a file name.
func fenceNameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
		b == '.' || b == '_' || b == '-' || b >= 0x80
}

// ─── session wiring ─────────────────────────────────────────────────────────

// buildFence computes the locked list for this session's next launch.
func (s *session) buildFence() (*fence, error) {
	env := s.d.environ()
	home := envLookup(env, "HOME", runtime.GOOS)
	if runtime.GOOS == "windows" {
		home = envLookup(env, "USERPROFILE", runtime.GOOS)
	}
	if home == "" || !filepath.IsAbs(home) {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return nil, err
		}
	}
	bin := s.d.claude.Bin
	if s.isCodex() {
		bin = s.d.codex.Bin
	}
	return newFence(fenceSpec{Home: home, Env: env, StateDir: s.d.store.Dir(), Cwd: s.rec.Cwd,
		CodexHome: s.d.codex.OwnerHome, Keep: []string{bin}, Codex: s.isCodex()})
}

// currentFence is the fence of the running launch, computed when none is.
func (s *session) currentFence() *fence {
	if s.fence == nil {
		f, err := s.buildFence()
		if err != nil {
			s.log.Printf("session fence: %v", err)
			return nil
		}
		s.fence = f
	}
	return s.fence
}

// claudeFenceDeny refuses a can_use_tool request that names a locked path
// (no card, whatever the mode); true when answered. Without a fence every
// request is refused.
func (s *session) claudeFenceDeny(requestID string, req controlRequest) bool {
	f := s.currentFence()
	if f != nil && !f.fenceRefusal(req.Input) {
		return false
	}
	s.log.Printf("%s request %s denied: it names a location sessions do not open", truncateString(req.ToolName, 64), requestID)
	if err := s.proc.WriteJSON(denyFrame(requestID, fenceDenied)); err != nil {
		s.log.Printf("write to claude failed: %v", err)
	}
	return true
}

// codexFenceDecline declines a command or file change approval that names
// a locked path (the request params, and for a file change the change
// list); true when answered.
func (s *session) codexFenceDecline(reply codexReplyFunc, method string, params json.RawMessage) bool {
	f := s.currentFence()
	refused := f == nil || f.fenceRefusal(params)
	if !refused && method == "item/fileChange/requestApproval" {
		var p struct {
			ItemID string `json:"itemId"`
		}
		if json.Unmarshal(params, &p) == nil {
			refused = f.fenceRefusal(s.ctr.files[p.ItemID])
		}
	}
	if !refused {
		return false
	}
	s.log.Printf("codex %s declined: it names a location sessions do not open", method)
	reply(map[string]any{"decision": "decline"})
	return true
}
