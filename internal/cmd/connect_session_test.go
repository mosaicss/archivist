package cmd_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mosaicss/archivist/internal/cmd"
)

const testSessionID = "0f8fad5b-d9cb-469f-a165-70867728950e"

func writePrompt(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Session-bound mode flags (Story 78.22): all three together, a session
// UUID, claude or codex, and a readable prompt within the relay limit.
func TestConnectValidatesSessionFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ok := writePrompt(t, "summarize the 10-K")
	cases := map[string][]string{
		"session only":      {"connect", "--session", testSessionID},
		"no prompt file":    {"connect", "--session", testSessionID, "--agent", "codex"},
		"bad uuid":          {"connect", "--session", "nope", "--agent", "codex", "--prompt-file", ok},
		"bad agent":         {"connect", "--session", testSessionID, "--agent", "gemini", "--prompt-file", ok},
		"missing file":      {"connect", "--session", testSessionID, "--agent", "codex", "--prompt-file", "/nonexistent/p"},
		"empty prompt":      {"connect", "--session", testSessionID, "--agent", "codex", "--prompt-file", writePrompt(t, " \n\t")},
		"prompt too long":   {"connect", "--session", testSessionID, "--agent", "codex", "--prompt-file", writePrompt(t, strings.Repeat("a", 32001))},
		"check and session": {"connect", "--check", "--session", testSessionID, "--agent", "codex", "--prompt-file", ok},
	}
	for name, args := range cases {
		out, err := runAuthCmd(t, args...)
		if code := exitCodeFrom(err); code != cmd.ExitUsageError {
			t.Errorf("%s: exit %d, want 2\n%s", name, code, out)
		}
	}
}

// fakeSessionAPI records chat-api calls and answers GET /agent-sessions/:id
// with status (and the session body when 200).
func fakeSessionAPI(t *testing.T, status int, body string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/agent-sessions/") {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"not found","code":"NOT_FOUND"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

// A logged-out Codex is kept for its sign-in (no "no usable harness" exit);
// the session is checked with chat-api before any socket, and an unknown
// session ends the run with no relay ticket minted.
func TestConnectSessionModeChecksTheSessionFirst(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n--version) echo 'codex-cli 0.160.0';;\n" +
		"login) echo 'Not logged in' >&2; exit 1;;\n*) exit 2;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex-home")) // no auth.json yet
	srv, calls := fakeSessionAPI(t, 404, `{"error":"Session not found.","code":"SESSION_NOT_FOUND"}`)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ARCHIVIST_TOKEN", "ak_00000000000000000000")
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	t.Setenv("ARCHIVIST_RELAY_URL", "ws://127.0.0.1:1")
	out, err := runAuthCmd(t, "connect", "--session", testSessionID, "--agent", "codex", "--prompt-file", writePrompt(t, "hi"))
	if code := exitCodeFrom(err); code != cmd.ExitGenericError {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "logged out: signing in first") || !strings.Contains(out, "does not know this session") {
		t.Fatalf("output:\n%s", out)
	}
	got := calls()
	if len(got) != 1 || got[0] != "GET /agent-sessions/"+testSessionID {
		t.Fatalf("chat-api calls %v, want only the session lookup", got)
	}
}

// The requested harness missing, or logged in with an API key, is refused
// with its typed exit before any network call.
func TestConnectSessionModeRefusesUnusableHarness(t *testing.T) {
	for name, tc := range map[string]struct {
		script string
		code   int
	}{
		"missing":     {"", cmd.ExitNotFound},
		"api key":     {"#!/bin/sh\ncase \"$1\" in\n--version) echo 'codex-cli 0.160.0';;\nlogin) echo 'Logged in using an API key' >&2;;\n*) exit 2;;\nesac\n", cmd.ExitAuthError},
		"below floor": {"#!/bin/sh\ncase \"$1\" in\n--version) echo 'codex-cli 0.100.0';;\nlogin) echo 'Not logged in' >&2; exit 1;;\n*) exit 2;;\nesac\n", cmd.ExitGenericError},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.script != "" {
				if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(tc.script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir)
			t.Setenv("CODEX_HOME", t.TempDir())
			srv, calls := fakeSessionAPI(t, 500, `{}`)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("ARCHIVIST_TOKEN", "ak_00000000000000000000")
			t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
			t.Setenv("ARCHIVIST_RELAY_URL", "ws://127.0.0.1:1")
			out, err := runAuthCmd(t, "connect", "--session", testSessionID, "--agent", "codex", "--prompt-file", writePrompt(t, "hi"))
			if code := exitCodeFrom(err); code != tc.code {
				t.Fatalf("exit %d, want %d\n%s", code, tc.code, out)
			}
			if len(calls()) != 0 {
				t.Fatalf("chat-api called: %v", calls())
			}
		})
	}
}

// --max-permission reaches the daemon (Story 78.32): the log names the
// daemon's own effective ceiling (Daemon.MaxMode), default full_auto in the
// session-bound mode.
func TestConnectMaxPermissionReachesTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		flags []string
		want  string
	}{
		{[]string{"--max-permission", "ask", "--mode", "read_only"}, "permission ceiling ask;"},
		{nil, "permission ceiling full_auto;"},
	} {
		dir := t.TempDir()
		script := "#!/bin/sh\ncase \"$1\" in\n--version) echo 'codex-cli 0.160.0';;\n" +
			"login) echo 'Not logged in' >&2; exit 1;;\n*) exit 2;;\nesac\n"
		if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex-home"))
		srv, _ := fakeSessionAPI(t, 404, `{"error":"Session not found.","code":"SESSION_NOT_FOUND"}`)
		t.Setenv("HOME", t.TempDir())
		t.Setenv("ARCHIVIST_TOKEN", "ak_00000000000000000000")
		t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
		t.Setenv("ARCHIVIST_RELAY_URL", "ws://127.0.0.1:1")
		args := append([]string{"connect", "--session", testSessionID, "--agent", "codex", "--prompt-file", writePrompt(t, "hi")}, tc.flags...)
		out, _ := runAuthCmd(t, args...)
		if !strings.Contains(out, tc.want) {
			t.Fatalf("%v: output lacks %q:\n%s", tc.flags, tc.want, out)
		}
	}
}

// --web-search reaches the daemon (Story 78.33): on by default in the
// session-bound mode (a Mosaic cloud sandbox), off with --web-search=false.
func TestConnectWebSearchReachesTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		flags []string
		want  string
	}{
		{nil, "; web search on;"},
		{[]string{"--web-search=false"}, "; web search off;"},
		{[]string{"--web-search"}, "; web search on;"},
	} {
		dir := t.TempDir()
		script := "#!/bin/sh\ncase \"$1\" in\n--version) echo 'codex-cli 0.160.0';;\n" +
			"login) echo 'Not logged in' >&2; exit 1;;\n*) exit 2;;\nesac\n"
		if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex-home"))
		srv, _ := fakeSessionAPI(t, 404, `{"error":"Session not found.","code":"SESSION_NOT_FOUND"}`)
		t.Setenv("HOME", t.TempDir())
		t.Setenv("ARCHIVIST_TOKEN", "ak_00000000000000000000")
		t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
		t.Setenv("ARCHIVIST_RELAY_URL", "ws://127.0.0.1:1")
		args := append([]string{"connect", "--session", testSessionID, "--agent", "codex", "--prompt-file", writePrompt(t, "hi")}, tc.flags...)
		out, _ := runAuthCmd(t, args...)
		if !strings.Contains(out, tc.want) {
			t.Fatalf("%v: output lacks %q:\n%s", tc.flags, tc.want, out)
		}
	}
}
