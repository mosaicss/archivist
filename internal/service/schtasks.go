package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/mosaicss/archivist/internal/fsutil"
)

// Story 78.34: on Windows background connect is a per-user scheduled task
// registered without admin rights: a logon trigger and a principal that
// both name the current user (only an "any user" trigger needs elevation),
// InteractiveToken, LeastPrivilege. Its action runs the console-less
// archivistw.exe `connect --service [args...]`, which supervises the daemon
// (restart after 10 s on an unexpected exit, stop on exit 0) because Task
// Scheduler's RestartOnFailure covers a failed launch, not a non-zero exit.
// Task Scheduler cannot carry an environment, so the captured one is
// written to service.env for the supervisor to apply. Status comes from
// schtasks exit codes and the daemon's pid file only: schtasks /query
// output is localised and never parsed.

// TaskPrefix starts every task name; the sanitised user name follows.
const TaskPrefix = "MosaicArchivistConnect"

// taskXMLName is the task definition kept under ~/.archivist/connect.
const taskXMLName = TaskPrefix + ".xml"

// TaskName is the scheduled task name for user (DOMAIN\name or name) with
// account security identifier sid: MosaicArchivistConnect-<name>-<hash>,
// the name reduced to letters, digits, dot, underscore and hyphen (for
// people reading Task Scheduler), and <hash> the first 8 hex digits of the
// SHA-256 of the SID, which Windows keeps when the PC or the account is
// renamed, so --install and --uninstall keep finding the task, and two
// accounts whose reduced names agree get distinct tasks. Without a SID the
// full user string is hashed instead.
func TaskName(user, sid string) string {
	key := sid
	if key == "" {
		key = user
	}
	sum := sha256.Sum256([]byte(key))
	short := user
	if i := strings.LastIndexAny(short, `\/`); i >= 0 {
		short = short[i+1:]
	}
	var b strings.Builder
	for _, r := range short {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if strings.Trim(name, "_") == "" {
		name = "user"
	}
	return TaskPrefix + "-" + name + "-" + hex.EncodeToString(sum[:4])
}

// TaskXMLPath is the task definition file, ~/.archivist/connect/MosaicArchivistConnect.xml.
func TaskXMLPath(home string) string {
	return filepath.Join(home, ".archivist", "connect", taskXMLName)
}

// EnvFilePath is the captured service environment the Windows supervisor
// applies to the daemon, ~/.archivist/connect/service.env.
func EnvFilePath(home string) string {
	return filepath.Join(home, ".archivist", "connect", "service.env")
}

// WriteEnvFile writes env as KEY=VALUE lines (0600, atomically). CaptureEnv
// already dropped values with control characters.
func WriteEnvFile(home string, env []EnvVar) error {
	path := EnvFilePath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	for _, e := range env {
		if e.Key == "" || strings.ContainsAny(e.Key, "=\r\n") || hasControl(e.Value) {
			continue
		}
		b.WriteString(e.Key + "=" + e.Value + "\n")
	}
	return writeFileAtomic(path, []byte(b.String()), 0o600)
}

// ReadEnvFile reads WriteEnvFile's file; nil when it is absent.
func ReadEnvFile(home string) []EnvVar {
	b, err := fsutil.ReadFile(EnvFilePath(home))
	if err != nil {
		return nil
	}
	var out []EnvVar
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if ok && k != "" && !hasControl(v) {
			out = append(out, EnvVar{Key: k, Value: v})
		}
	}
	return out
}

