package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"html"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode"
)

// Label is the launchd job label (macOS).
const Label = "com.mosaic-finance.archivist.connect"

// UnitName is the systemd user unit name (Linux).
const UnitName = "archivist-connect.service"

// EnvVar is one environment variable written into the service definition.
type EnvVar struct {
	Key   string
	Value string
}

// capturedKeys are the only variables a service definition carries, in the
// order written. Never ARCHIVIST_TOKEN (the service reads the saved
// credentials file) and never any other variable.
var capturedKeys = []string{
	"PATH", "HOME", "LANG", "LC_ALL", "CODEX_HOME", "CLAUDE_CONFIG_DIR",
	"ARCHIVIST_BASE_URL", "ARCHIVIST_RELAY_URL",
}

// absoluteOnly keys are paths a service manager would resolve against its
// own working directory: a relative value is dropped.
var absoluteOnly = map[string]bool{"CODEX_HOME": true, "CLAUDE_CONFIG_DIR": true}

// CaptureEnv picks the service environment from environ (os.Environ form):
// PATH, HOME, LANG, LC_ALL, an absolute CODEX_HOME and CLAUDE_CONFIG_DIR,
// and ARCHIVIST_BASE_URL and ARCHIVIST_RELAY_URL when set. Empty values and
// values with control characters are dropped. On Windows keys match
// case-insensitively (Path is PATH there, Story 78.34).
func CaptureEnv(environ []string) []EnvVar {
	return captureEnv(environ, runtime.GOOS == "windows")
}

