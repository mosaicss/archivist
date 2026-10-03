package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ClaudeFloor is the oldest Claude Code the adapter drives: the version the
// 78.3 spike (S4) proved. Newer versions are reported, not blocked.
const ClaudeFloor = "2.1.280"

// detectTimeout bounds each detection command.
const detectTimeout = 20 * time.Second

// AuthStatus is the subset of `claude auth status --json` the daemon reads.
type AuthStatus struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	SubscriptionType string `json:"subscriptionType"`
}

// Subscription reports whether the login is a claude.ai subscription.
func (a *AuthStatus) Subscription() bool {
	return a != nil && a.LoggedIn && a.AuthMethod == "claude.ai"
}

// ClaudeInfo is what detection learned about the local Claude Code.
type ClaudeInfo struct {
	Path      string
	Version   string
	VersionOK bool
	Auth      *AuthStatus
	// Problem names why Claude is not usable ("" when usable).
	Problem string
	// ProblemCode classifies Problem: "missing", "version", "auth".
	ProblemCode string
}

// Usable reports whether sessions may start with this Claude Code.
func (c ClaudeInfo) Usable() bool { return c.Problem == "" }

// CodexFloor is the oldest Codex CLI the adapter drives: the version whose
// app-server protocol Story 78.17 was built and verified against. Newer
// versions are reported, not blocked.
const CodexFloor = "0.160.0"

// CodexInfo is what detection learned about a local Codex CLI.
type CodexInfo struct {
	Path      string
	Version   string
	VersionOK bool
	// LoggedIn is `codex login status` (a local check, no model call)
	// exiting 0 and reporting a ChatGPT login.
	LoggedIn bool
	// Login is the status line `codex login status` printed (stdout or stderr).
	Login string
	// Home is the owner's Codex home (CODEX_HOME if absolute, else ~/.codex).
	Home string
	// AuthPresent reports that Home holds an auth.json file.
	AuthPresent bool
	// Problem names why Codex is not usable ("" when usable).
	Problem string
}

// Usable reports whether Codex sessions may start: installed, at or above
// the floor, logged in and with the owner's auth.json present. The
// ChatGPT subscription itself is proven per session (account/read).
func (c CodexInfo) Usable() bool { return c.Path != "" && c.Problem == "" }

// Detection is the local harness inventory.
type Detection struct {
	Claude ClaudeInfo
	Codex  CodexInfo
}

// Capability is one relay capabilities record (78.14 strict shape).
type Capability struct {
	Agent     string `json:"agent"`
	Version   string `json:"version"`
	Available bool   `json:"available"`
	LoggedIn  bool   `json:"loggedIn"`
}

// Capabilities reports each installed harness; available only when usable.
func (d Detection) Capabilities() []Capability {
	caps := []Capability{}
	if d.Claude.Path != "" {
		caps = append(caps, Capability{
			Agent:     "claude",
			Version:   capabilityVersion(d.Claude.Version),
			Available: d.Claude.Usable(),
			LoggedIn:  d.Claude.Auth != nil && d.Claude.Auth.LoggedIn,
		})
	}
	if d.Codex.Path != "" {
		caps = append(caps, Capability{Agent: "codex", Version: capabilityVersion(d.Codex.Version),
			Available: d.Codex.Usable(), LoggedIn: d.Codex.LoggedIn})
	}
	return caps
}

// capabilityVersion fits the relay rule: nonempty, trimmed, at most 64
// characters, no ASCII control characters.
func capabilityVersion(v string) string {
	v = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, v)
	v = strings.TrimSpace(v)
	if len(v) > 64 {
		v = strings.TrimSpace(v[:64])
	}
	if v == "" {
		return "unknown"
	}
	return v
}

// Runner runs a detection command and returns its stdout. Tests replace it.
type Runner func(ctx context.Context, env []string, dir, bin string, args ...string) ([]byte, error)

// ExecCombinedRunner is ExecRunner returning stdout and stderr together
// (for probes that report on stderr).
func ExecCombinedRunner(ctx context.Context, env []string, dir, bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	return out, nil
}

// ExecRunner runs bin with exactly env and dir.
func ExecRunner(ctx context.Context, env []string, dir, bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err,
			Scrub(strings.TrimSpace(firstLine(stderr.String()))))
	}
	return stdout.Bytes(), nil
}

// LookPath resolves a harness binary on the daemon's PATH (never from the relay).
type LookPath func(file string) (string, error)

// Detect inventories Claude Code and Codex. env is the allowlisted child
// environment; dir is the working directory for the probes. Without an
// owner Codex home (DetectWith) Codex is reported but never usable.
func Detect(ctx context.Context, lookPath LookPath, run Runner, env []string, dir string) Detection {
	return DetectWith(ctx, DetectOptions{LookPath: lookPath, Run: run, Env: env, Dir: dir})
}

// DetectOptions configures DetectWith.
type DetectOptions struct {
	LookPath LookPath
	Run      Runner
	// Env is the allowlisted child environment (no CODEX_HOME).
	Env []string
	Dir string
	// CodexHome is the owner's Codex home (OwnerCodexHome); codex probes run
	// with CODEX_HOME set to it. "" leaves Codex unusable.
	CodexHome string
	// RunCombined runs `codex login status`, which prints on stderr: it
	// returns stdout and stderr together. nil uses Run.
	RunCombined Runner
}

// DetectWith inventories Claude Code and Codex.
func DetectWith(ctx context.Context, o DetectOptions) Detection {
	var d Detection
	d.Claude = detectClaude(ctx, o.LookPath, o.Run, o.Env, o.Dir)
	d.Codex = detectCodex(ctx, o)
	return d
}

