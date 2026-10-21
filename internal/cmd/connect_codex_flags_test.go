package cmd_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mosaicss/archivist/internal/cmd"
	"github.com/mosaicss/archivist/internal/connect"
)

func TestConnectValidatesCodexFlags(t *testing.T) {
	setHome(t, t.TempDir())
	for _, args := range [][]string{
		{"connect", "--codex-effort", "turbo"},
		{"connect", "--codex-model", "-x="},
		{"connect", "--codex-model", "a b"},
	} {
		_, err := runAuthCmd(t, args...)
		var exitErr *cmd.ExitError
		if !errors.As(err, &exitErr) || exitErr.Code != cmd.ExitUsageError {
			t.Errorf("%v: %v, want a usage exit (2)", args, err)
		}
	}
}

// Codex-only startup: Claude Code missing, a usable Codex on PATH: the run
// gate passes and the daemon asks chat-api for a relay ticket.
func TestConnectStartsWithCodexOnly(t *testing.T) { runCodexStartup(t, false) }

// An installed but unusable Claude Code next to a usable Codex: startup
// says why Claude Code sessions are unavailable.
func TestConnectWarnsAboutUnusableHarness(t *testing.T) { runCodexStartup(t, true) }

// Story 94.5: on Linux, a logged in Claude Code without bubblewrap or socat
// on PATH: startup names the two packages, and Codex still runs.
func TestConnectWarnsAboutLinuxPackages(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	runCodexStartupWith(t, true, false)
}

func runCodexStartup(t *testing.T, brokenClaude bool) { runCodexStartupWith(t, brokenClaude, true) }

// runCodexStartupWith starts connect with a usable Codex and, when
// claudeInstalled, a Claude Code that is logged out (sandboxTools: bwrap
// and socat on PATH) or, without the sandbox programs, logged in.
func runCodexStartupWith(t *testing.T, claudeInstalled, sandboxTools bool) {
	brokenClaude := claudeInstalled
	skipShellStubs(t)
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n--version) echo 'codex-cli 0.163.0';;\n" +
		"login) echo 'Logged in using ChatGPT' >&2;;\n*) exit 2;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	owner := t.TempDir()
	if err := os.WriteFile(filepath.Join(owner, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if brokenClaude {
		// An installed but logged-out Claude Code: warned about, not driven.
		claude := "#!/bin/sh\ncase \"$1\" in\n--version) echo '2.1.299 (Claude Code)';;\n" +
			"auth) echo '{\"loggedIn\":false}';;\n*) exit 2;;\nesac\n"
		if !sandboxTools {
			claude = strings.Replace(claude, `{\"loggedIn\":false}`,
				`{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"subscriptionType\":\"max\"}`, 1)
		}
		if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(claude), 0o755); err != nil {
			t.Fatal(err)
		}
		if sandboxTools {
			sandboxToolStubs(t, dir)
		}
	}
	t.Setenv("PATH", dir) // no other claude
	t.Setenv("CODEX_HOME", owner)
	var tickets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/relay-tickets" {
			tickets.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"The requested endpoint does not exist.","code":"FEATURE_DISABLED"}`))
	}))
	defer srv.Close()
	setHome(t, t.TempDir())
	t.Setenv("ARCHIVIST_TOKEN", "ak_00000000000000000000")
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	t.Setenv("ARCHIVIST_RELAY_URL", "ws://127.0.0.1:1")
	out, err := runAuthCmd(t, "connect")
	if tickets.Load() == 0 || !strings.Contains(out, "FEATURE_DISABLED") || !strings.Contains(out, "Codex 0.163.0") {
		t.Fatalf("exit %d, tickets %d\n%s", exitCodeFrom(err), tickets.Load(), out)
	}
	if !sandboxTools {
		if !strings.Contains(out, "warning: Claude Code sessions are unavailable: "+connect.ClaudeLinuxPackages) {
			t.Fatalf("no package warning\n%s", out)
		}
		return
	}
	if warned := strings.Contains(out, "warning: Claude Code sessions are unavailable: Claude Code is not logged in"); warned != brokenClaude {
		t.Fatalf("Claude warning %v, want %v\n%s", warned, brokenClaude, out)
	}
}