// captureEnv is CaptureEnv; fold matches keys case-insensitively.
func captureEnv(environ []string, fold bool) []EnvVar {
	norm := func(k string) string {
		if fold {
			return strings.ToUpper(k)
		}
		return k
	}
	vals := map[string]string{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			vals[norm(k)] = v
		}
	}
	var out []EnvVar
	for _, k := range capturedKeys {
		v, ok := vals[norm(k)]
		if !ok || v == "" || hasControl(v) {
			continue
		}
		if absoluteOnly[k] && !filepath.IsAbs(v) {
			continue
		}
		out = append(out, EnvVar{Key: k, Value: v})
	}
	return out
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// RenderSystemdUnit renders the systemd user unit that runs
// `<bin> connect --service [args...]` (args: the daemon flags the install
// chose, such as --max-permission <mode>). Restart=on-failure restarts a crash after 10 s;
// a clean exit (Ctrl-C equivalent, superseded, revoked key, feature off, no
// harness) stays stopped. KillMode=mixed sends SIGTERM to the daemon only,
// so it can stop its harness sessions and revoke their tokens, then SIGKILL
// to whatever is left after TimeoutStopSec.
func RenderSystemdUnit(bin string, args []string, env []EnvVar) string {
	var b strings.Builder
	b.WriteString("# Written by archivist connect --install. Remove with: archivist connect --uninstall\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Archivist connect (drive your own Claude Code or Codex from Mosaic)\n")
	b.WriteString("Documentation=https://github.com/mosaicss/archivist\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	fmt.Fprintf(&b, "ExecStart=%s connect --service", systemdQuote(bin, true))
	for _, a := range args {
		b.WriteString(" " + systemdArg(a))
	}
	b.WriteString("\n")
	for _, e := range env {
		fmt.Fprintf(&b, "Environment=%s\n", systemdQuote(e.Key+"="+e.Value, false))
	}
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=10\n")
	b.WriteString("KillMode=mixed\n")
	b.WriteString("TimeoutStopSec=30\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// systemdQuote double quotes s for a unit file (systemd.syntax(7) quoting):
// backslash and double quote are escaped, % is doubled (specifiers expand in
// both ExecStart= and Environment=) and, in a command line, $ is doubled
// (ExecStart= expands variables; Environment= does not).
func systemdQuote(s string, command bool) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%")
	q := r.Replace(s)
	if command {
		q = strings.ReplaceAll(q, "$", "$$")
	}
	return `"` + q + `"`
}

// systemdArg is one ExecStart= argument: bare when it holds only
// characters systemd passes through unchanged, else double quoted.
func systemdArg(s string) string {
	if plainArg.MatchString(s) {
		return s
	}
	return systemdQuote(s, true)
}

var plainArg = regexp.MustCompile(`^[A-Za-z0-9._:/=+-]+$`)

// RenderLaunchdPlist renders the launchd agent that runs
// `<bin> connect --service [args...]` at login (ProgramArguments, one
// string per argument, no shell). KeepAlive SuccessfulExit false
// restarts only a non-zero exit, no sooner than ThrottleInterval seconds.
func RenderLaunchdPlist(bin, home, logPath string, args []string, env []EnvVar) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<!-- Written by archivist connect --install. Remove with: archivist connect --uninstall -->` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	kv := func(k, v string) {
		fmt.Fprintf(&b, "\t<key>%s</key>\n\t<string>%s</string>\n", xmlEscape(k), xmlEscape(v))
	}
	kv("Label", Label)
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range append([]string{bin, "connect", "--service"}, args...) {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", xmlEscape(a))
	}
	b.WriteString("\t</array>\n")
	if len(env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, e := range env {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", xmlEscape(e.Key), xmlEscape(e.Value))
		}
		b.WriteString("\t</dict>\n")
	}
	kv("WorkingDirectory", home)
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	b.WriteString("\t<key>ThrottleInterval</key>\n\t<integer>10</integer>\n")
	b.WriteString("\t<key>ExitTimeOut</key>\n\t<integer>30</integer>\n")
	kv("StandardOutPath", logPath)
	kv("StandardErrorPath", logPath)
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func xmlEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// ServiceArgs returns the arguments after `connect --service` in a unit,
// plist or task XML this package rendered (systemd ExecStart=, launchd
// ProgramArguments, or the task's Command and Arguments, read from its
// UTF-16LE encoding), or nil when there are none or the file is not one of
// ours.
func ServiceArgs(content string) []string {
	var argv []string
	if decoded, ok := decodeUTF16LE(content); ok {
		content = decoded
	}
	if strings.Contains(content, "<Task ") {
		command, ok1 := taskElement(content, "Command")
		arguments, ok2 := taskElement(content, "Arguments")
		if !ok1 || !ok2 {
			return nil
		}
		argv = append([]string{command}, splitWindowsArgs(arguments)...)
	} else if strings.Contains(content, "<plist") {
		_, rest, ok := strings.Cut(content, "<key>ProgramArguments</key>")
		if !ok {
			return nil
		}
		block, _, ok := strings.Cut(rest, "</array>")
		if !ok {
			return nil
		}
		for _, m := range plistString.FindAllStringSubmatch(block, -1) {
			argv = append(argv, html.UnescapeString(m[1]))
		}
	} else {
		for _, line := range strings.Split(content, "\n") {
			if v, ok := strings.CutPrefix(line, "ExecStart="); ok {
				argv = systemdSplit(v)
				break
			}
		}
	}
	if len(argv) <= 3 || argv[1] != "connect" || argv[2] != "--service" {
		return nil
	}
	return argv[3:]
}

var plistString = regexp.MustCompile(`<string>([^<]*)</string>`)

// taskElement is the unescaped text of the first <name>...</name> in a task
// XML.
func taskElement(content, name string) (string, bool) {
	_, rest, ok := strings.Cut(content, "<"+name+">")
	if !ok {
		return "", false
	}
	text, _, ok := strings.Cut(rest, "</"+name+">")
	if !ok {
		return "", false
	}
	return html.UnescapeString(text), true
}

// systemdSplit undoes RenderSystemdUnit's ExecStart= quoting: words split on
// spaces, double quoted words with backslash escapes, %% and $$ halved.
func systemdSplit(line string) []string {
	var out []string
	var cur strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quoted && c == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
		case c == '"':
			quoted = !quoted
			inWord = true
		case !quoted && (c == ' ' || c == '\t'):
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		case (c == '%' || c == '$') && i+1 < len(line) && line[i+1] == c:
			i++
			cur.WriteByte(c)
			inWord = true
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}
