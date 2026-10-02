package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
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

// CodexInfo is what detection learned about a local Codex CLI.
type CodexInfo struct {
	Path    string
	Version string
}

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

// Capabilities reports Claude (available only when usable) and, when
// installed, Codex as unavailable until its adapter exists (Story 78.17).
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
		caps = append(caps, Capability{Agent: "codex", Version: capabilityVersion(d.Codex.Version)})
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
// environment; dir is the working directory for the probes.
func Detect(ctx context.Context, lookPath LookPath, run Runner, env []string, dir string) Detection {
	var d Detection
	d.Claude = detectClaude(ctx, lookPath, run, env, dir)
	if path, err := lookPath("codex"); err == nil {
		d.Codex.Path = path
		if out, err := run(ctx, env, dir, path, "--version"); err == nil {
			d.Codex.Version = parseVersion(string(out))
		}
	}
	return d
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
