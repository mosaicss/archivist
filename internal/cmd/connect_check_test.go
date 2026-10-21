package cmd

import (
	"bytes"
	"context"
	"errors"
	"runtime"
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
		if file == "bwrap" || file == "socat" {
			return "/usr/bin/" + file, nil // Story 94.5: the Linux sandbox programs
		}
		return "", errors.New("not found")
	}
	run := func(_ context.Context, _ []string, _ string, bin string, args ...string) ([]byte, error) {
		switch {
		case strings.HasSuffix(bin, "codex") && len(args) == 2 && args[0] == "login":
			return []byte("Logged in using ChatGPT\n"), nil // exit 0: logged in
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
// loggedIn, authMethod, subscriptionType and the codex block (path, version,
// floor, loggedIn, status), and exits with a typed code.
func TestConnectCheckOutputAndExitCodes(t *testing.T) {
	sub := `{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}`
	cases := []struct {
		name     string
		det      connect.Detection
		exit     int
		contains []string
	}{
		{"usable", detectWith([]string{"claude", "codex"}, "2.1.299 (Claude Code)", sub, "codex-cli 0.163.0"), 0, []string{
			"path:     /opt/bin/claude", "version:  2.1.299", "floor:    2.1.299 (ok)", "loggedIn: true",
			"authMethod: claude.ai", "subscriptionType: max", "status:   usable",
			"path:     /opt/bin/codex", "version:  0.163.0", "floor:    0.163.0 (ok)", "not usable: the Codex home directory is unknown"}},
		{"missing", detectWith(nil, "", "", ""), ExitNotFound, []string{
			"claude\n  path:     not found", "not usable: Claude Code (claude) was not found on PATH",
			"codex\n  path:     not found"}},
		{"below floor", detectWith([]string{"claude"}, "2.1.298 (Claude Code)", sub, ""), ExitGenericError, []string{
			"version:  2.1.298", "floor:    2.1.299 (below floor)", "not usable: Claude Code 2.1.298 is older than the supported floor 2.1.299"}},
		{"not subscription", detectWith([]string{"claude"}, "2.1.303 (Claude Code)", `{"loggedIn":true,"authMethod":"console","subscriptionType":""}`, ""), ExitAuthError, []string{
			"floor:    2.1.299 (ok)", "loggedIn: true", "authMethod: console", "subscriptionType: -",
			"not usable: Claude Code is logged in with \"console\""}},
		{"logged out", detectWith([]string{"claude"}, "2.1.299 (Claude Code)", `{"loggedIn":false}`, ""), ExitAuthError, []string{
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

// Story 94.5: on Linux, --check names the two packages a Claude Code
// session needs when either is missing; Codex is unaffected.
func TestConnectCheckShowsLinuxPackages(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	sub := `{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}`
	look := func(file string) (string, error) {
		if file == "claude" || file == "bwrap" {
			return "/opt/bin/" + file, nil
		}
		return "", errors.New("not found")
	}
	run := func(_ context.Context, _ []string, _ string, _ string, args ...string) ([]byte, error) {
		if len(args) == 1 {
			return []byte("2.1.306 (Claude Code)"), nil
		}
		return []byte(sub), nil
	}
	det := connect.Detect(context.Background(), look, run, nil, "/")
	var out bytes.Buffer
	printDetection(&out, det)
	want := "status:   not usable: Claude Code needs two Linux packages, bubblewrap and socat. Install them, for example: sudo apt install bubblewrap socat"
	if !strings.Contains(out.String(), want) || det.Claude.Usable() {
		t.Fatalf("--check:\n%s", out.String())
	}
	if got := exitOf(connectExit(det)); got != ExitGenericError {
		t.Fatalf("exit %d", got)
	}
}

func TestConnectCheckShowsCodexLogin(t *testing.T) {
	sub := `{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}`
	det := detectWith([]string{"claude", "codex"}, "2.1.299 (Claude Code)", sub, "codex-cli 0.163.0")
	var out bytes.Buffer
	printDetection(&out, det)
	if !strings.Contains(out.String(), "codex\n  path:     /opt/bin/codex\n  version:  0.163.0\n  floor:    0.163.0 (ok)\n  loggedIn: true\n  login:    Logged in using ChatGPT\n  status:   not usable") {
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
