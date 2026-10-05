package cmd

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/connect"
)

// sessionHarness keeps a logged-out harness for its sign-in, runs Claude
// Code on any logged in login (Story 78.38: a login other than claude.ai is
// the sandbox's Console login mode) and refuses a missing harness or a Codex
// API key login; sessionConfigs configures only the requested harness, Bin
// included when it is logged out.
func TestSessionHarnessAndConfigs(t *testing.T) {
	loggedOut := `{"loggedIn":false,"authMethod":"none"}`
	sub := `{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}`
	cases := []struct {
		name, agent string
		det         connect.Detection
		signIn      string
		login       connect.ClaudeLogin
		exit        int
	}{
		{"claude logged out", "claude", detectWith([]string{"claude"}, "2.1.285", loggedOut, ""), "claude", 0, 0},
		{"claude usable", "claude", detectWith([]string{"claude"}, "2.1.285", sub, ""), "", 0, 0},
		{"claude console key", "claude", detectWith([]string{"claude"}, "2.1.285", `{"loggedIn":true,"authMethod":"api_key"}`, ""), "",
			connect.ClaudeLoginConsole, 0},
		{"claude missing", "claude", detectWith(nil, "", "", ""), "", 0, ExitNotFound},
		{"claude below floor", "claude", detectWith([]string{"claude"}, "2.0.1", loggedOut, ""), "", 0, ExitGenericError},
		{"codex logged out", "codex", connect.Detection{Codex: connect.CodexInfo{Path: "/opt/codex", Version: "0.160.0",
			VersionOK: true, Home: "/h/.codex", Problem: "Codex is not logged in"}}, "codex", 0, 0},
		{"codex no auth.json", "codex", connect.Detection{Codex: connect.CodexInfo{Path: "/opt/codex", Version: "0.160.0",
			VersionOK: true, LoggedIn: true, Home: "/h/.codex", Problem: "no auth.json"}}, "codex", 0, 0},
		{"codex api key", "codex", connect.Detection{Codex: connect.CodexInfo{Path: "/opt/codex", Version: "0.160.0",
			VersionOK: true, APILogin: true, Home: "/h/.codex", Problem: "api key"}}, "", 0, ExitAuthError},
		{"codex missing", "codex", connect.Detection{}, "", 0, ExitNotFound},
	}
	for _, c := range cases {
		signIn, login, err := sessionHarness(io.Discard, c.det, c.agent)
		if signIn != c.signIn || login != c.login || exitOf(err) != c.exit {
			t.Errorf("%s: signIn %q login %v exit %d, want %q %v %d", c.name, signIn, login, exitOf(err), c.signIn, c.login, c.exit)
		}
	}
	det := connect.Detection{
		Claude: connect.ClaudeInfo{Path: "/opt/claude", Problem: "not logged in", ProblemCode: "auth"},
		Codex:  connect.CodexInfo{Path: "/opt/codex", Version: "0.160.0", Home: "/h/.codex", Problem: "not logged in"},
	}
	start := &connect.SessionStart{SessionID: "x", Agent: "codex", Prompt: "p"}
	cl, cx := sessionConfigs(det, connectFlags{session: start}, "/bin/archivist", "")
	if cl.Bin != "" || cx.Bin != "/opt/codex" || cx.OwnerHome != "/h/.codex" || cx.Executable != "/bin/archivist" {
		t.Fatalf("codex session configs %+v %+v", cl, cx)
	}
	start.Agent = "claude"
	cl, cx = sessionConfigs(det, connectFlags{session: start}, "/bin/archivist", "")
	if cl.Bin != "/opt/claude" || cx.Bin != "" {
		t.Fatalf("claude session configs %+v %+v", cl, cx)
	}
}

// ARCHIVIST_ACTIVITY_FILE (Story 78.37): an absolute path is used (cleaned);
// a relative one is ignored with a warning, never failing the task.
func TestActivityFileEnv(t *testing.T) {
	var buf strings.Builder
	log := connect.NewLogger(&buf)
	if got := activityFile(log, ""); got != "" {
		t.Fatalf("empty: %q", got)
	}
	// An absolute path on every platform (a /run literal is not absolute on
	// Windows), with a ".." the result must have cleaned away.
	dir := t.TempDir()
	sep := string(filepath.Separator)
	if got := activityFile(log, dir+sep+"archivist"+sep+".."+sep+"archivist"+sep+"activity.json"); got != filepath.Join(dir, "archivist", "activity.json") {
		t.Fatalf("absolute: %q", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("warning for a usable value: %s", buf.String())
	}
	if got := activityFile(log, "run/activity.json"); got != "" || !strings.Contains(buf.String(), "not an absolute path") {
		t.Fatalf("relative: %q, log %q", got, buf.String())
	}
}

// ARCHIVIST_SIGNIN_FILE (Story 78.38) follows the same rule: an absolute
// path is used (cleaned), a relative one is ignored with a warning.
func TestSignInFileEnv(t *testing.T) {
	var buf strings.Builder
	log := connect.NewLogger(&buf)
	// An absolute path on every platform, with a ".." the result must have cleaned away.
	dir := t.TempDir()
	sep := string(filepath.Separator)
	if got := runFile(log, signInFileEnv, dir+sep+"run"+sep+".."+sep+"run"+sep+"signin.json"); got != filepath.Join(dir, "run", "signin.json") {
		t.Fatalf("absolute: %q", got)
	}
	if got := runFile(log, signInFileEnv, ""); got != "" || buf.Len() != 0 {
		t.Fatalf("empty: %q, log %q", got, buf.String())
	}
	if got := runFile(log, signInFileEnv, "signin.json"); got != "" || !strings.Contains(buf.String(), signInFileEnv) {
		t.Fatalf("relative: %q, log %q", got, buf.String())
	}
}
