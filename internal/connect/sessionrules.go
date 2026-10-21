package connect

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Claude "for the session" answers (Story 94.4). An "Allow for session" on
// a Claude approval card is kept as one narrow rule in Claude Code's own
// rule style, derived here from the approved call (never from the
// harness's permission_suggestions):
//
//   - Bash: the command prefix (the program, plus its subcommand word) for a
//     plain command; the exact command for anything else (shell operators,
//     substitutions, redirections, environment assignments, wrappers,
//     interpreters and runners).
//   - Edit and Write: the absolute file path, shared by both tools (a
//     path with a ".." element is kept as that exact call; letter case is
//     compared as written).
//   - Read, Grep, Glob and any other tool: every call of that tool.
//
// A call whose input cannot be read that way is remembered as that exact
// call. "Deny for session" keeps its tool wide meaning and wins over every
// allow rule, as deny rules come before allow rules in Claude Code.

// Session scope kinds, as the approval card's archivist_session_scope
// states them.
const (
	scopePrefix  = "prefix"
	scopeCommand = "command"
	scopePath    = "path"
	scopeCall    = "call"
	scopeTool    = "tool"
)

// sessionRules are one Claude session's remembered answers.
type sessionRules struct {
	deny     map[string]bool // tool name: rejected for the session
	tools    map[string]bool // tool name: every call allowed
	prefixes map[string]bool // Bash prefix words, joined by one space
	commands map[string]bool // exact Bash commands (trimmed)
	paths    map[string]bool // Edit and Write file paths (cleaned)
	calls    map[string]bool // callKey: one exact tool call
}

func newSessionRules() *sessionRules {
	return &sessionRules{deny: map[string]bool{}, tools: map[string]bool{}, prefixes: map[string]bool{},
		commands: map[string]bool{}, paths: map[string]bool{}, calls: map[string]bool{}}
}

// decide answers a call from the remembered rules: ok is false when no rule
// covers it (the call raises a card).
func (r *sessionRules) decide(tool string, input json.RawMessage) (allow, ok bool) {
	if r.deny[tool] {
		return false, true
	}
	in := decodeInput(input)
	kind, value := scopeOf(tool, in)
	switch kind {
	case scopeTool:
		return r.tools[tool], r.tools[tool]
	case scopePath:
		return r.paths[value], r.paths[value]
	case scopeCall:
		ok := r.calls[callKey(tool, in)]
		return ok, ok
	}
	// Bash with a command string: an exact rule, or a prefix rule.
	cmd := strings.TrimSpace(bashCommand(in))
	if r.commands[cmd] {
		return true, true
	}
	words, plain := plainBashWords(cmd)
	if !plain || findAction(words) {
		return false, false
	}
	for p := range r.prefixes {
		if hasWordPrefix(words, strings.Split(p, " ")) {
			return true, true
		}
	}
	return false, false
}

// remember keeps one "for the session" answer.
func (r *sessionRules) remember(tool string, input json.RawMessage, allow bool) {
	if !allow {
		r.deny[tool] = true
		return
	}
	in := decodeInput(input)
	kind, value := scopeOf(tool, in)
	switch kind {
	case scopeTool:
		r.tools[tool] = true
	case scopePath:
		r.paths[value] = true
	case scopeCall:
		r.calls[callKey(tool, in)] = true
	case scopePrefix:
		r.prefixes[value] = true
	case scopeCommand:
		r.commands[value] = true
	}
}

// sessionScope states what an "Allow for session" on this call would cover:
// the kind and its value (the prefix, the exact command, the path or the
// tool name; empty for one exact call).
func sessionScope(tool string, input json.RawMessage) (kind, value string) {
	return scopeOf(tool, decodeInput(input))
}

