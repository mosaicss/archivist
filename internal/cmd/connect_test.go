package cmd_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mosaicss/archivist/internal/cmd"
)

func TestConnectIsHiddenFromMCP(t *testing.T) {
	root := cmd.NewRootCmd("dev", "unknown", "unknown")
	c, _, err := root.Find([]string{"connect"})
	if err != nil || c.Name() != "connect" {
		t.Fatalf("connect not registered: %v", err)
	}
	if c.Annotations["mcp:hidden"] != "true" || c.Annotations["pp:typed-exit-codes"] == "" {
		t.Fatalf("connect annotations %v", c.Annotations)
	}
}

// Credentials other than ak_ are refused before any network call.
func TestConnectRefusesNonOwnerKeysOffline(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	t.Setenv("ARCHIVIST_RELAY_URL", strings.Replace(srv.URL, "http", "ws", 1))
	for _, tok := range []string{
		"mc_pat_legacy_token_123",
		"mst_0f8fad5b-d9cb-469f-a165-70867728950e.AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-abcde",
	} {
		t.Setenv("ARCHIVIST_TOKEN", tok)
		out, err := runAuthCmd(t, "connect")
		if exitCodeFrom(err) != cmd.ExitAuthError || !strings.Contains(out, "needs an ak_ API key") {
			t.Errorf("%s: exit %d\n%s", tok[:6], exitCodeFrom(err), out)
		}
	}
	t.Setenv("ARCHIVIST_TOKEN", "")
	if out, err := runAuthCmd(t, "connect"); exitCodeFrom(err) != cmd.ExitAuthError {
		t.Errorf("no token: exit %d\n%s", exitCodeFrom(err), out)
	}
	if hits.Load() != 0 {
		t.Fatalf("refused credentials reached the network (%d requests)", hits.Load())
	}
}

func TestConnectValidatesLocalFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, args := range [][]string{
		{"connect", "--claude-effort", "turbo"},
		{"connect", "--claude-model", "--dangerously-skip-permissions"},
		{"connect", "--claude-model", "-x"},
		{"connect", "extra-arg"},
	} {
		_, err := runAuthCmd(t, args...)
		var exitErr *cmd.ExitError
		// Cobra usage errors are plain errors that main maps to exit 2.
		if err == nil || (errors.As(err, &exitErr) && exitErr.Code != cmd.ExitUsageError) {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// stubClaude puts a shell `claude` answering --version and auth status
// (subscription login) first on PATH; nothing else is reachable.
func stubClaude(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n--version) echo '2.1.280 (Claude Code)';;\n" +
		"auth) echo '{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"subscriptionType\":\"max\"}';;\n" +
		"*) exit 2;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
}

// Relay ticket refusals map to typed exits: 401 → 4, FEATURE_DISABLED → 1.
func TestConnectTicketRefusalExitCodes(t *testing.T) {
	stubClaude(t)
	cases := []struct {
		status int
		body   string
		exit   int
		says   string
	}{
		{401, `{"error":"Unauthorized","code":"UNAUTHORIZED"}`, cmd.ExitAuthError, "rejected the credential"},
		{404, `{"error":"The requested endpoint does not exist.","code":"FEATURE_DISABLED"}`, cmd.ExitGenericError, "FEATURE_DISABLED"},
	}
	for _, c := range cases {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if r.Method != "POST" || r.URL.Path != "/relay-tickets" {
				w.WriteHeader(500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		t.Setenv("HOME", t.TempDir())
		t.Setenv("ARCHIVIST_TOKEN", "ak_00000000000000000000")
		t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
		t.Setenv("ARCHIVIST_RELAY_URL", "ws://127.0.0.1:1")
		out, err := runAuthCmd(t, "connect")
		srv.Close()
		if got := exitCodeFrom(err); got != c.exit || !strings.Contains(out, c.says) || hits.Load() == 0 {
			t.Errorf("HTTP %d: exit %d, hits %d\n%s", c.status, got, hits.Load(), out)
		}
	}
}

// A cleartext relay URL to a non-loopback host is refused before any request.
func TestConnectRefusesCleartextRemoteRelay(t *testing.T) {
	stubClaude(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ARCHIVIST_TOKEN", "ak_00000000000000000000")
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	t.Setenv("ARCHIVIST_RELAY_URL", "ws://relay.example.test")
	out, err := runAuthCmd(t, "connect")
	if exitCodeFrom(err) != cmd.ExitUsageError || !strings.Contains(out, "wss://") || hits.Load() != 0 {
		t.Fatalf("exit %d, hits %d\n%s", exitCodeFrom(err), hits.Load(), out)
	}
}

// The relay URL is validated first, --check included (exit 2, nothing run).
func TestConnectCheckRejectsBadRelayURL(t *testing.T) {
	stubClaude(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ARCHIVIST_RELAY_URL", "wss://relay.example.test/?q=1")
	out, err := runAuthCmd(t, "connect", "--check")
	if exitCodeFrom(err) != cmd.ExitUsageError || !strings.Contains(out, "ARCHIVIST_RELAY_URL") || strings.Contains(out, "path:") {
		t.Fatalf("exit %d\n%s", exitCodeFrom(err), out)
	}
}
