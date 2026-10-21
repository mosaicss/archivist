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

	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/cmd"
	"github.com/zalando/go-keyring"
)

// Story 94.6: connect keeps its key in the credentials file. A normal
// connect start moves a key saved in the keychain (auth login, marker
// present) into the file before it mints its first relay ticket, so before
// any relay dial; session-bound mode leaves the keychain alone.

const keychainConnectKey = "ak_keychainconnect0000000"

// saveKeychainKey saves keychainConnectKey in the in memory keychain the
// way auth login does (item plus marker) and returns the item's account.
func saveKeychainKey(t *testing.T) (home, account string) {
	t.Helper()
	home = t.TempDir()
	setHome(t, home)
	t.Setenv("ARCHIVIST_TOKEN", "")
	auth.MockKeychainForTest(t, false)
	if loc, err := auth.SaveToken(keychainConnectKey); err != nil || !loc.Keychain {
		t.Fatalf("SaveToken: %+v %v", loc, err)
	}
	account, err := filepath.Abs(filepath.Join(home, ".archivist", "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	return home, account
}

func TestConnectStartMovesAKeychainKeyBeforeTheRelay(t *testing.T) {
	stubClaude(t)
	home, account := saveKeychainKey(t)
	path := filepath.Join(home, ".archivist", "credentials")
	var tickets, movedFirst atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/relay-tickets" {
			w.WriteHeader(500)
			return
		}
		tickets.Add(1)
		// The relay dial needs this ticket: the key must be in the file
		// and the keychain item gone by now.
		data, _ := os.ReadFile(path)
		_, gerr := keyring.Get("archivist", account)
		if strings.TrimSpace(string(data)) == keychainConnectKey && errors.Is(gerr, keyring.ErrNotFound) &&
			r.Header.Get("Authorization") == "Bearer "+keychainConnectKey {
			movedFirst.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"Unauthorized","code":"UNAUTHORIZED"}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	t.Setenv("ARCHIVIST_RELAY_URL", "ws://127.0.0.1:1")
	out, err := runAuthCmd(t, "connect")
	if code := exitCodeFrom(err); code != cmd.ExitAuthError || tickets.Load() == 0 {
		t.Fatalf("exit %d, tickets %d\n%s", code, tickets.Load(), out)
	}
	if movedFirst.Load() != tickets.Load() {
		t.Fatalf("a ticket was minted before the key moved to the file\n%s", out)
	}
	if !strings.Contains(out, "the saved key moved to "+path) {
		t.Fatalf("output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".archivist", "keychain")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker kept: %v", err)
	}
}

func TestConnectSessionModeLeavesTheKeychain(t *testing.T) {
	skipShellStubs(t)
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n--version) echo 'codex-cli 0.160.0';;\n" +
		"login) echo 'Not logged in' >&2; exit 1;;\n*) exit 2;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex-home"))
	home, account := saveKeychainKey(t)
	srv, calls := fakeSessionAPI(t, 404, `{"error":"Session not found.","code":"SESSION_NOT_FOUND"}`)
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	t.Setenv("ARCHIVIST_RELAY_URL", "ws://127.0.0.1:1")
	out, err := runAuthCmd(t, "connect", "--session", testSessionID, "--agent", "codex", "--prompt-file", writePrompt(t, "hi"))
	if code := exitCodeFrom(err); code != cmd.ExitGenericError || len(calls()) != 1 {
		t.Fatalf("exit %d, calls %v\n%s", code, calls(), out)
	}
	if got, err := keyring.Get("archivist", account); err != nil || got != keychainConnectKey {
		t.Fatalf("keychain item: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".archivist", "credentials")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a credentials file was written: %v", err)
	}
	if strings.Contains(out, "moved to") {
		t.Fatalf("output:\n%s", out)
	}
}