// scopeOf is sessionScope for a decoded input.
func scopeOf(tool string, in any) (kind, value string) {
	switch tool {
	case "Bash":
		m, _ := in.(map[string]any)
		cmd, ok := m["command"].(string)
		if !ok {
			return scopeCall, ""
		}
		cmd = strings.TrimSpace(cmd)
		if prefix := bashPrefix(cmd); prefix != "" {
			return scopePrefix, prefix
		}
		return scopeCommand, cmd
	case "Edit", "Write":
		m, _ := in.(map[string]any)
		p, ok := m["file_path"].(string)
		if !ok || p == "" || !filepath.IsAbs(p) || strings.ContainsRune(p, 0) || hasParentElement(p) {
			return scopeCall, ""
		}
		return scopePath, filepath.Clean(p)
	}
	return scopeTool, tool
}

// hasParentElement reports a path with a ".." element (split on / and \):
// such a path is kept as that exact call, since resolving ".." by name can
// name another file than the one the link structure reaches. A path rule
// is otherwise the cleaned absolute path, so on Windows "C:/s/a.md" and
// "C:\s\a.md" are one rule; letter case is compared as written.
func hasParentElement(p string) bool {
	for _, e := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if e == ".." {
			return true
		}
	}
	return false
}

// decodeInput decodes a tool input (numbers kept exact); nil when it is not
// JSON.
func decodeInput(input json.RawMessage) any {
	var v any
	if len(input) == 0 || decodeNumbers(input, &v) != nil {
		return nil
	}
	return v
}

// callKey names one exact call: the tool and its input as canonical JSON
// (object keys sorted).
func callKey(tool string, in any) string {
	raw, err := json.Marshal(in)
	if err != nil {
		return tool + "\x00\x00unrepresentable"
	}
	return tool + "\x00" + string(raw)
}

func bashCommand(in any) string {
	m, _ := in.(map[string]any)
	s, _ := m["command"].(string)
	return s
}

// ─── Bash command rules ─────────────────────────────────────────────────────

// bashSpecial are the characters that make a command not plain: shell
// operators, redirections, substitutions, grouping, escapes and line breaks.
const bashSpecial = ";&|<>$`(){}\\\n\r"

// programWord is the shape a plain command's program word must have (no
// quotes, globs, keywords such as "!" or "[[", or assignments).
var programWord = regexp.MustCompile(`^[A-Za-z0-9_./~+:@%,-]+$`)

// subcommandWord is a bare subcommand ("status" in "git status").
var subcommandWord = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// versionSuffix strips a version from a program name (python3.12, go1.22).
var versionSuffix = regexp.MustCompile(`[0-9.]+$`)

// bashWrappers run another command or a script given to them: a plain
// command never starts with one.
var bashWrappers = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "csh": true, "tcsh": true,
	"env": true, "sudo": true, "doas": true, "su": true, "xargs": true, "eval": true, "exec": true, "command": true,
	"builtin": true, "nohup": true, "time": true, "timeout": true, "nice": true, "ionice": true, "stdbuf": true,
	"setsid": true, "flock": true, "watch": true, "noglob": true, "nocorrect": true, "source": true, ".": true,
	"python": true, "python2": true, "python3": true, "node": true, "nodejs": true, "deno": true, "bun": true,
	"perl": true, "ruby": true, "php": true, "lua": true, "osascript": true, "pwsh": true, "powershell": true,
	"npx": true, "bunx": true, "pnpx": true, "uvx": true, "ssh": true, "strace": true, "ltrace": true,
	"xvfb-run": true, "valgrind": true, "gdb": true, "lldb": true, "chroot": true, "unshare": true, "nsenter": true,
	"firejail": true, "systemd-run": true, "script": true, "expect": true, "parallel": true, "entr": true,
	"sqlite3": true, "sqlite": true,
}

// exactPrograms are commands that run scripts or commands given in their
// arguments, change files outside Edit and Write, or reach the network: an
// approval of one keeps the exact command, never a prefix.
var exactPrograms = map[string]bool{
	"sed": true, "vi": true, "vim": true, "nvim": true, "ex": true, "emacs": true, "zip": true, "awk": true,
	"gawk": true, "mawk": true, "nawk": true, "make": true, "taskset": true, "chrt": true, "busybox": true,
	"fakeroot": true, "tmux": true, "screen": true, "hyperfine": true, "pypy": true, "pypy3": true,
	"numactl": true, "prlimit": true, "runuser": true, "pkexec": true, "sg": true, "unbuffer": true,
	"proxychains": true, "proxychains4": true, "torsocks": true, "faketime": true, "at": true, "batch": true,
	"rscript": true, "tclsh": true, "julia": true, "rm": true, "rmdir": true, "dd": true, "shred": true,
	"truncate": true, "chmod": true, "chown": true, "chgrp": true, "mv": true, "cp": true, "ln": true,
	"install": true, "tee": true, "curl": true, "wget": true, "nc": true, "ncat": true, "netcat": true,
	"socat": true, "scp": true, "sftp": true, "ftp": true, "telnet": true,
}