func detectCodex(ctx context.Context, o DetectOptions) CodexInfo {
	var c CodexInfo
	path, err := o.LookPath("codex")
	if err != nil {
		return c
	}
	c.Path, c.Home = path, o.CodexHome
	env := o.Env
	if c.Home != "" {
		env = append(append([]string(nil), o.Env...), "CODEX_HOME="+c.Home)
	}
	if out, err := o.Run(ctx, env, o.Dir, path, "--version"); err == nil {
		c.Version = parseVersion(string(out))
	}
	c.VersionOK = c.Version != "" && !versionLess(c.Version, CodexFloor)
	// codex-cli prints "Logged in using ChatGPT" (or "... an API key", or
	// "Not logged in") on stderr and exits 0 or 1; only a ChatGPT login
	// counts, any error counts as logged out.
	runLogin := o.RunCombined
	if runLogin == nil {
		runLogin = o.Run
	}
	out, loginErr := runLogin(ctx, env, o.Dir, path, "login", "status")
	c.Login = firstLine(string(out))
	switch {
	case loginErr != nil || strings.Contains(c.Login, "Not logged in"):
	case strings.Contains(c.Login, "ChatGPT"):
		c.LoggedIn = true
	case c.Login != "":
		c.Problem = fmt.Sprintf("Codex reports %q; archivist connect only drives a ChatGPT login (run 'codex login' and sign in with ChatGPT)", c.Login)
	}
	if c.Home != "" {
		if st, err := os.Stat(filepath.Join(c.Home, "auth.json")); err == nil && st.Mode().IsRegular() {
			c.AuthPresent = true
		}
	}
	switch {
	case c.Problem != "":
	case c.Version == "":
		c.Problem = "could not read the Codex version"
	case !c.VersionOK:
		c.Problem = fmt.Sprintf("Codex %s is older than the supported floor %s; update Codex", c.Version, CodexFloor)
	case !c.LoggedIn:
		c.Problem = "Codex is not logged in; run 'codex login' and sign in with ChatGPT"
	case c.Home == "":
		c.Problem = "the Codex home directory is unknown"
	case !c.AuthPresent:
		c.Problem = fmt.Sprintf("no auth.json in %s; run 'codex login' and sign in with ChatGPT", c.Home)
	}
	return c
}

// OwnerCodexHome is the owner's Codex home: the daemon's CODEX_HOME when it
// is an absolute path, else ~/.codex. Sessions never run in it; they link
// its auth.json into a private per-session home.
func OwnerCodexHome(environ []string, userHome string) string {
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, "CODEX_HOME="); ok && filepath.IsAbs(v) {
			return filepath.Clean(v)
		}
	}
	if userHome == "" {
		return ""
	}
	return filepath.Join(userHome, ".codex")
}

func detectClaude(ctx context.Context, lookPath LookPath, run Runner, env []string, dir string) ClaudeInfo {
	var c ClaudeInfo
	path, err := lookPath("claude")
	if err != nil {
		c.Problem, c.ProblemCode = "Claude Code (claude) was not found on PATH", "missing"
		return c
	}
	c.Path = path
	out, err := run(ctx, env, dir, path, "--version")
	if err != nil {
		c.Problem, c.ProblemCode = "claude --version failed: "+err.Error(), "version"
		return c
	}
	c.Version = parseVersion(string(out))
	if c.Version == "" {
		c.Problem, c.ProblemCode = fmt.Sprintf("could not read the Claude Code version from %q", firstLine(string(out))), "version"
		return c
	}
	c.VersionOK = !versionLess(c.Version, ClaudeFloor)
	auth, authErr := ClaudeAuthStatus(ctx, run, env, dir, path)
	c.Auth = auth
	switch {
	case !c.VersionOK:
		c.Problem, c.ProblemCode = fmt.Sprintf("Claude Code %s is older than the supported floor %s; update Claude Code", c.Version, ClaudeFloor), "version"
	case authErr != nil:
		c.Problem, c.ProblemCode = "claude auth status failed: "+authErr.Error(), "auth"
	case !auth.LoggedIn:
		c.Problem, c.ProblemCode = "Claude Code is not logged in; run 'claude' and log in with your Claude subscription", "auth"
	case auth.AuthMethod != "claude.ai":
		c.Problem, c.ProblemCode = fmt.Sprintf("Claude Code is logged in with %q; archivist connect only drives a claude.ai subscription login", auth.AuthMethod), "auth"
	}
	return c
}

// ClaudeAuthStatus runs `claude auth status --json` with env and dir.
func ClaudeAuthStatus(ctx context.Context, run Runner, env []string, dir, bin string) (*AuthStatus, error) {
	out, err := run(ctx, env, dir, bin, "auth", "status", "--json")
	// Logged-out status exits nonzero on some versions but still prints JSON.
	var st AuthStatus
	if jerr := json.Unmarshal(bytes.TrimSpace(out), &st); jerr != nil {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("auth status is not JSON: %w", jerr)
	}
	return &st, nil
}

var versionRe = regexp.MustCompile(`\b(\d+\.\d+\.\d+)\b`)

// parseVersion extracts the first x.y.z from version output.
func parseVersion(s string) string {
	if m := versionRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// versionLess reports a < b for x.y.z versions; unparsable compares false.
func versionLess(a, b string) bool {
	pa, pb := splitVersion(a), splitVersion(b)
	if pa == nil || pb == nil {
		return false
	}
	for i := range 3 {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func splitVersion(v string) []int {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
