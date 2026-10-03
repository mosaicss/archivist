package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/connect"
)

// detectWith runs the real connect.Detect against injected binaries.
func detectWith(found []string, claudeVersion, authJSON, codexVersion string) connect.Detection {
	look := func(file string) (string, error) {
		for _, f := range found {
			if f == file {
				return "/opt/bin/" + file, nil
			}
		}
		return "", errors.New("not found")
	}
	run := func(_ context.Context, _ []string, _ string, bin string, args ...string) ([]byte, error) {
		switch {
		case strings.HasSuffix(bin, "codex") && len(args) == 2 && args[0] == "login":
			return nil, nil // exit 0: logged in
		case strings.HasSuffix(bin, "codex"):
			return []byte(codexVersion), nil
		case len(args) == 1 && args[0] == "--version":
			return []byte(claudeVersion), nil
		case len(args) == 3 && args[0] == "auth":
			return []byte(authJSON), nil
		}
		return nil, errors.New("unexpected command")
	}
	return connect.Detect(context.Background(), look, run, nil, "/")
}

func exitOf(err error) int {
	var e *ExitError
	if errors.As(err, &e) {
		return e.Code
	}
	if err != nil {
		return -1
	}
	return 0
}

// Check row: `archivist connect --check` prints path, version, floor verdict,
// loggedIn, authMethod, subscriptionType and codex "adapter: not supported
// yet", and exits with a typed code.
func TestConnectCheckOutputAndExitCodes(t *testing.T) {
	sub := `{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}`
	cases := []struct {
		name     string
		det      connect.Detection
		exit     int
		contains []string
	}{
		{"usable", detectWith([]string{"claude", "codex"}, "2.1.280 (Claude Code)", sub, "codex-cli 0.160.0"), 0, []string{
			"path:     /opt/bin/claude", "version:  2.1.280", "floor:    2.1.280 (ok)", "loggedIn: true",
			"authMethod: claude.ai", "subscriptionType: max", "status:   usable",
			"path:     /opt/bin/codex", "version:  0.160.0", "adapter:  not supported yet"}},
		{"missing", detectWith(nil, "", "", ""), ExitNotFound, []string{
			"claude\n  path:     not found", "not usable: Claude Code (claude) was not found on PATH",
			"codex\n  path:     not found", "adapter:  not supported yet"}},
		{"below floor", detectWith([]string{"claude"}, "2.1.279 (Claude Code)", sub, ""), ExitGenericError, []string{
			"version:  2.1.279", "floor:    2.1.280 (below floor)", "not usable: Claude Code 2.1.279 is older than the supported floor 2.1.280"}},
		{"not subscription", detectWith([]string{"claude"}, "2.1.288 (Claude Code)", `{"loggedIn":true,"authMethod":"console","subscriptionType":""}`, ""), ExitAuthError, []string{
			"floor:    2.1.280 (ok)", "loggedIn: true", "authMethod: console", "subscriptionType: -",
			"not usable: Claude Code is logged in with \"console\""}},
		{"logged out", detectWith([]string{"claude"}, "2.1.280 (Claude Code)", `{"loggedIn":false}`, ""), ExitAuthError, []string{
			"loggedIn: false", "authMethod: -", "not usable: Claude Code is not logged in"}},
	}
	for _, c := range cases {
		var out bytes.Buffer
		printDetection(&out, c.det)
		for _, want := range c.contains {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s: output lacks %q:\n%s", c.name, want, out.String())
			}
		}
		if got := exitOf(claudeExit(c.det.Claude)); got != c.exit {
			t.Errorf("%s: exit %d, want %d", c.name, got, c.exit)
		}
	}
}

func TestConnectCheckShowsCodexLogin(t *testing.T) {
	sub := `{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}`
	det := detectWith([]string{"claude", "codex"}, "2.1.280 (Claude Code)", sub, "codex-cli 0.160.0")
	var out bytes.Buffer
	printDetection(&out, det)
	if !strings.Contains(out.String(), "codex\n  path:     /opt/bin/codex\n  version:  0.160.0\n  loggedIn: true\n  adapter:  not supported yet") {
		t.Fatalf("codex block:\n%s", out.String())
	}
}

func TestRelayURLFromEnv(t *testing.T) {
	ok := map[string]string{
		"":                           connect.DefaultRelayURL,
		"wss://relay.example.test":   "wss://relay.example.test",
		"https://relay.example.test": "https://relay.example.test",
		"ws://127.0.0.1:8787":        "ws://127.0.0.1:8787",
		"http://localhost:8787":      "http://localhost:8787",
		"ws://[::1]:8787":            "ws://[::1]:8787",
	}
	for in, want := range ok {
		if got, err := relayURLFromEnv(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"ws://relay.example.test", "http://10.0.0.5:8787", "ws://localhost.example.test", "ftp://x", "relay.example.test", "ws://",
		"wss://relay.example.test?x=1", "wss://relay.example.test?", "wss://relay.example.test#frag", "wss://relay.example.test#",
		"wss://user:pass@relay.example.test", "wss://user@relay.example.test", "wss://:443", "ws://:8787"} {
		if _, err := relayURLFromEnv(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}
