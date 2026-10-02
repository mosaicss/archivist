package cmd_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