// MergeEnv is environ with the captured service variables applied: an
// entry whose key a captured variable names is replaced (keys compared
// case-insensitively when fold is set, as Windows does), ARCHIVIST_TOKEN is
// dropped (the service reads the saved key only), then extra entries are
// appended.
func MergeEnv(environ []string, captured []EnvVar, fold bool, extra ...string) []string {
	same := func(a, b string) bool {
		if fold {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	drop := func(k string) bool {
		if same(k, "ARCHIVIST_TOKEN") {
			return true
		}
		for _, e := range captured {
			if same(k, e.Key) {
				return true
			}
		}
		for _, kv := range extra {
			if ek, _, _ := strings.Cut(kv, "="); same(k, ek) {
				return true
			}
		}
		return false
	}
	out := make([]string, 0, len(environ)+len(captured)+len(extra))
	for _, kv := range environ {
		if k, _, ok := strings.Cut(kv, "="); ok && k != "" && drop(k) {
			continue
		}
		out = append(out, kv)
	}
	for _, e := range captured {
		out = append(out, e.Key+"="+e.Value)
	}
	return append(out, extra...)
}

// RenderTaskXML renders the scheduled task definition (Task Scheduler
// schema 1.2) as UTF-16LE with a byte order mark, the encoding its
// declaration names and schtasks /create /xml reads. The action runs
// `<bin> connect --service [args...]` in home; each argument is quoted as
// a Windows program parses its command line.
func RenderTaskXML(user, bin, home string, args []string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-16"?>` + "\r\n")
	b.WriteString(`<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` + "\r\n")
	line := func(indent int, s string) {
		b.WriteString(strings.Repeat("  ", indent) + s + "\r\n")
	}
	elem := func(indent int, name, value string) {
		line(indent, "<"+name+">"+xmlEscape(value)+"</"+name+">")
	}
	line(1, "<RegistrationInfo>")
	// No XML comment: "--" is illegal inside one, and schtasks rejects the
	// file as malformed (seen on the Windows CI runner). The removal hint
	// lives in the description instead.
	elem(2, "Description", "Archivist connect: drive your own Claude Code or Codex from Mosaic. Written by archivist connect --install; remove it with archivist connect --uninstall.")
	line(1, "</RegistrationInfo>")
	line(1, "<Triggers>")
	line(2, "<LogonTrigger>")
	elem(3, "Enabled", "true")
	elem(3, "UserId", user)
	line(2, "</LogonTrigger>")
	line(1, "</Triggers>")
	line(1, "<Principals>")
	line(2, `<Principal id="Author">`)
	elem(3, "UserId", user)
	elem(3, "LogonType", "InteractiveToken")
	elem(3, "RunLevel", "LeastPrivilege")
	line(2, "</Principal>")
	line(1, "</Principals>")
	line(1, "<Settings>")
	elem(2, "MultipleInstancesPolicy", "IgnoreNew")
	elem(2, "DisallowStartIfOnBatteries", "false")
	elem(2, "StopIfGoingOnBatteries", "false")
	elem(2, "AllowHardTerminate", "true")
	elem(2, "StartWhenAvailable", "false")
	elem(2, "RunOnlyIfNetworkAvailable", "false")
	line(2, "<IdleSettings>")
	elem(3, "StopOnIdleEnd", "false")
	elem(3, "RestartOnIdle", "false")
	line(2, "</IdleSettings>")
	elem(2, "AllowStartOnDemand", "true")
	elem(2, "Enabled", "true")
	elem(2, "Hidden", "false")
	elem(2, "RunOnlyIfIdle", "false")
	elem(2, "WakeToRun", "false")
	elem(2, "ExecutionTimeLimit", "PT0S")
	elem(2, "Priority", "7")
	line(2, "<RestartOnFailure>")
	elem(3, "Interval", "PT1M")
	elem(3, "Count", "3")
	line(2, "</RestartOnFailure>")
	line(1, "</Settings>")
	line(1, `<Actions Context="Author">`)
	line(2, "<Exec>")
	elem(3, "Command", bin)
	argv := append([]string{"connect", "--service"}, args...)
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = windowsArg(a)
	}
	elem(3, "Arguments", strings.Join(quoted, " "))
	elem(3, "WorkingDirectory", home)
	line(2, "</Exec>")
	line(1, "</Actions>")
	b.WriteString("</Task>\r\n")
	return encodeUTF16LE(b.String())
}

// encodeUTF16LE is s as UTF-16LE with a byte order mark.
func encodeUTF16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2, 2+2*len(units))
	out[0], out[1] = 0xFF, 0xFE
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// decodeUTF16LE undoes encodeUTF16LE for content starting with its byte
// order mark; ok is false for anything else.
func decodeUTF16LE(content string) (string, bool) {
	if len(content) < 2 || content[0] != 0xFF || content[1] != 0xFE {
		return "", false
	}
	b := content[2:]
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		units = append(units, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(units)), true
}

// windowsArg quotes s as one command line argument the way a Windows
// program (the Go runtime and CommandLineToArgvW) splits it back: the
// algorithm of syscall.EscapeArg, which exists only in Windows builds.
func windowsArg(s string) string {
	if s == "" {
		return `""`
	}
	needsBackslash, hasSpace := false, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			needsBackslash = true
		case ' ', '\t':
			hasSpace = true
		}
	}
	if !needsBackslash && !hasSpace {
		return s
	}
	if !needsBackslash {
		return `"` + s + `"`
	}
	var b strings.Builder
	if hasSpace {
		b.WriteByte('"')
	}
	slashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		default:
			slashes = 0
		case '\\':
			slashes++
		case '"':
			for ; slashes > 0; slashes-- {
				b.WriteByte('\\')
			}
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	if hasSpace {
		for ; slashes > 0; slashes-- {
			b.WriteByte('\\')
		}
		b.WriteByte('"')
	}
	return b.String()
}

