package cmd

import (
	"io"
	"testing"

	"github.com/mosaicss/archivist/internal/connect"
)

// sessionHarness keeps a logged-out harness for its sign-in and refuses a
// missing one or a login of the wrong kind; sessionConfigs configures only
// the requested harness, Bin included when it is logged out.
func TestSessionHarnessAndConfigs(t *testing.T) {
	loggedOut := `{"loggedIn":false,"authMethod":"none"}`
	sub := `{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}`
	cases := []struct {
		name, agent string
		det         connect.Detection
		signIn      string
		exit        int
	}{
		{"claude logged out", "claude", detectWith([]string{"claude"}, "2.1.285", loggedOut, ""), "claude", 0},
		{"claude usable", "claude", detectWith([]string{"claude"}, "2.1.285", sub, ""), "", 0},
		{"claude api key", "claude", detectWith([]string{"claude"}, "2.1.285", `{"loggedIn":true,"authMethod":"api_key"}`, ""), "", ExitAuthError},
		{"claude missing", "claude", detectWith(nil, "", "", ""), "", ExitNotFound},
		{"claude below floor", "claude", detectWith([]string{"claude"}, "2.0.1", loggedOut, ""), "", ExitGenericError},
		{"codex logged out", "codex", connect.Detection{Codex: connect.CodexInfo{Path: "/opt/codex", Version: "0.160.0",
			VersionOK: true, Home: "/h/.codex", Problem: "Codex is not logged in"}}, "codex", 0},
		{"codex no auth.json", "codex", connect.Detection{Codex: connect.CodexInfo{Path: "/opt/codex", Version: "0.160.0",
			VersionOK: true, LoggedIn: true, Home: "/h/.codex", Problem: "no auth.json"}}, "codex", 0},
		{"codex api key", "codex", connect.Detection{Codex: connect.CodexInfo{Path: "/opt/codex", Version: "0.160.0",
			VersionOK: true, APILogin: true, Home: "/h/.codex", Problem: "api key"}}, "", ExitAuthError},
		{"codex missing", "codex", connect.Detection{}, "", ExitNotFound},
	}
	for _, c := range cases {
		signIn, err := sessionHarness(io.Discard, c.det, c.agent)
		if signIn != c.signIn || exitOf(err) != c.exit {
			t.Errorf("%s: signIn %q exit %d, want %q %d", c.name, signIn, exitOf(err), c.signIn, c.exit)
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