// runnerSubcommands are subcommand words that, for any program, run
// another command (mise exec, npm x, uv tool, docker compose, bundle exec,
// conda run, git submodule foreach, gcloud compute ssh): a command with one
// after its program word is kept exact and never covered by a prefix.
var runnerSubcommands = map[string]bool{
	"exec": true, "x": true, "run": true, "dlx": true, "tool": true, "compose": true, "container": true,
	"shell": true, "foreach": true, "ssh": true,
}

// runnerPrograms have runner subcommands: an approval of one without a
// bare subcommand word is kept exact, since its flags could still name one.
var runnerPrograms = map[string]bool{
	"mise": true, "direnv": true, "docker": true, "podman": true, "devbox": true, "npm": true, "pnpm": true,
	"yarn": true, "uv": true, "poetry": true, "pipx": true, "kubectl": true, "nix": true, "go": true,
	"bundle": true, "conda": true, "mamba": true,
}

// findPrograms are the find commands (GNU and BSD names).
var findPrograms = map[string]bool{"find": true, "gfind": true, "bfind": true}

// findActions are find primaries that run commands, delete or write files.
var findActions = map[string]bool{
	"-exec": true, "-execdir": true, "-ok": true, "-okdir": true, "-delete": true, "-fprint": true,
	"-fprint0": true, "-fprintf": true, "-fls": true, "-files0-from": true,
}

// tarRunOptions are the tar options that run another program.
var tarRunOptions = []string{"--to-command", "--use-compress-program", "-I", "--checkpoint-action",
	"--info-script", "--new-volume-script", "-F", "--rsh-command"}

// runOptions are, per program, options that run another program or write
// files. A one letter option ("-e") also counts inside a cluster of short
// options ("-avze"); a longer one matches the word or its part before "=".
var runOptions = map[string][]string{
	"fd":     {"-x", "-X", "--exec", "--exec-batch"},
	"fdfind": {"-x", "-X", "--exec", "--exec-batch"},
	"rg":     {"--pre"},
	"git": {"-c", "-u", "-x", "--config-env", "--exec-path", "--upload-pack", "--receive-pack", "--exec",
		"--extcmd", "--output"},
	"go":     {"-exec", "-toolexec", "-vettool"},
	"tar":    tarRunOptions,
	"gtar":   tarRunOptions,
	"bsdtar": tarRunOptions,
	"rsync":  {"-e", "--rsh", "--rsync-path"},
}

// abbreviating are the programs that accept an unambiguous leading part of
// a long option ("--to-comm" for "--to-command"); goDashes the one that
// takes its options with one or two dashes.
var (
	abbreviating = map[string]bool{"tar": true, "gtar": true, "bsdtar": true, "git": true}
	goDashes     = map[string]bool{"go": true}
)

// programName is a program word's comparable name: its base name, lower
// case, without ".exe" or a trailing version.
func programName(word string) string {
	base := strings.ToLower(word[strings.LastIndex(word, "/")+1:])
	base = strings.TrimSuffix(base, ".exe")
	if trimmed := versionSuffix.ReplaceAllString(base, ""); trimmed != "" {
		return trimmed
	}
	return base
}

// isWrapper reports a program word that runs another command, or one whose
// approval always keeps the exact command (exactPrograms).
func isWrapper(word string) bool {
	base := strings.TrimSuffix(strings.ToLower(word[strings.LastIndex(word, "/")+1:]), ".exe")
	name := programName(word)
	return bashWrappers[base] || bashWrappers[name] || exactPrograms[base] || exactPrograms[name]
}