// splitWindowsArgs splits a command line tail into arguments with the
// rules the Go runtime uses for os.Args on Windows (os.commandLineToArgv).
func splitWindowsArgs(cmd string) []string {
	var args []string
	for len(cmd) > 0 {
		if cmd[0] == ' ' || cmd[0] == '\t' {
			cmd = cmd[1:]
			continue
		}
		var arg []byte
		arg, cmd = readWindowsArg(cmd)
		args = append(args, string(arg))
	}
	return args
}

func readWindowsArg(cmd string) (arg []byte, rest string) {
	var b []byte
	var inquote bool
	var nslash int
	for ; len(cmd) > 0; cmd = cmd[1:] {
		c := cmd[0]
		switch c {
		case ' ', '\t':
			if !inquote {
				return appendBackslashes(b, nslash), cmd[1:]
			}
		case '"':
			b = appendBackslashes(b, nslash/2)
			if nslash%2 == 0 {
				// A doubled quote inside quotes is a literal quote (the
				// "prior to 2008" rule the Go runtime follows).
				if inquote && len(cmd) > 1 && cmd[1] == '"' {
					b = append(b, c)
					cmd = cmd[1:]
				}
				inquote = !inquote
			} else {
				b = append(b, c)
			}
			nslash = 0
			continue
		case '\\':
			nslash++
			continue
		}
		b = appendBackslashes(b, nslash)
		nslash = 0
		b = append(b, c)
	}
	return appendBackslashes(b, nslash), ""
}

func appendBackslashes(b []byte, n int) []byte {
	for ; n > 0; n-- {
		b = append(b, '\\')
	}
	return b
}

// schtasks manages the per-user logon task through schtasks.exe.
type schtasks struct{ opts Options }

func newSchtasks(opts Options) *schtasks { return &schtasks{opts: opts} }

func (s *schtasks) Name() string { return "Task Scheduler" }

func (s *schtasks) Path() string { return TaskXMLPath(s.opts.Home) }

func (s *schtasks) task() string { return TaskName(s.opts.User, s.opts.SID) }

func (s *schtasks) schtasks(ctx context.Context, args ...string) (Result, error) {
	r, err := s.opts.run(ctx, "schtasks", args...)
	if err != nil {
		return r, fmt.Errorf("schtasks is not available (%w). Run archivist connect in a terminal instead", err)
	}
	return r, nil
}

// registered reports whether the task exists (schtasks /query exits 0).
func (s *schtasks) registered(ctx context.Context) (bool, error) {
	r, err := s.schtasks(ctx, "/query", "/tn", s.task())
	if err != nil {
		return false, err
	}
	return r.OK(), nil
}

// Polling bounds (the launchd values): an ended task's daemon gets 10 s to
// exit, a started one about 5 s to write its pid.
const (
	schtasksEndWait   = launchdUnloadWait
	schtasksStartWait = launchdStartWait
)

