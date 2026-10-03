package cmd

import (
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/connect"
)

// connectExit (78.17): 0 when any harness is usable; with no usable
// harness, Claude Code's typed code when Codex is not installed, else 3.
func TestConnectExitAnyUsableHarness(t *testing.T) {
	usableCodex := connect.CodexInfo{Path: "/opt/bin/codex", Version: "0.160.0", VersionOK: true, LoggedIn: true,
		Home: "/h/.codex", AuthPresent: true}
	oldCodex := connect.CodexInfo{Path: "/opt/bin/codex", Version: "0.159.2", Problem: "too old"}
	claudeOK := connect.ClaudeInfo{Path: "/opt/bin/claude", Version: "2.1.280", VersionOK: true}
	claudeAuth := connect.ClaudeInfo{Path: "/opt/bin/claude", Problem: "logged out", ProblemCode: "auth"}
	claudeMissing := connect.ClaudeInfo{Problem: "missing", ProblemCode: "missing"}
	for _, c := range []struct {
		name string
		det  connect.Detection
		exit int
	}{
		{"claude only", connect.Detection{Claude: claudeOK}, 0},
		{"codex only", connect.Detection{Claude: claudeMissing, Codex: usableCodex}, 0},
		{"codex too old, claude usable", connect.Detection{Claude: claudeOK, Codex: oldCodex}, 0},
		{"no codex, claude logged out", connect.Detection{Claude: claudeAuth}, ExitAuthError},
		{"nothing installed", connect.Detection{Claude: claudeMissing}, ExitNotFound},
		{"codex too old, claude logged out", connect.Detection{Claude: claudeAuth, Codex: oldCodex}, ExitNotFound},
	} {
		if got := exitOf(connectExit(c.det)); got != c.exit {
			t.Errorf("%s: exit %d, want %d", c.name, got, c.exit)
		}
	}
}

func TestConnectCheckShowsUsableCodex(t *testing.T) {
	det := connect.Detection{Claude: connect.ClaudeInfo{Problem: "missing", ProblemCode: "missing"},
		Codex: connect.CodexInfo{Path: "/opt/bin/codex", Version: "0.162.1", VersionOK: true, LoggedIn: true,
			Login: "Logged in using ChatGPT", Home: "/h/.codex", AuthPresent: true}}
	var out strings.Builder
	printDetection(&out, det)
	want := "codex\n  path:     /opt/bin/codex\n  version:  0.162.1\n  floor:    0.160.0 (ok)\n  loggedIn: true\n" +
		"  login:    Logged in using ChatGPT\n  home:     /h/.codex (auth.json present)\n  status:   usable\n"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("codex block:\n%s", out.String())
	}
}

func TestHarnessConfigsMapFlagsAndDetection(t *testing.T) {
	det := connect.Detection{
		Claude: connect.ClaudeInfo{Problem: "Claude Code (claude) was not found on PATH", ProblemCode: "missing"},
		Codex: connect.CodexInfo{Path: "/opt/bin/codex", Version: "0.161.0", VersionOK: true, LoggedIn: true,
			Home: "/h/.codex", AuthPresent: true},
	}
	f := connectFlags{model: "claude-sonnet-5", effort: "low", codexModel: "gpt-6-luna", codexEffort: "high"}
	cl, cx := harnessConfigs(det, f, "/opt/bin/archivist", "http://127.0.0.1:9")
	if cl != (connect.ClaudeConfig{}) {
		t.Fatalf("an unusable Claude Code got a config: %+v", cl)
	}
	want := connect.CodexConfig{Bin: "/opt/bin/codex", Version: "0.161.0", Model: "gpt-6-luna", Effort: "high",
		OwnerHome: "/h/.codex", Executable: "/opt/bin/archivist", BaseURL: "http://127.0.0.1:9"}
	if cx != want {
		t.Fatalf("codex config %+v, want %+v", cx, want)
	}
	det.Claude = connect.ClaudeInfo{Path: "/opt/bin/claude", Version: "2.1.280", VersionOK: true}
	det.Codex.Problem = "too old"
	cl, cx = harnessConfigs(det, f, "/a", "")
	if cl.Bin != "/opt/bin/claude" || cl.Model != "claude-sonnet-5" || cl.Effort != "low" || cx != (connect.CodexConfig{}) {
		t.Fatalf("claude %+v codex %+v", cl, cx)
	}
}