// plainBashWords splits a plain command into its words (quotes kept as
// typed); plain is false for anything a prefix rule may not cover.
func plainBashWords(cmd string) (words []string, plain bool) {
	if cmd == "" || !utf8.ValidString(cmd) || strings.ContainsAny(cmd, bashSpecial) {
		return nil, false
	}
	var cur strings.Builder
	var quote rune
	inWord := false
	for _, c := range cmd {
		if c != '\t' && (c < 0x20 || c == 0x7f) {
			return nil, false
		}
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			cur.WriteRune(c)
		case c == '\'' || c == '"':
			quote = c
			inWord = true
			cur.WriteRune(c)
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			inWord = true
			cur.WriteRune(c)
		}
	}
	if quote != 0 {
		return nil, false
	}
	if inWord {
		words = append(words, cur.String())
	}
	if len(words) == 0 || !programWord.MatchString(words[0]) || strings.Contains(words[0], "=") || isWrapper(words[0]) {
		return nil, false
	}
	return words, true
}

// bashPrefix is the prefix rule an approval of cmd keeps: the program word,
// plus its subcommand word; "" when the command is kept exactly (not
// plain, a runner subcommand, a find action or run option, or a runner
// program without a bare subcommand, whose flags could still name a
// runner).
func bashPrefix(cmd string) string {
	words, plain := plainBashWords(cmd)
	if !plain || findAction(words) {
		return ""
	}
	program := programName(words[0])
	if len(words) > 1 && subcommandWord.MatchString(words[1]) {
		if runnerSubcommands[words[1]] {
			return ""
		}
		return words[0] + " " + words[1]
	}
	if runnerPrograms[program] {
		return ""
	}
	return words[0]
}

// unquote removes quotes from a word, as the program receives it.
func unquote(w string) string { return strings.NewReplacer(`'`, "", `"`, "").Replace(w) }

// findAction reports a command no prefix rule may cover: a runner
// subcommand word after the program word, a find with a primary that runs
// a command, deletes or writes, or a program given one of its run options.
func findAction(words []string) bool {
	if len(words) == 0 {
		return false
	}
	for _, w := range words[1:] {
		if runnerSubcommands[unquote(w)] {
			return true
		}
	}
	program := programName(words[0])
	if findPrograms[program] {
		for _, w := range words[1:] {
			if findActions[unquote(w)] {
				return true
			}
		}
		return false
	}
	for i, w := range words[1:] {
		w = unquote(w)
		// tar's old style: a first argument without a dash is a cluster of
		// short options ("tar xIf prog a.tar").
		if i == 0 && tarRunOptionsFor(program) && w != "" && w[0] != '-' {
			w = "-" + w
		}
		if runOption(program, w) {
			return true
		}
	}
	return false
}

func tarRunOptionsFor(program string) bool {
	return program == "tar" || program == "gtar" || program == "bsdtar"
}

// runOption reports one argument word (quotes removed) that is one of the
// program's run options, in any spelling the program accepts.
func runOption(program, w string) bool {
	name, _, _ := strings.Cut(w, "=")
	for _, o := range runOptions[program] {
		switch {
		case name == o:
			return true
		case goDashes[program] && strings.HasPrefix(name, "-") && strings.TrimLeft(name, "-") == strings.TrimLeft(o, "-"):
			return true
		case abbreviating[program] && strings.HasPrefix(name, "--") && strings.HasPrefix(o, "--") &&
			len(name) >= 4 && strings.HasPrefix(o, name):
			return true
		}
		short := len(o) == 2 && o[0] == '-' && o[1] != '-'
		if short && len(w) > 2 && w[0] == '-' && w[1] != '-' && strings.ContainsRune(w[1:], rune(o[1])) {
			return true
		}
	}
	return false
}

// hasWordPrefix reports words starting with the prefix words (whole words
// only: "ls" never covers "lsof").
func hasWordPrefix(words, prefix []string) bool {
	if len(prefix) == 0 || len(words) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if words[i] != p {
			return false
		}
	}
	return true
}