// Install ends a previous run and waits for its daemon to exit, writes
// service.env and the task XML, registers the task (/create /xml /f
// replaces the earlier definition), runs it and waits about 5 s for the new
// daemon's pid. A failed registration puts the previous files back (or
// removes files this call created) and, on a reinstall, runs the previous
// definition again, so a refused update leaves the service as it was.
func (s *schtasks) Install(ctx context.Context) (InstallReport, error) {
	if s.opts.Binary == "" || !filepath.IsAbs(s.opts.Binary) {
		return InstallReport{}, fmt.Errorf("service: binary path %q is not absolute", s.opts.Binary)
	}
	if s.opts.User == "" {
		return InstallReport{}, errors.New("service: the current user name is unknown")
	}
	path, envPath := s.Path(), EnvFilePath(s.opts.Home)
	rep := InstallReport{Path: path}
	if err := EnsureLog(s.opts.Home); err != nil {
		return rep, err
	}
	was, err := s.registered(ctx)
	if err != nil {
		return rep, err
	}
	old := readPID(s.opts.Home)
	if !was && old > 0 && s.opts.alive(old) {
		// The task is gone (deleted in Task Scheduler) while its daemon
		// still runs: a /run would start a second, untracked daemon.
		return rep, fmt.Errorf("background connect still runs (pid %d) although its scheduled task is gone; nothing was changed. End it in Task Manager and run 'archivist connect --install' again", old)
	}
	if was {
		// The running instance ends before the definition changes. Not
		// running answers non-zero; either way the old run must be over.
		_, _ = s.schtasks(ctx, "/end", "/tn", s.task())
		for i := 0; i < schtasksEndWait && old > 0 && s.opts.alive(old); i++ {
			s.opts.sleep(launchdPoll)
		}
		if old > 0 && s.opts.alive(old) {
			// MultipleInstancesPolicy IgnoreNew would make the /run below a
			// no-op while the old daemon runs on: nothing is changed.
			return rep, fmt.Errorf("the previous background connect run (pid %d) did not stop within 10 s; nothing was changed. End it in Task Manager and run 'archivist connect --install' again", old)
		}
		rep.Restarted = true
	}
	// A hard kill (task end, logoff) leaves service.pid behind; once the
	// old run is gone it is removed, so a reused pid is never taken for the
	// daemon.
	removePIDFile(s.opts.Home)
	prevXML, xmlErr := os.ReadFile(path)
	prevEnv, envErr := os.ReadFile(envPath)
	restore := func() {
		restoreFile(path, prevXML, xmlErr)
		restoreFile(envPath, prevEnv, envErr)
		if was {
			_, _ = s.schtasks(ctx, "/run", "/tn", s.task())
		}
	}
	if err := WriteEnvFile(s.opts.Home, s.opts.Env); err != nil {
		restore()
		return rep, err
	}
	if err := writeFileAtomic(path, RenderTaskXML(s.opts.User, s.opts.Binary, s.opts.Home, s.opts.Args), 0o600); err != nil {
		restore()
		return rep, err
	}
	r, err := s.schtasks(ctx, "/create", "/xml", path, "/tn", s.task(), "/f")
	if err == nil && !r.OK() {
		err = commandError("schtasks /create", r)
	}
	if err != nil {
		restore()
		return rep, err
	}
	if r, err = s.schtasks(ctx, "/run", "/tn", s.task()); err != nil {
		return rep, err
	}
	if !r.OK() {
		return rep, commandError("schtasks /run", r)
	}
	for i := 0; i < schtasksStartWait && ctx.Err() == nil; i++ {
		if pid := readPID(s.opts.Home); pid != 0 && pid != old && s.opts.alive(pid) {
			break
		}
		s.opts.sleep(launchdPoll)
	}
	return rep, nil
}

// restoreFile puts back content read before an install (readErr nil), or
// removes a file the install created (readErr set).
func restoreFile(path string, content []byte, readErr error) {
	if readErr != nil {
		_ = os.Remove(path)
		return
	}
	_ = writeFileAtomic(path, content, 0o600)
}

// Uninstall ends and deletes the task and removes its XML and service.env.
// A failed delete keeps the files, so the task stays visible to --status.
func (s *schtasks) Uninstall(ctx context.Context) (bool, error) {
	path, env := s.Path(), EnvFilePath(s.opts.Home)
	was, err := s.registered(ctx)
	if err != nil {
		return false, err
	}
	if !was && !fileExists(path) && !fileExists(env) {
		return false, nil
	}
	if was {
		_, _ = s.schtasks(ctx, "/end", "/tn", s.task())
		r, err := s.schtasks(ctx, "/delete", "/tn", s.task(), "/f")
		if err == nil && !r.OK() {
			err = commandError("schtasks /delete", r)
		}
		if err != nil {
			return false, err
		}
	}
	removePIDFile(s.opts.Home)
	for _, p := range []string{path, env} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return true, err
		}
	}
	return true, nil
}

// removePIDFile removes service.pid (best effort).
func removePIDFile(home string) { _ = os.Remove(PIDPath(home)) }

// Status reads schtasks /query's exit code (registered or not) and the
// daemon's service.pid: a registered task whose daemon stopped (exit 0:
// superseded, no harness, revoked key) is "loaded, not running". A task run
// a moment ago gets about 5 s for its daemon's pid.
func (s *schtasks) Status(ctx context.Context) (Status, error) {
	st := Status{Manager: s.Name(), Path: s.Path(), Installed: fileExists(s.Path())}
	if st.Installed {
		st.Args = installedArgs(st.Path)
	}
	reg, err := s.registered(ctx)
	if err != nil {
		return st, err
	}
	st.Loaded = reg
	if !reg {
		return st, nil
	}
	for i := 0; ; i++ {
		if s.opts.alive(readPID(s.opts.Home)) {
			st.Running = true
			return st, nil
		}
		if i >= schtasksStartWait || ctx.Err() != nil {
			return st, nil
		}
		s.opts.sleep(launchdPoll)
	}
}
